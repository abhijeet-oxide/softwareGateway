package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// `targets` follows the rule the command tree already follows: things you LOOK
// AT are nouns, things the tool DOES are verbs. `apply` and `sync` live under
// the noun because they act on one target rather than on the estate.
//
// `apply` is a command rather than something a config reload does, and that is
// the load-bearing decision here. Applying a mirror flips the destination
// repository into MIRROR state, which makes it read-only to everyone but a
// robot and converges its content to a tag glob - deleting whatever does not
// match. A `kubectl apply` of a ConfigMap with a typo in `tags` must not be
// able to empty a production repository. See docs/design/18 section 8.
func newTargetsCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "targets",
		Aliases: []string{"target"},
		Short:   "Inspect and reconcile how content gets into a target",
		Long: "A target says HOW content reaches it: `copy` (our workers push),\n" +
			"`mirror` (Quay pulls on its own schedule) or `proxy` (a Quay\n" +
			"organization fills when a pod pulls).\n\n" +
			"Configuration lives in Git. Applying it to the registry is a\n" +
			"separate, deliberate command, because the write is destructive.",
	})
	cmd.AddCommand(
		newTargetsListCommand(),
		newTargetsDescribeCommand(),
		newTargetsApplyCommand(),
		newTargetsSyncCommand(),
		newTargetsDriftCommand(),
	)
	return cmd
}

func newTargetsListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Targets and how content gets into each",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListReplication(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderTargetList(w, resp)
			})
		},
	}
	takes(cmd, "list", productArg())
	return cmd
}

func renderTargetList(w io.Writer, resp *v1.ListReplicationResponse) error {
	if len(resp.Targets) == 0 {
		fmt.Fprintln(w, "No targets configured.")
		return nil
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "TARGET\tMODE\tUPSTREAM\tINTERVAL\tSTATE\tLAST SYNC")
	for _, t := range resp.Targets {
		up, interval := "-", "-"
		if t.Desired != nil {
			if t.Desired.Upstream != "" {
				up = t.Desired.Upstream
			}
			if t.Desired.Interval != "" {
				interval = t.Desired.Interval
			}
		}
		sync := "-"
		if t.Observed != nil && t.Observed.SyncStatus != "" {
			sync = t.Observed.SyncStatus
			// The registry's own word, when we did not recognise it. Reporting
			// "UNKNOWN" alone would hide the one piece of information that
			// would let somebody look it up.
			if t.Observed.SyncStatus == "UNKNOWN" && t.Observed.SyncStatusRaw != "" {
				sync = "UNKNOWN (" + t.Observed.SyncStatusRaw + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Target, t.Mode, up, interval, replicationState(t), sync)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// The byte columns a reader expects are absent on purpose, so say why once
	// rather than leaving them to wonder.
	if hasDelegated(resp.Targets) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "A delegated target reports a STATE, not a speed: the registry moves those")
		fmt.Fprintln(w, "bytes and does not tell us how many. Nothing here is derived from elapsed time.")
	}
	return nil
}

func hasDelegated(targets []v1.ReplicationView) bool {
	for _, t := range targets {
		if t.Mode != "copy" {
			return true
		}
	}
	return false
}

// replicationState collapses the several things that can be true into the one
// an operator has to act on, worst first.
func replicationState(t v1.ReplicationView) string {
	switch {
	case t.Mode == "copy":
		return "n/a"
	case t.Unreachable != "":
		return "unreachable"
	case t.Drift != nil && t.Drift.Absent:
		return "not applied"
	case t.Drift != nil && t.Drift.Detected:
		return "DRIFTED"
	case t.PendingApply:
		return "pending apply"
	default:
		return "in sync"
	}
}

func newTargetsDescribeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Everything known about one target's replication",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().GetReplication(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderTargetDetail(w, resp)
			})
		},
	}
	takes(cmd, "describe", productArg(), targetArg())
	return cmd
}

