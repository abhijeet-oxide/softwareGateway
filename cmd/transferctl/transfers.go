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
		newTransfersCreateCommand(),
		newTransfersPromoteCommand(),
		newTransfersListCommand(),
		newTransfersDescribeCommand(),
		newTransfersJobsCommand(),
	)
	return cmd
}

// copySpec is the flag set create and promote share.
//
// They differ only in how an omitted origin and destination are RESOLVED, so
// sharing the flags is not tidiness — it is what stops the two drifting into
// subtly different spellings of the same idea.
type copySpec struct {
	from          string
	to            []string
	toEnvironment string
	priority      int
	dryRun        bool
}

func (c *copySpec) bind(cmd *cobra.Command, fromHelp, toHelp string) {
	cmd.Flags().StringVar(&c.from, "from", "", fromHelp)
	cmd.Flags().StringArrayVar(&c.to, "to", nil, toHelp)
	cmd.Flags().StringVar(&c.toEnvironment, "to-environment", "",
		"copy to every target in this environment")
	cmd.Flags().IntVar(&c.priority, "priority", 0, "0-1000; higher runs first (default 50)")
	cmd.Flags().BoolVar(&c.dryRun, "dry-run", false,
		"resolve and check everything, create nothing")
}

func (c *copySpec) request(product, pkg string, promote bool) v1.CreateTransferRequest {
	return v1.CreateTransferRequest{
		Product:       product,
		Package:       pkg,
		From:          c.from,
		To:            c.to,
		ToEnvironment: c.toEnvironment,
		Promote:       promote,
		Priority:      c.priority,
		ValidateOnly:  c.dryRun,
	}
}

func newTransfersCreateCommand() *cobra.Command {
	var spec copySpec

	cmd := &cobra.Command{
		Aliases: []string{"copy"},
		Short:   "Copy a package to one or more targets",
		Long: "Copies a package and everything it references — images, charts,\n" +
			"generic artifacts — preserving every digest, repository path and\n" +
			"tag the vendor published.\n\n" +
			"The OPERATION is derived, never typed. --from naming a source is a\n" +
			"replication; --from naming a target is a promotion. Omitted, it is\n" +
			"the repository the package was discovered in.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(spec.to) > 0 && spec.toEnvironment != "" {
				return usageError{msg: "--to and --to-environment name destinations " +
					"two different ways; use one"}
			}
			return runCopy(cmd, spec.request(args[0], args[1], false), spec.dryRun)
		},
	}

	takes(cmd, "create", productArg(), packageArg())
	spec.bind(cmd,
		"where to copy FROM: a source or a target (default: where the package was discovered)",
		"a target to copy to (repeatable; default: the product's default target)")
	return cmd
}

func newTransfersPromoteCommand() *cobra.Command {
	var spec copySpec

	cmd := &cobra.Command{
		Short: "Promote a package from one target to another",
		Long: "Promotion moves between YOUR targets — lab to production — rather\n" +
			"than from a vendor. It is the same copy underneath; what differs is\n" +
			"that the origin must be a target, and that omitting --from/--to\n" +
			"resolves them through the product's promotion path.\n\n" +
			"With one target in each environment neither flag is needed. With\n" +
			"several, the ambiguity is refused rather than guessed, and the error\n" +
			"names every candidate.\n\n" +
			"Usually near-instant: lab and production commonly share a registry,\n" +
			"so blobs are relocated server-side rather than moved.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(spec.to) > 0 && spec.toEnvironment != "" {
				return usageError{msg: "--to and --to-environment name destinations " +
					"two different ways; use one"}
			}
			return runCopy(cmd, spec.request(args[0], args[1], true), spec.dryRun)
		},
	}

	takes(cmd, "promote", productArg(), packageArg())
	spec.bind(cmd,
		"the target to promote FROM (default: the single target in the promotion source environment)",
		"a target to promote to (repeatable; default: the single target in the promotion destination environment)")
	return cmd
}

