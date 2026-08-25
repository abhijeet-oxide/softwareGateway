package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// `promote` at the top level, beside `download`.
//
// It is the same command as `transfers promote` and shares its implementation
// exactly - see runCopy. It exists at the top level for the reason `download`
// does: these two are what an operator actually DOES, and burying the second
// of them one level down under a noun made it something people found by
// reading `--help` rather than by expecting it.
//
// The extra it earns is the pre-flight. `promote P v2.14.0` with no
// destination asks the Coordinator where this release can go before sending
// anything, so the answer to "which target did you mean" is a list of real
// targets with real reasons rather than a resolution error.

func newPromoteCommand() *cobra.Command {
	var spec copySpec

	cmd := &cobra.Command{
		Short: "Promote a release from one of your targets to another",
		Long: "Promotion moves between YOUR targets - lab to production - rather\n" +
			"than from a vendor. The SOURCE is where the release already is: the\n" +
			"product's promotion path when it names one target, and otherwise\n" +
			"the one that actually holds it.\n\n" +
			"Where both targets are repositories of one JFrog, the registry\n" +
			"relocates the release itself: no bytes cross the wire and a 45 GB\n" +
			"release lands in seconds. Where they are not, it is copied - always\n" +
			"correct, and within one registry still mounted rather than moved.\n\n" +
			"  promote P v2.14.0                    where the promotion path says\n" +
			"  promote P v2.14.0 --to production    a named target\n" +
			"  promote P v2.14.0 --dry-run          what would happen, and how\n\n" +
			"With no --to and nothing to deduce, it lists the destinations and\n" +
			"says what promoting into each would do, rather than guessing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(spec.to) > 0 && spec.toEnvironment != "" {
				return usageError{msg: "--to and --to-environment name destinations " +
					"two different ways; use one"}
			}
			if len(spec.to) == 0 && spec.toEnvironment == "" {
				// Nothing named. Ask where it COULD go before asking to send
				// it: a resolution error names candidates, and this names
				// candidates plus what each would cost.
				done, err := offerDestinations(cmd, args[0], args[1], &spec)
				if err != nil || done {
					return err
				}
			}
			return runCopy(cmd, spec.request(args[0], args[1], true), spec.dryRun)
		},
	}

	takes(cmd, "promote", productArg(), packageArg())
	spec.bind(cmd,
		"the target to promote FROM (default: where the release already is)",
		"a target to promote to (repeatable)")
	return cmd
}

// offerDestinations resolves an unnamed destination, or explains the choice.
//
// done is true when the command has said its piece and must not go on to
// create anything. That happens when the Coordinator will not resolve a
// destination on its own - several are possible, or none is - because sending
// the request anyway would produce the same refusal with less in it.
//
// A Coordinator too old to answer, or one that fails for any reason, is NOT an
// error: the request path resolves destinations perfectly well by itself and
// this is a courtesy on top of it. Failing here would make an unavailable
// helper break the command it was helping.
func offerDestinations(cmd *cobra.Command, product, pkg string, spec *copySpec) (bool, error) {
	options, err := newClient().PromotionOptions(cmd.Context(), product, pkg)
	if err != nil {
		return false, nil
	}

	if spec.from == "" && options.DefaultOrigin != "" {
		// Said explicitly rather than left implicit, so the resolution the
		// dry run reports is the one this invocation asked for.
		spec.from = options.DefaultOrigin
	}
	if len(options.DefaultDestinations) == 1 {
		spec.to = options.DefaultDestinations
		return false, nil
	}
	if !options.Promotable {
		return true, fmt.Errorf("%s cannot be promoted: %s", pkg, orDefault(options.Reason,
			"there is no destination to promote it into"))
	}

	return true, render(stdout(), globalOutput(), options, func(w io.Writer) error {
		return renderDestinations(w, product, pkg, options)
	})
}

// globalOutput is the -o flag. Named rather than read inline because `opts` is
// shadowed here by the promotion options themselves, and a silent shadow of a
// package-level variable is exactly how -o json stops working.
func globalOutput() string { return opts.output }

// renderDestinations says where a release can go and what sending it there does.
func renderDestinations(w io.Writer, product, pkg string, o *v1.PromotionOptionsResponse) error {
	fmt.Fprintf(w, "%s is at %s.\n\n", pkg, orDefault(o.DefaultOrigin, "several targets"))

	if o.DefaultOrigin == "" {
		fmt.Fprintln(w, "Several hold it, so name the origin:")
		for _, origin := range o.Origins {
			if origin.Holds {
				fmt.Fprintf(w, "  --from %s\n", origin.Name)
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Where it can go:")
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  TARGET\tENVIRONMENT\tSTATE\tHOW")
	for _, d := range sortedDestinations(o.Destinations) {
		if d.Unavailable != "" {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			d.Name, orDefault(d.Environment, "-"),
			destinationState(d), promotionHow(d))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	// The reasons, once each, below the table. In the table they would be a
	// paragraph per row and the table would stop being one.
	for _, reason := range distinctReasons(o.Destinations) {
		fmt.Fprintf(w, "  %s\n", reason)
	}
	if !o.Analysed {
		fmt.Fprintf(w, "\n  transferctl packages describe %s %s --expand   allows relocation\n",
			product, pkg)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Name one: transferctl promote %s %s --to <target>\n", product, pkg)
	return nil
}

func sortedDestinations(in []v1.PromotionDestination) []v1.PromotionDestination {
	out := append([]v1.PromotionDestination(nil), in...)
	// Relocatable first, because that is the one somebody wants to notice, and
	// alphabetically within it so the list does not reorder between runs.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Method == v1.PromotionRelocate) != (out[j].Method == v1.PromotionRelocate) {
			return out[i].Method == v1.PromotionRelocate
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func destinationState(d v1.PromotionDestination) string {
	switch d.State {
	case "PRESENT":
		return "already there"
	case "IN_FLIGHT":
		return "in progress"
	default:
		return "not there"
	}
}

func promotionHow(d v1.PromotionDestination) string {
	if d.Method == v1.PromotionRelocate {
		return "relocate"
	}
	return "copy"
}

// distinctReasons is each explanation once.
//
// Deduplicated because four production targets on one wrong host produce four
// identical sentences, and a reader who has read it once should not have to
// check whether the second one says something different.
func distinctReasons(in []v1.PromotionDestination) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		if d.Unavailable != "" || d.MethodReason == "" || seen[d.MethodReason] {
			continue
		}
		seen[d.MethodReason] = true
		out = append(out, strings.TrimSpace(d.MethodReason))
	}
	return out
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