func renderTargetDetail(w io.Writer, t *v1.ReplicationView) error {
	fmt.Fprintf(w, "%s / %s\n", t.Product, t.Target)
	fmt.Fprintf(w, "  mode              %s\n", t.Mode)
	if t.Mode == "copy" {
		fmt.Fprintln(w, "\n  Our own workers pull from the origin and push here. There is no")
		fmt.Fprintln(w, "  registry-side configuration, and nothing to apply or sync.")
		return nil
	}

	if d := t.Desired; d != nil {
		fmt.Fprintln(w, "\n  CONFIGURED (from Git)")
		printIf(w, "upstream", d.Upstream)
		printIf(w, "organization", d.Organization)
		// The tag set is printed in full and never truncated. This is the
		// third defence against the signature trap: a glob list that cannot
		// match `sha256-*.sig` is visible here rather than inferred.
		if len(d.Tags) > 0 {
			fmt.Fprintf(w, "    %-16s %s\n", "tags", strings.Join(d.Tags, ", "))
			if !selectsSignatures(d.Tags) {
				fmt.Fprintln(w, "                     ↑ no glob matches a cosign signature tag (sha256-*.sig).")
				fmt.Fprintln(w, "                       The images will mirror and the signatures will not.")
			}
		}
		printIf(w, "interval", d.Interval)
		printIf(w, "expiration", d.Expiration)
		printIf(w, "robot", d.Robot)
	}

	fmt.Fprintln(w, "\n  APPLIED (what we last wrote)")
	if t.Applied == nil {
		fmt.Fprintln(w, "    never applied - run `transferctl targets apply` to write it")
	} else {
		printIf(w, "at", t.Applied.At)
		printIf(w, "by", t.Applied.By)
		if t.PendingApply {
			fmt.Fprintln(w, "    the configuration in Git has changed since - an apply is pending")
		}
	}

	fmt.Fprintln(w, "\n  OBSERVED (what the registry says)")
	switch {
	case t.Unreachable != "":
		fmt.Fprintf(w, "    could not read it: %s\n", t.Unreachable)
	case t.Observed == nil || !t.Observed.Configured:
		fmt.Fprintln(w, "    the registry holds no configuration for this target")
	default:
		printIf(w, "repository state", t.Observed.RepositoryState)
		printIf(w, "upstream", t.Observed.Upstream)
		if len(t.Observed.Tags) > 0 {
			fmt.Fprintf(w, "    %-16s %s\n", "tags", strings.Join(t.Observed.Tags, ", "))
		}
		if t.Observed.SyncStatus != "" {
			status := t.Observed.SyncStatus
			if status == "UNKNOWN" && t.Observed.SyncStatusRaw != "" {
				status += " - the registry said " + t.Observed.SyncStatusRaw + ", which this build does not recognise"
			}
			printIf(w, "sync status", status)
		}
	}

	if d := t.Drift; d != nil && d.Detected {
		fmt.Fprintln(w, "\n  DRIFT")
		fmt.Fprintf(w, "    %s\n", d.Summary)
		for _, f := range d.Fields {
			fmt.Fprintf(w, "      %-28s want %s, got %s\n", f.Field, quoteEmpty(f.Want), quoteEmpty(f.Got))
		}
		if d.CredentialsRotated {
			fmt.Fprintln(w, "      the upstream credential has been rotated since the last apply")
		}
		fmt.Fprintln(w, "\n    Drift is never closed automatically. `targets apply` closes it,")
		fmt.Fprintln(w, "    after showing you what that will do.")
	}
	return nil
}

func printIf(w io.Writer, label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(w, "    %-16s %s\n", label, value)
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// selectsSignatures mirrors the server-side check, so the CLI can say the same
// thing about a tag set without a second round trip.
func selectsSignatures(globs []string) bool {
	for _, g := range globs {
		if g == "*" || strings.HasPrefix(g, "sha256-") {
			return true
		}
	}
	return false
}

func newTargetsApplyCommand() *cobra.Command {
	var (
		dryRun bool
		yes    bool
	)
	cmd := &cobra.Command{
		Short: "Write the replication configuration to the registry",
		Long: "Shows what will change and, for a destructive change, asks first.\n\n" +
			"Destructive means what it says: putting a Quay repository into MIRROR\n" +
			"state makes it read-only to everyone but its robot, and narrowing a\n" +
			"tag glob deletes the tags that no longer match.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			// Always plan first, even when --yes was given. The plan is the
			// record of what an apply did, and a caller that skipped it would
			// have nothing to print afterwards.
			plan, err := client.ApplyReplication(cmd.Context(), args[0], args[1],
				v1.ApplyReplicationRequest{ValidateOnly: true})
			if err != nil {
				return err
			}
			if dryRun {
				return render(stdout(), opts.output, plan, func(w io.Writer) error {
					return renderApplyPlan(w, plan, true)
				})
			}

			resp, err := client.ApplyReplication(cmd.Context(), args[0], args[1],
				v1.ApplyReplicationRequest{Confirm: yes})
			if err != nil {
				return err
			}
			if resp.NeedsConfirmation {
				return render(stdout(), opts.output, resp, func(w io.Writer) error {
					if err := renderApplyPlan(w, resp, false); err != nil {
						return err
					}
					fmt.Fprintln(w, "\nThis change is destructive and was NOT applied.")
					fmt.Fprintf(w, "Re-run with --yes to proceed:\n\n")
					fmt.Fprintf(w, "  transferctl targets apply %s %s --yes\n", args[0], args[1])
					return nil
				})
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderApplyPlan(w, resp, false)
			})
		},
	}
	takes(cmd, "apply", productArg(), targetArg())
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the diff and change nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed with a destructive change without asking")
	return cmd
}

