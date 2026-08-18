package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// `rules` is the noun; run, suspend and resume are the verbs that act on one.
//
// `suspend`/`resume` rather than `enable`/`disable`, and the difference is the
// point rather than a naming preference: `enabled` lives in Git, and this does
// not touch it. A CLI that spelled both the same would hide the one fact a
// reader most needs — that configuration still says what it said, and an
// override is sitting on top of it. See docs/design/20 §9.
func newRulesCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "rules",
		Aliases: []string{"rule"},
		Short:   "Inspect and operate download rules",
		Long: "A download rule says what to bring in, from where, to where, and\n" +
			"when. The ORDER of its destinations is not in the rule: it comes\n" +
			"from the targets' own configuration, so naming the end of a chain\n" +
			"names the chain.\n\n" +
			"Rules live in Git. Running one, and stopping one, are operations;\n" +
			"changing what a rule says is a commit.",
	})
	cmd.AddCommand(
		newRulesListCommand(),
		newRulesDescribeCommand(),
		newRulesRunCommand(),
		newRulesSuspendCommand(),
		newRulesResumeCommand(),
	)
	return cmd
}

func newRulesListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Rules, their chains, and what is suspended",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListDownloadRules(cmd.Context(), args[0])
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

func renderRuleList(w io.Writer, resp *v1.ListDownloadRulesResponse) error {
	if len(resp.Rules) == 0 {
		fmt.Fprintln(w, "No download rules configured.")
		return nil
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "RULE\tMATCHES\tCHAIN\tTRIGGER\tVERIFY\tSTATE")
	suspended := 0
	for _, r := range resp.Rules {
		if r.Suspended {
			suspended++
		}
		chain := r.ChainText
		if r.ChainError != "" {
			chain = "! " + r.ChainError
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.TagPattern, chain,
			strings.Join(r.Trigger, ","), verifySummary(r), ruleState(r))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if suspended > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s suspended. That is an operational override, not a configuration change —\n",
			plural(suspended, "rule is", "rules are"))
		fmt.Fprintln(w, "Git still says what it said. `rules describe` shows who stopped it and why.")
	}
	return nil
}

// ruleState reports the thing a reader has to act on, not the flag they asked
// about. Configured-off and suspended are different situations with different
// fixes, and both differ from a rule that is simply working.
func ruleState(r v1.DownloadRuleView) string {
	switch {
	case r.ChainError != "":
		return "BROKEN"
	case r.Suspended && !r.Enabled:
		return "suspended (also disabled in Git)"
	case r.Suspended:
		return "SUSPENDED"
	case !r.Enabled:
		return "disabled in Git"
	default:
		return "active"
	}
}

func verifySummary(r v1.DownloadRuleView) string {
	parts := []string{}
	if r.VerifyBefore == "true" {
		parts = append(parts, "before")
	}
	if r.VerifyAfter == "true" {
		parts = append(parts, "after")
	}
	if len(parts) == 0 {
		return "inherit"
	}
	out := strings.Join(parts, "+")
	if r.VerifyPolicy != "" {
		out += " (" + r.VerifyPolicy + ")"
	}
	return out
}

func newRulesDescribeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "The derived step order and the gates a rule applies",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().GetDownloadRule(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderRuleDetail(w, resp)
			})
		},
	}
	takes(cmd, "describe", productArg(), ruleArg())
	return cmd
}

func renderRuleDetail(w io.Writer, r *v1.DownloadRuleView) error {
	fmt.Fprintf(w, "%s / %s\n\n", r.Product, r.Name)

	tw := newTabWriter(w)
	fmt.Fprintf(tw, "  Matches:\t%s\n", r.TagPattern)
	if len(r.Sources) > 0 {
		fmt.Fprintf(tw, "  From:\t%s\n", strings.Join(r.Sources, ", "))
	}
	fmt.Fprintf(tw, "  Trigger:\t%s\n", strings.Join(r.Trigger, ", "))
	if r.Window != "" {
		fmt.Fprintf(tw, "  Window:\t%s\n", r.Window)
	}
	fmt.Fprintf(tw, "  Priority:\t%d\n", r.Priority)
	if err := tw.Flush(); err != nil {
		return err
	}

	// The chain in full, always. It is implicit to TYPE and never implicit to
	// read: a rule naming one target that plans three steps has to say so
	// somewhere, and this is that somewhere.
	fmt.Fprintln(w, "\n  Chain")
	if r.ChainError != "" {
		fmt.Fprintf(w, "    could not be derived: %s\n", r.ChainError)
	} else {
		fmt.Fprintf(w, "    %s\n", r.ChainText)
		if len(r.Chain) > len(r.Targets) {
			fmt.Fprintf(w, "    (the rule names %s; the rest is pulled in by what those targets mirror from)\n",
				strings.Join(r.Targets, ", "))
		}
	}

	fmt.Fprintln(w, "\n  Verification")
	fmt.Fprintf(w, "    before the transfer   %s\n", r.VerifyBefore)
	fmt.Fprintf(w, "    at the destination    %s\n", r.VerifyAfter)
	if r.VerifyPolicy != "" {
		fmt.Fprintf(w, "    policy                %s\n", r.VerifyPolicy)
	}
	if r.VerifyAfter == "true" && r.VerifyPolicy == "enforce" {
		fmt.Fprintln(w, "\n    Under `enforce`, a destination that fails verification stops every")
		fmt.Fprintln(w, "    step that depends on it. Nothing downstream is configured or written.")
	}

	fmt.Fprintln(w, "\n  State")
	fmt.Fprintf(w, "    configuration says    %s\n", enabledWord(r.Enabled))
	if r.Suspended {
		fmt.Fprintf(w, "    SUSPENDED by          %s\n", orUnknown(r.SuspendedBy))
		fmt.Fprintf(w, "    reason                %s\n", r.SuspendedReason)
		if r.SuspendedUntil != "" {
			fmt.Fprintf(w, "    until                 %s\n", r.SuspendedUntil)
		} else {
			fmt.Fprintln(w, "    until                 indefinitely — nothing will lift this on its own")
		}
		fmt.Fprintln(w, "\n    A suspension is an operational override. Configuration was not changed.")
	}
	if r.Runnable {
		fmt.Fprintln(w, "    this rule would act if a matching package appeared")
	}
	return nil
}

