package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Two noun groups, because there are two things.
//
//	downloads   WHAT happens: where software goes and what gates it passes
//	rules       WHEN it happens by itself: a pattern, and which download it fires
//
// Both are read-only. They are configuration, and configuration comes from Git
// — there is deliberately no `enable`, no `disable` and no runtime override,
// because a second source of truth for "is this on" is exactly what GitOps
// exists to prevent.
func newDownloadsCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "downloads",
		Aliases: []string{"download-config"},
		Short:   "How software moves from a source into internal targets",
		Long: "A download says where software goes and what has to pass before it\n" +
			"gets there. It holds no tag pattern: by the time one runs, somebody\n" +
			"has already chosen the software — either by typing it, or by an\n" +
			"auto-download rule matching it.\n\n" +
			"The ORDER of its targets is not in the download. It comes from what\n" +
			"the targets themselves say, so naming the end of a chain names the\n" +
			"chain.",
	})
	cmd.AddCommand(newDownloadsListCommand())
	return cmd
}

func newDownloadsListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Downloads, their chains and their gates",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListDownloads(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderDownloadList(w, resp)
			})
		},
	}
	takes(cmd, "list", productArg())
	return cmd
}

func renderDownloadList(w io.Writer, resp *v1.ListDownloadsResponse) error {
	if len(resp.Downloads) == 0 {
		fmt.Fprintln(w, "No downloads configured, and the product has no default target.")
		fmt.Fprintln(w, "Nothing can be downloaded until one of the two exists.")
		return nil
	}

	for i, d := range resp.Downloads {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", downloadTitle(d))

		if d.ChainError != "" {
			fmt.Fprintf(w, "  chain      ! %s\n", d.ChainError)
		} else {
			fmt.Fprintf(w, "  chain      %s\n", d.ChainText)
			// The chain in full, always: a download naming one target that
			// plans three steps has to say so somewhere, and this is it.
			if len(d.Chain) > len(d.Targets) {
				fmt.Fprintf(w, "             (names %s; the rest is pulled in by what those targets mirror from)\n",
					strings.Join(d.Targets, ", "))
			}
		}
		fmt.Fprintf(w, "  verify     before %s, at destination %s%s\n",
			d.VerifyBefore, d.VerifyAfter, policySuffix(d.VerifyPolicy))
		fmt.Fprintf(w, "  priority   %d\n", d.Priority)

		if d.VerifyAfter == "true" && d.VerifyPolicy == "enforce" {
			fmt.Fprintln(w, "\n  Under `enforce`, a destination that fails verification stops every step")
			fmt.Fprintln(w, "  that depends on it. Nothing downstream is configured or written.")
		}
	}
	return nil
}

func downloadTitle(d v1.DownloadView) string {
	switch {
	case d.Name == "":
		return "the download"
	case d.Default:
		return d.Name + "  (default)"
	default:
		return d.Name
	}
}

func policySuffix(policy string) string {
	if policy == "" {
		return ""
	}
	return " (" + policy + ")"
}

// `rules` is what fires a download when nobody is asking. A pattern, and
// nothing else about the work.
func newRulesCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "rules",
		Aliases: []string{"rule", "auto-download"},
		Short:   "What gets downloaded automatically",
		Long: "An auto-download rule holds a tag pattern and the name of the\n" +
			"download it triggers. It does not do the downloading: it fires the\n" +
			"same operation a person performs by hand.\n\n" +
			"Rules live in Git. `enabled: false` is the only way to turn one off,\n" +
			"deliberately — a runtime override would be a second source of truth\n" +
			"for whether a rule is on.",
	})
	cmd.AddCommand(newRulesListCommand(), newRulesMatchesCommand())
	return cmd
}

func newRulesListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Rules, what they match, and what they trigger",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListAutoDownloadRules(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderRuleList(w, resp)
			})
		},
	}
	takes(cmd, "list", productArg())
	return cmd
}

func renderRuleList(w io.Writer, resp *v1.ListAutoDownloadRulesResponse) error {
	if len(resp.Rules) == 0 {
		fmt.Fprintln(w, "No auto-download rules. Nothing downloads without being asked.")
		return nil
	}

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "RULE\tMATCHES\tFROM\tTRIGGERS\tCHAIN\tSTATE")
	for _, r := range resp.Rules {
		chain := r.ChainText
		if r.ChainError != "" {
			chain = "! " + r.ChainError
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.TagPattern, orAll(r.Sources), triggersWhat(r), chain, ruleState(r))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// The master switch is a fact about the whole block, and a reader looking
	// at three active-looking rules while nothing fires deserves to be told.
	if !resp.Enabled {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "autoDownload.enabled is false: none of these fire on their own.")
		fmt.Fprintln(w, "Downloading by hand is unaffected.")
	}
	return nil
}

func orAll(sources []string) string {
	if len(sources) == 0 {
		return "every source"
	}
	return strings.Join(sources, ",")
}

func triggersWhat(r v1.AutoDownloadRuleView) string {
	switch {
	case r.Download != "":
		return r.Download
	case r.Inline:
		// The older spelling, where the rule carried its own targets. Named
		// rather than hidden, because it is why this rule's chain can differ
		// from every other rule's.
		return "(its own targets)"
	default:
		return "the default download"
	}
}

func ruleState(r v1.AutoDownloadRuleView) string {
	switch {
	case r.ChainError != "":
		return "BROKEN"
	case !r.Enabled:
		return "disabled"
	default:
		return "active"
	}
}

func newRulesMatchesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "What a rule would pick up from what has been discovered",
		Long: "A question about the RULE, not about downloading. It reads and\n" +
			"creates nothing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().RuleMatches(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				if len(resp.Matches) == 0 {
					fmt.Fprintf(w, "%s matches nothing that has been discovered yet.\n", resp.Rule)
					return nil
				}
				fmt.Fprintf(w, "%s matches %s:\n\n", resp.Rule,
					plural(len(resp.Matches), "package", "packages"))
				for _, m := range resp.Matches {
					fmt.Fprintf(w, "  %s\n", m)
				}
				return nil
			})
		},
	}
	takes(cmd, "matches", productArg(), ruleArg())
	return cmd
}

func ruleArg() argSpec {
	return argSpec{
		Name: "rule",
		Help: "a configured auto-download rule in that product",
		Find: "transferctl rules list <product>",
	}
}