// runCopy is the one request path both verbs take.
func runCopy(cmd *cobra.Command, req v1.CreateTransferRequest, dryRun bool) error {
	resp, err := newClient().CreateTransfer(cmd.Context(), req)
	if err != nil {
		return err
	}

	return render(stdout(), opts.output, resp, func(w io.Writer) error {
		verb := "Copying"
		if strings.EqualFold(resp.Operation, "promote") {
			verb = "Promoting"
		}

		destinations := make([]string, 0, len(resp.To))
		for _, t := range resp.To {
			destinations = append(destinations, describeEndpoint(t))
		}

		fmt.Fprintf(w, "%s %s\n", verb, req.Package)
		fmt.Fprintf(w, "  from  %s\n", describeEndpoint(resp.From))
		for _, d := range destinations {
			fmt.Fprintf(w, "  to    %s\n", d)
		}

		if dryRun {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Dry run: nothing was created.")
			fmt.Fprintln(w, "Everything above resolved — the product, the package, the origin,")
			fmt.Fprintln(w, "the destinations and their promotion rules. What would MOVE is not")
			fmt.Fprintln(w, "computed here; run it and `transferctl transfers describe` reports it.")
			return nil
		}

		fmt.Fprintln(w)
		if !resp.Created {
			fmt.Fprintln(w, "This was already requested; the existing request is reused.")
		}
		for i, id := range resp.TransferIDs {
			name := ""
			if i < len(resp.To) {
				name = " -> " + resp.To[i].Name
			}
			fmt.Fprintf(w, "  transfer %s%s\n", shortID(id), name)
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w, "Bytes move once a worker picks the jobs up. Follow with:")
		if len(resp.TransferIDs) > 0 {
			fmt.Fprintf(w, "  transferctl transfers describe %s\n", shortID(resp.TransferIDs[0]))
		}
		return nil
	})
}

// describeEndpoint renders a resolved end of a copy.
func describeEndpoint(e v1.TransferEndpoint) string {
	out := e.Name
	if e.Environment != "" {
		out += " (" + e.Environment + ")"
	}
	out += "  " + e.Registry
	if e.Repository != "" {
		out += "/" + e.Repository
	}
	return out
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
				fmt.Fprintln(tw, "ID\tPRODUCT\tTAG\tTARGET\tSTATE\tPROGRESS\tRUNNING\tMOVED\tSAVED")
				for _, t := range resp.Transfers {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
						shortID(t.ID),
						t.Product,
						t.Tag,
						t.Target,
						strings.ToLower(string(t.State)),
						jobProgress(t.Progress),
						t.Progress.JobsInFlight,
						humanBytes(t.Progress.BytesTransferred),
						humanBytes(t.Progress.DedupeSkippedBytes),
					)
				}
				if err := tw.Flush(); err != nil {
					return err
				}

				fmt.Fprintln(w)
				fmt.Fprintln(w, "RUNNING is how many jobs are being worked on right now.")
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
	fmt.Fprintf(pw, "  In flight:\t%s\n", inFlight(p))
	if p.JobsWaiting > 0 {
		fmt.Fprintf(pw, "  Waiting:\t%d in retry backoff\n", p.JobsWaiting)
	}
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
	// worth explaining, because it looks identical to a broken one.
	if t.State == v1.TransferRunning && p.JobsOutstanding > 0 && t.MaxWave > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Wave %d of %d. Manifests are pushed only after every blob beneath\n",
			t.CurrentWave, t.MaxWave)
		fmt.Fprintln(w, "them has landed, so a tag never appears at the destination until the")
		fmt.Fprintln(w, "whole package is there.")

		// Nothing running is the state most likely to be mistaken for a hang,
		// and it has two very different causes.
		if p.JobsInFlight == 0 {
			fmt.Fprintln(w)
			switch {
			case p.JobsWaiting > 0:
				fmt.Fprintf(w,
					"No job is running: %d are waiting out a retry backoff. They become\n",
					p.JobsWaiting)
				fmt.Fprintln(w, "runnable on their own; `transfers jobs <id> --failed` shows why they failed.")
			default:
				fmt.Fprintln(w, "No job is running and none is waiting. Check that a worker is up and")
				fmt.Fprintln(w, "pointed at this Coordinator — `transferctl health` reports what it sees.")
			}
		}
	}
	return nil
}

// inFlight renders concurrency in the terms a reader is asking about.
//
// "3 across 1 worker" answers the question directly; a bare count leaves them
// wondering whether the fleet or the queue is the limit.
func inFlight(p v1.TransferProgress) string {
	if p.JobsInFlight == 0 {
		return "nothing running"
	}
	worker := "workers"
	if p.Workers == 1 {
		worker = "worker"
	}
	return fmt.Sprintf("%d job(s) across %d %s", p.JobsInFlight, p.Workers, worker)
}

func newTransfersJobsCommand() *cobra.Command {
	var (
		failedOnly bool
		state      string
	)

	cmd := &cobra.Command{
		Short: "Show per-blob progress",
		Long: "The unit of work is a BLOB, not a package. This is that level:\n" +
			"which blob, how far, on which attempt, and — when something is\n" +
			"stuck — which worker is holding it and what the registry said.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if failedOnly && state != "" {
				return usageError{msg: "--failed and --state name the same thing two ways; use one"}
			}
			if failedOnly {
				state = "failed"
			}

			resp, err := newClient().ListTransferJobs(cmd.Context(), args[0], state)
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				jobs := resp.Jobs
				if len(jobs) == 0 {
					if state != "" {
						fmt.Fprintf(w, "No %s jobs.\n", state)
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
	cmd.Flags().StringVar(&state, "state", "",
		"only this state: leased shows what is running right now")
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
