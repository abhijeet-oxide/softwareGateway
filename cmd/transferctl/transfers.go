package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newTransfersCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "transfers",
		Aliases: []string{"transfer"},
		Short:   "Watch packages moving to their destinations",
		Long: "A transfer is one package moving to ONE destination. A request\n" +
			"naming three targets produces three transfers, which succeed or\n" +
			"fail independently — one unreachable registry does not hold up\n" +
			"the other two.",
	})
	cmd.AddCommand(
		newTransfersListCommand(),
		newTransfersDescribeCommand(),
		newTransfersJobsCommand(),
	)
	return cmd
}

func newTransfersListCommand() *cobra.Command {
	var (
		productName string
		state       string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List transfers, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := newClient().ListTransfers(cmd.Context(), v1.ListTransfersOptions{
				Product: productName,
				State:   state,
			})
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				if len(resp.Transfers) == 0 {
					fmt.Fprintln(w, "No transfers yet.")
					fmt.Fprintln(w)
					fmt.Fprintln(w, "Transfers are created by auto-download rules when discovery finds")
					fmt.Fprintln(w, "a matching package. `transferctl packages list` shows what has been")
					fmt.Fprintln(w, "discovered so far.")
					return nil
				}

				tw := newTabWriter(w)
				fmt.Fprintln(tw, "ID\tPRODUCT\tTAG\tTARGET\tSTATE\tPROGRESS\tMOVED\tSAVED")
				for _, t := range resp.Transfers {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						shortID(t.ID),
						t.Product,
						t.Tag,
						t.Target,
						strings.ToLower(string(t.State)),
						jobProgress(t.Progress),
						humanBytes(t.Progress.BytesTransferred),
						humanBytes(t.Progress.DedupeSkippedBytes),
					)
				}
				if err := tw.Flush(); err != nil {
					return err
				}

				fmt.Fprintln(w)
				fmt.Fprintln(w, "SAVED is what deduplication avoided moving — content the destination already had.")
				fmt.Fprintln(w, "transferctl transfers describe <id>   full detail")
				fmt.Fprintln(w, "transferctl transfers jobs <id>       per-blob progress")
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&productName, "product", "", "only this product's transfers")
	cmd.Flags().StringVar(&state, "state", "",
		"only this state (running, succeeded, failed, ...)")
	return cmd
}

func newTransfersDescribeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Show one transfer in full",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newClient().GetTransfer(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, t, func(w io.Writer) error {
				return describeTransfer(w, t)
			})
		},
	}
	takes(cmd, "describe", transferArg())
	return cmd
}

func describeTransfer(w io.Writer, t *v1.Transfer) error {
	tw := newTabWriter(w)
	fmt.Fprintf(tw, "ID:\t%s\n", t.ID)
	fmt.Fprintf(tw, "Product:\t%s\n", t.Product)
	fmt.Fprintf(tw, "Package:\t%s\n", t.Tag)
	fmt.Fprintf(tw, "Source:\t%s\n", t.Source)
	fmt.Fprintf(tw, "Target:\t%s\n", t.Target)
	fmt.Fprintf(tw, "State:\t%s\n", strings.ToLower(string(t.State)))
	fmt.Fprintf(tw, "Priority:\t%d\n", t.Priority)
	fmt.Fprintf(tw, "Wave:\t%d of %d\n", t.CurrentWave, t.MaxWave)
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Progress")
	pw := newTabWriter(w)
	p := t.Progress
	fmt.Fprintf(pw, "  Jobs:\t%d done, %d outstanding, %d failed (of %d planned)\n",
		p.JobsDone, p.JobsOutstanding, p.JobsFailed, p.JobsPlanned)
	fmt.Fprintf(pw, "  Transferred:\t%s of %s planned\n",
		humanBytes(p.BytesTransferred), humanBytes(p.PlannedBytes))
	fmt.Fprintf(pw, "  Deduplicated:\t%s never moved\n", humanBytes(p.DedupeSkippedBytes))
	if err := pw.Flush(); err != nil {
		return err
	}

	if t.FailureReason != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Failed: %s\n", t.FailureReason)
		fmt.Fprintln(w, "`transferctl transfers jobs "+shortID(t.ID)+"` shows which job, and why.")
	}

	// A transfer that is not moving and has no failure reason is the case
	// worth explaining, because it looks identical to a broken one: waves are
	// sequential, so nothing in wave 2 starts until wave 1 has fully drained.
	if t.State == v1.TransferRunning && p.JobsOutstanding > 0 && t.MaxWave > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Wave %d of %d is in flight. Manifests are pushed only after every\n",
			t.CurrentWave, t.MaxWave)
		fmt.Fprintln(w, "blob beneath them has landed, so a tag never appears at the destination")
		fmt.Fprintln(w, "until the whole package is there.")
	}
	return nil
}