func enabledWord(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func orUnknown(s string) string {
	if s == "" {
		return "(unrecorded)"
	}
	return s
}

func newRulesRunCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Short: "Run a rule now",
		Long: "Produces the same request, the same chain and the same gates as a\n" +
			"rule that fired on its own. A manual trigger changes who is recorded\n" +
			"as the actor and nothing else.\n\n" +
			"A named tag still has to match the rule's pattern: running the GA\n" +
			"rule for a release candidate is refused rather than obeyed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().RunDownloadRule(cmd.Context(), args[0], args[1],
				v1.RunDownloadRuleRequest{Tags: args[2:], ValidateOnly: dryRun})
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderRuleRun(w, resp)
			})
		},
	}
	takes(cmd, "run", productArg(), ruleArg(), tagsArg())
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would run and create nothing")
	return cmd
}

func renderRuleRun(w io.Writer, r *v1.RunDownloadRuleResponse) error {
	fmt.Fprintf(w, "%s / %s\n", r.Product, r.Rule)
	fmt.Fprintf(w, "  chain   %s\n\n", strings.Join(r.Chain, " → "))

	if len(r.Matched) == 0 {
		fmt.Fprintln(w, "Nothing matched. No packages this rule selects have been discovered.")
		return nil
	}
	fmt.Fprintf(w, "  %s: %s\n", plural(len(r.Matched), "match", "matches"),
		strings.Join(r.Matched, ", "))

	switch {
	case r.ValidateOnly:
		fmt.Fprintln(w, "\nDry run: nothing was created.")
	case len(r.Created) > 0:
		fmt.Fprintf(w, "\nOpened %s.\n", plural(len(r.Created), "run", "runs"))
		for _, id := range r.Created {
			fmt.Fprintf(w, "  %s\n", id)
		}
	}
	if n := len(r.AlreadyRequested); n > 0 {
		// Not a failure. Asking twice for the same work is the normal result of
		// a re-run, and idempotency is what makes it safe.
		fmt.Fprintf(w, "\n%s already requested: %s\n",
			plural(n, "match was", "matches were"), strings.Join(r.AlreadyRequested, ", "))
	}
	return nil
}

func newRulesSuspendCommand() *cobra.Command {
	var (
		reason string
		until  time.Duration
	)
	cmd := &cobra.Command{
		Short: "Stop a rule now, without editing Git",
		Long: "An operational override for when a rule has to stop in ten seconds:\n" +
			"a vendor pushing broken images at 02:00 is not the moment to open a\n" +
			"pull request and wait for Flux.\n\n" +
			"It does NOT change configuration. Git still says what it said, and\n" +
			"both facts are reported side by side wherever the rule appears.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if reason == "" {
				return usageError{msg: "transferctl rules suspend needs --reason.\n\n" +
					"  A suspension nobody can explain is one nobody dares lift, and an\n" +
					"  unexplained override is how a rule stays off for a quarter."}
			}
			req := v1.SuspendDownloadRuleRequest{Reason: reason}
			if until > 0 {
				req.Until = time.Now().UTC().Add(until).Format(time.RFC3339)
			}
			resp, err := newClient().SuspendDownloadRule(cmd.Context(), args[0], args[1], req)
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				fmt.Fprintf(w, "Suspended %s/%s.\n", resp.Product, resp.Rule)
				fmt.Fprintf(w, "  reason  %s\n", resp.Reason)
				if resp.Until != "" {
					fmt.Fprintf(w, "  until   %s\n", resp.Until)
				} else {
					fmt.Fprintln(w, "  until   indefinitely — nothing will lift this on its own")
				}
				fmt.Fprintf(w, "\n%s\n", resp.Note)
				return nil
			})
		},
	}
	takes(cmd, "suspend", productArg(), ruleArg())
	cmd.Flags().StringVar(&reason, "reason", "", "why (required)")
	cmd.Flags().DurationVar(&until, "until", 0, "lift automatically after this long; omit for indefinite")
	return cmd
}

func newRulesResumeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Lift a suspension",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ResumeDownloadRule(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				fmt.Fprintf(w, "%s/%s: %s\n", resp.Product, resp.Rule, resp.Note)
				return nil
			})
		},
	}
	takes(cmd, "resume", productArg(), ruleArg())
	return cmd
}

func ruleArg() argSpec {
	return argSpec{
		Name: "rule",
		Help: "a configured download rule in that product",
		Find: "transferctl rules list <product>",
	}
}

func tagsArg() argSpec {
	return argSpec{
		Name:     "tags",
		Help:     "one or more tags to run; each still has to match the rule's pattern",
		Optional: true,
		Default:  "every discovered package the rule matches",
	}
}