func renderApplyPlan(w io.Writer, resp *v1.ApplyReplicationResponse, dryRun bool) error {
	fmt.Fprintf(w, "%s / %s (%s)\n\n", resp.Product, resp.Target, resp.Mode)
	for _, s := range resp.Steps {
		fmt.Fprintf(w, "  %s\n", s)
	}
	switch {
	case resp.NoOp:
		// Nothing more to say. A no-op still means something - the registry
		// agrees with Git right now - and the step list already says it.
	case resp.Applied:
		fmt.Fprintln(w, "\nApplied.")
	case dryRun:
		fmt.Fprintln(w, "\nDry run: nothing was changed.")
	}
	if resp.Destructive && !resp.Applied {
		fmt.Fprintln(w, "\n  ⚠ This is destructive: it makes the repository read-only to everyone")
		fmt.Fprintln(w, "    but the mirror robot, and removes content the tag globs do not match.")
	}
	return nil
}

func newTargetsSyncCommand() *cobra.Command {
	var cancel bool
	cmd := &cobra.Command{
		Short: "Ask the registry to sync a mirror target now",
		Long: "Requests a sync and returns. There is no progress to follow: the\n" +
			"registry moves the bytes and reports a state, not a speed.\n\n" +
			"Use `transferctl targets describe` to see where it got to.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			var (
				resp *v1.SyncReplicationResponse
				err  error
			)
			if cancel {
				resp, err = client.CancelSyncReplication(cmd.Context(), args[0], args[1])
			} else {
				resp, err = client.SyncReplication(cmd.Context(), args[0], args[1])
			}
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				switch {
				case cancel && resp.Requested:
					fmt.Fprintf(w, "Cancelled the sync running on %s/%s.\n", resp.Product, resp.Target)
				case cancel:
					fmt.Fprintf(w, "No sync was running on %s/%s.\n", resp.Product, resp.Target)
				case resp.AlreadyRunning:
					// Not a failure: the caller wanted a sync, and one is
					// happening.
					fmt.Fprintf(w, "A sync of %s/%s was already running.\n", resp.Product, resp.Target)
				default:
					fmt.Fprintf(w, "Requested a sync of %s/%s at %s.\n", resp.Product, resp.Target, resp.At)
				}
				return nil
			})
		},
	}
	takes(cmd, "sync", productArg(), targetArg())
	cmd.Flags().BoolVar(&cancel, "cancel", false, "stop a sync that is in progress")
	return cmd
}

func newTargetsDriftCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "What differs between Git and the registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListReplication(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				drifted := 0
				for _, t := range resp.Targets {
					if t.Drift == nil || !t.Drift.Detected {
						continue
					}
					drifted++
					fmt.Fprintf(w, "%s\t%s\n", t.Target, t.Drift.Summary)
					for _, f := range t.Drift.Fields {
						fmt.Fprintf(w, "    %-28s want %s, got %s\n", f.Field, quoteEmpty(f.Want), quoteEmpty(f.Got))
					}
				}
				if drifted == 0 {
					fmt.Fprintln(w, "No drift: every delegated target's registry says what Git says.")
					return nil
				}
				fmt.Fprintf(w, "\n%d target(s) drifted. `targets apply` closes it, after showing you what that will do.\n", drifted)
				return nil
			})
		},
	}
	takes(cmd, "drift", productArg())
	return cmd
}

// targetArg is a configured target name, which is what an operator types and
// therefore what they mistype.
func targetArg() argSpec {
	return argSpec{
		Name: "target",
		Help: "a configured target in that product",
		Find: "transferctl targets list <product>",
	}
}