func newTransfersJobsCommand() *cobra.Command {
	var failedOnly bool

	cmd := &cobra.Command{
		Short: "Show per-blob progress",
		Long: "The unit of work is a BLOB, not a package. This is that level:\n" +
			"which blob, how far, on which attempt, and — when something is\n" +
			"stuck — which worker is holding it and what the registry said.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListTransferJobs(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				jobs := resp.Jobs
				if failedOnly {
					jobs = filterFailed(jobs)
				}
				if len(jobs) == 0 {
					if failedOnly {
						fmt.Fprintln(w, "No failed jobs.")
						return nil
					}
					fmt.Fprintln(w, "This transfer has no jobs: nothing was planned for it.")
					return nil
				}

				tw := newTabWriter(w)
				fmt.Fprintln(tw, "WAVE\tKIND\tDIGEST\tSIZE\tSTATE\tMOVED\tATTEMPTS\tDETAIL")
				for _, j := range jobs {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						j.Wave,
						j.Kind,
						shortDigest(j.Digest),
						humanBytes(j.SizeBytes),
						strings.ToLower(string(j.State)),
						humanBytes(j.BytesTransferred),
						attemptsOf(j),
						jobDetail(j),
					)
				}
				if err := tw.Flush(); err != nil {
					return err
				}

				fmt.Fprintln(w)
				fmt.Fprintln(w, "A `skipped` job is a success carrying zero bytes: the content was already")
				fmt.Fprintln(w, "at the destination, or the registry relocated it server-side.")
				return nil
			})
		},
	}

	takes(cmd, "jobs", transferArg())
	cmd.Flags().BoolVar(&failedOnly, "failed", false, "only jobs that have failed")
	return cmd
}

// transferArg is the transfer ID every per-transfer command takes.
//
// Find matters more here than for most arguments: a transfer ID is a UUID
// nobody has memorised, and an error that does not say where to get one leaves
// the reader stuck with a correct message and no next step.
func transferArg() argSpec {
	return argSpec{
		Name: "transfer-id",
		Help: "the transfer to look at, as shown by `transferctl transfers list`",
		Find: "transferctl transfers list",
	}
}

func filterFailed(jobs []v1.Job) []v1.Job {
	out := make([]v1.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.State == v1.JobFailed {
			out = append(out, j)
		}
	}
	return out
}

// jobDetail is the one column that explains an unexpected state.
func jobDetail(j v1.Job) string {
	switch {
	case j.LastError != "":
		detail := j.LastError
		if j.LastErrorClass != "" {
			detail = j.LastErrorClass + ": " + detail
		}
		return truncate(detail, 60)
	case j.SkipReason != "":
		return string(j.SkipReason)
	case j.LeaseOwner != "":
		return "held by " + j.LeaseOwner
	default:
		return "-"
	}
}

func attemptsOf(j v1.Job) string {
	if j.Attempts == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", j.Attempts, j.MaxAttempts)
}

// jobProgress renders "done/planned" without pretending to a percentage when
// nothing has been planned yet.
func jobProgress(p v1.TransferProgress) string {
	if p.JobsPlanned == 0 {
		return "-"
	}
	out := fmt.Sprintf("%d/%d", p.JobsDone, p.JobsPlanned)
	if p.JobsFailed > 0 {
		out += fmt.Sprintf(" (%d failed)", p.JobsFailed)
	}
	return out
}

// shortID trims a UUID to its first segment.
//
// Transfers are identified by UUID, which is unreadable at a glance and
// unnecessary in a listing: the first segment is enough to tell rows apart and
// enough to paste into `describe`, which accepts the full ID.
func shortID(id string) string {
	if i := strings.Index(id, "-"); i > 0 {
		return id[:i]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
