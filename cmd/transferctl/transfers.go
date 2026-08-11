package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

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
		watch       bool
		interval    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List transfers, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// One tracker per transfer, kept across redraws: a rate needs two
			// readings, and a listing that rebuilt them every poll would never
			// have a second one.
			rates := rateTrackers{}

			once := func(w io.Writer) (bool, error) {
				resp, err := newClient().ListTransfers(cmd.Context(), v1.ListTransfersOptions{
					Product: productName,
					State:   state,
				})
				if err != nil {
					return false, err
				}
				if watch {
					rates.observe(resp.Transfers, time.Now())
				}
				if err := render(w, opts.output, resp, func(w io.Writer) error {
					return renderTransferList(w, resp, rates)
				}); err != nil {
					return false, err
				}
				return allSettled(resp.Transfers), nil
			}

			if watch {
				return watchLoop(cmd.Context(), stdout(), interval, once)
			}
			_, err := once(stdout())
			return err
		},
	}

	cmd.Flags().StringVar(&productName, "product", "", "only this product's transfers")
	cmd.Flags().StringVar(&state, "state", "",
		"only this state (running, succeeded, failed, ...)")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false,
		"re-read until every transfer has settled")
	cmd.Flags().DurationVar(&interval, "interval", DefaultWatchInterval,
		"how often --watch re-reads")
	return cmd
}

// allSettled reports that nothing is left to watch.
//
// A watch that never ends is one somebody leaves running; ending it when the
// work does means `--watch` can be the last line of a script.
func allSettled(transfers []v1.Transfer) bool {
	if len(transfers) == 0 {
		return false
	}
	for _, t := range transfers {
		switch t.State {
		case v1.TransferSucceeded, v1.TransferFailed, v1.TransferCancelled:
		default:
			return false
		}
	}
	return true
}

// rateTrackers holds one sampler per transfer, for a watch over a listing.
type rateTrackers map[string]*rateTracker

// observe records this poll's byte totals against every transfer on the page.
func (r rateTrackers) observe(transfers []v1.Transfer, at time.Time) {
	for i := range transfers {
		t := &transfers[i]
		tracker, ok := r[t.ID]
		if !ok {
			tracker = &rateTracker{}
			r[t.ID] = tracker
		}
		tracker.observe(int64Of(t.Progress.BytesTransferred), at)
	}
}

// watching reports whether any live rate has been measured yet, which is what
// decides whether the footnote describes a live rate or an average.
func (r rateTrackers) watching() bool {
	for _, tracker := range r {
		if tracker.smoothed > 0 {
			return true
		}
	}
	return false
}

// rateFor is the smoothed rate for one transfer, or zero when there is none —
// no watch, or only one reading so far.
func (r rateTrackers) rateFor(id string) float64 {
	if tracker, ok := r[id]; ok {
		return tracker.smoothed
	}
	return 0
}

func renderTransferList(
	w io.Writer, resp *v1.ListTransfersResponse, rates rateTrackers,
) error {
	return func(w io.Writer) error {
		if len(resp.Transfers) == 0 {
			fmt.Fprintln(w, "No transfers yet.")
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Transfers are created by auto-download rules when discovery finds")
			fmt.Fprintln(w, "a matching package. `transferctl packages list` shows what has been")
			fmt.Fprintln(w, "discovered so far.")
			return nil
		}

		tw := newTabWriter(w)
		fmt.Fprintln(tw,
			"ID\tPRODUCT\tTAG\tSTATE\tDONE\tJOBS\tCOPIED\tSPEED\tRUNNING\tELAPSED\tETA\tSAVED")
		for i := range resp.Transfers {
			t := &resp.Transfers[i]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f%%\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				shortID(t.ID),
				t.Product,
				transferTag(t),
				strings.ToLower(string(t.State)),
				percentComplete(t.Progress),
				jobProgress(t.Progress),
				bytesProgress(t.Progress),
				speedOf(t, rates.rateFor(t.ID)),
				t.Progress.JobsInFlight,
				elapsedOf(t),
				etaOf(t, rates.rateFor(t.ID)),
				humanBytes(t.Progress.DedupeSkippedBytes),
			)
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w, "DONE is by job count, which is the measure that reaches 100% — COPIED does not,")
		fmt.Fprintln(w, "because deduplicated content counts as planned and moves nothing.")
		if rates.watching() {
			fmt.Fprintln(w, "SPEED is the rate over the last half minute, and ETA extrapolates it — so a")
			fmt.Fprintln(w, "change you make now shows up within about that long.")
		} else {
			fmt.Fprintln(w, "SPEED is the average since the first job was leased, and ETA extrapolates it.")
			fmt.Fprintln(w, "An average carries the whole history, so it lags a speed that changed midway;")
			fmt.Fprintln(w, "`--watch` measures the live rate instead and uses that.")
		}
		fmt.Fprintln(w, "transferctl transfers describe <id>   full detail, including throughput")
		fmt.Fprintln(w, "transferctl transfers jobs <id>       what is copying right now")
		return nil
	}(w)
}

// transferTag prefers the vendor-shortened spelling in a table.
//
// NEAR puts `orb_` on the front of every tag it publishes, so a column of them
// is a column of the same four characters. The short form comes from the SERVER,
// computed by the source's vendor plugin, because only that plugin knows which
// part of a tag is structural noise — this package cannot tell `orb_25.7.2131`
// from a tag that genuinely begins that way. Both spellings resolve as input,
// and `describe` still shows the stored name.
func transferTag(t *v1.Transfer) string {
	if t.DisplayTag != "" {
		return t.DisplayTag
	}
	return t.Tag
}

// bytesProgress is how much of the package has actually crossed the network.
//
// Alongside the job count rather than instead of it, because they answer
// different questions and diverge honestly: jobs reaches 100%, bytes stops
// short by however much was deduplicated or mounted. Somebody watching a 30 GB
// bundle wants the gigabytes, not only the ratio.
func bytesProgress(p v1.TransferProgress) string {
	planned := int64Of(p.PlannedBytes)
	if planned <= 0 {
		return humanBytes(p.BytesTransferred)
	}
	return humanBytes(p.BytesTransferred) + "/" + humanBytes(p.PlannedBytes)
}

// speedOf is how fast this is going: the live rate under a watch, the average
// otherwise.
//
// The two are labelled differently in the footnote rather than shown in two
// columns. A listing is a scan for the row that is wrong, and a second rate
// column would cost the width of one that matters more.
func speedOf(t *v1.Transfer, live float64) string {
	if live > 0 {
		return humanRate(live)
	}
	rate, ok := averageRate(t)
	if !ok {
		return "-"
	}
	return humanRate(rate)
}

func elapsedOf(t *v1.Transfer) string {
	d, ok := elapsed(t)
	if !ok {
		return "-"
	}
	return humanDuration(d)
}

// etaOf extrapolates the remaining bytes, preferring a live rate to the average.
//
// live is the smoothed rate a watcher has measured, or zero when nothing is
// watching. It is preferred because the average is cumulative and therefore
// slow to react: after a change — a second worker, a proxy bypassed — the
// average still mostly describes the period before it, and the person who made
// the change is watching to find out whether it worked.
func etaOf(t *v1.Transfer, live float64) string {
	if t.State != v1.TransferRunning {
		return "-"
	}

	d, ok := estimateAt(t, live)
	if !ok {
		if d, ok = estimate(t); !ok {
			return "-"
		}
	}
	return "~" + humanDuration(d)
}

func newTransfersDescribeCommand() *cobra.Command {
	var (
		watch    bool
		interval time.Duration
	)

	cmd := &cobra.Command{
		Short: "Show one transfer in full",
		RunE: func(cmd *cobra.Command, args []string) error {
			// One tracker across the whole watch, so current and peak are
			// measured over the session rather than reset on every redraw.
			rates := &rateTracker{}

			once := func(w io.Writer) (bool, error) {
				t, err := newClient().GetTransfer(cmd.Context(), args[0])
				if err != nil {
					return false, err
				}
				rates.observe(int64Of(t.Progress.BytesTransferred), time.Now())

				if err := render(w, opts.output, t, func(w io.Writer) error {
					return describeTransfer(w, t, rates, watch)
				}); err != nil {
					return false, err
				}
				return settled(t.State), nil
			}

			if watch {
				return watchLoop(cmd.Context(), stdout(), interval, once)
			}
			_, err := once(stdout())
			return err
		},
	}

	takes(cmd, "describe", transferArg())
	cmd.Flags().BoolVarP(&watch, "watch", "w", false,
		"re-read until the transfer settles, reporting live throughput")
	cmd.Flags().DurationVar(&interval, "interval", DefaultWatchInterval,
		"how often --watch re-reads")
	return cmd
}

func settled(s v1.TransferState) bool {
	switch s {
	case v1.TransferSucceeded, v1.TransferFailed, v1.TransferCancelled:
		return true
	default:
		return false
	}
}

func describeTransfer(w io.Writer, t *v1.Transfer, rates *rateTracker, watching bool) error {
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
	fmt.Fprintf(pw, "  Transferred:\t%s of %s planned  (%.0f%% of jobs done)\n",
		humanBytes(p.BytesTransferred), humanBytes(p.PlannedBytes), percentComplete(p))
	fmt.Fprintf(pw, "  Deduplicated:\t%s never moved\n", humanBytes(p.DedupeSkippedBytes))
	if d, ok := elapsed(t); ok {
		fmt.Fprintf(pw, "  Elapsed:\t%s\n", humanDuration(d))
	}
	if t.State == v1.TransferRunning {
		if d, ok := estimateAt(t, rates.smoothed); ok {
			fmt.Fprintf(pw, "  Remaining:\t~%s at the current rate\n", humanDuration(d))
		} else if d, ok := estimate(t); ok {
			fmt.Fprintf(pw, "  Remaining:\t~%s at the average rate so far\n", humanDuration(d))
		}
	}
	if err := pw.Flush(); err != nil {
		return err
	}

	if err := describeThroughput(w, t, rates, watching); err != nil {
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

// describeThroughput reports how fast this is actually going.
//
// AVERAGE is derivable from what the server holds — bytes moved over time
// elapsed — so it is always available. CURRENT and PEAK are not: a rate needs
// two observations and the gap between them, and the server keeps no time
// series. The watcher takes those samples, so they appear under --watch and
// are honestly absent without it rather than being invented from one reading.
func describeThroughput(w io.Writer, t *v1.Transfer, rates *rateTracker, watching bool) error {
	avg, hasAvg := averageRate(t)
	if !hasAvg && !watching {
		return nil
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Throughput")
	tw := newTabWriter(w)

	if hasAvg {
		fmt.Fprintf(tw, "  Average:\t%s\tover the whole transfer\n", humanRate(avg))
	}
	if watching {
		if rates.current > 0 {
			fmt.Fprintf(tw, "  Current:\t%s\tover the last sample\n", humanRate(rates.current))
			// The one the ETA uses, so it is shown rather than left implicit:
			// a reader comparing "remaining" against a number on this page
			// should be able to find the number it was computed from.
			fmt.Fprintf(tw, "  Recent:\t%s\tover the last %s — what Remaining extrapolates\n",
				humanRate(rates.smoothed), humanDuration(smoothingWindow))
			fmt.Fprintf(tw, "  Peak:\t%s\thighest seen while watching\n", humanRate(rates.peak))
		} else {
			fmt.Fprintf(tw, "  Current:\t-\tmeasured from the next sample\n")
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if !watching {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Current and peak need two readings; `--watch` takes them.")
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
		watch      bool
		interval   time.Duration
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

			once := func(w io.Writer) (bool, error) {
				resp, err := newClient().ListTransferJobs(cmd.Context(), args[0], state)
				if err != nil {
					return false, err
				}
				if err := render(w, opts.output, resp, func(w io.Writer) error {
					return renderJobs(w, resp.Jobs, state)
				}); err != nil {
					return false, err
				}
				return jobsSettled(resp.Jobs), nil
			}

			if watch {
				return watchLoop(cmd.Context(), stdout(), interval, once)
			}
			_, err := once(stdout())
			return err
		},
	}

	takes(cmd, "jobs", transferArg())
	cmd.Flags().BoolVar(&failedOnly, "failed", false, "only jobs that have failed")
	cmd.Flags().StringVar(&state, "state", "",
		"only this state: leased shows what is running right now")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false,
		"re-read until no job is left running or runnable")
	cmd.Flags().DurationVar(&interval, "interval", DefaultWatchInterval,
		"how often --watch re-reads")
	return cmd
}

func renderJobs(w io.Writer, jobs []v1.Job, state string) error {
	if len(jobs) == 0 {
		if state != "" {
			fmt.Fprintf(w, "No %s jobs.\n", state)
			return nil
		}
		fmt.Fprintln(w, "This transfer has no jobs: nothing was planned for it.")
		return nil
	}

	// Rows arrive ordered by activity — what is running first, largest first
	// within that — so the top of the table is what is happening rather than
	// whatever happened to be planned first.
	tw := newTabWriter(w)
	fmt.Fprintln(tw,
		"STATE\tKIND\tSOURCE\tTARGET\tDIGEST\tSIZE\tMOVED\tATTEMPTS\tWAVE\tDETAIL")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			strings.ToLower(string(j.State)),
			j.Kind,
			jobSource(j),
			jobTarget(j),
			shortDigest(j.Digest),
			humanBytes(j.SizeBytes),
			humanBytes(j.BytesTransferred),
			attemptsOf(j),
			j.Wave,
			jobDetail(j),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "SOURCE and TARGET are the full registry paths this row reads from and writes")
	fmt.Fprintln(w, "to. A tag is shown only where the transfer will actually create one; a name")
	fmt.Fprintln(w, "in parentheses is what the content IS — the vendor's own name for it — where")
	fmt.Fprintln(w, "that differs from where it sits. A `*` marks content shared by several")
	fmt.Fprintln(w, "artifacts, so the one named is an example.")
	fmt.Fprintln(w, "One digest appearing twice with different targets is not a duplicate: a")
	fmt.Fprintln(w, "component is published both inside its bundle, so the index stays")
	fmt.Fprintln(w, "resolvable, and under its own name, so it can be pulled as itself.")
	fmt.Fprintln(w, "A `skipped` job is a success carrying zero bytes: the content was already")
	fmt.Fprintln(w, "at the destination, or the registry relocated it server-side.")
	return nil
}

// jobsSettled reports that nothing on this page can still move.
//
// Blocked counts as unsettled: a manifest blocked behind its blobs is waiting
// for work that is happening, and a watch that stopped there would quit just
// before the interesting part.
func jobsSettled(jobs []v1.Job) bool {
	if len(jobs) == 0 {
		return false
	}
	for _, j := range jobs {
		switch j.State {
		case v1.JobLeased, v1.JobPending, v1.JobBlocked:
			return false
		}
	}
	return true
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

// jobSource is the exact path a row READS FROM.
//
// A digest identifies content and nothing else, and a repository on its own
// only says which bundle. What a person needs is both: the repository the
// registry serves the bytes out of, and — for a blob, which is nobody's idea of
// a recognisable object — the image or chart it is a layer of, under the
// vendor's own name for it.
//
// # Why the artifact's name is sometimes a suffix and sometimes a parenthesis
//
// Everything in a bundle is read from ONE repository, because an index may only
// reference children co-located with it. So `repository:tag` is a reference you
// could actually pull only when the artifact's name belongs to that same
// repository. When a component names itself somewhere else — NEAR's ORBs name
// theirs `cfx-5000-product/mcc:25.7.2503` — it sits in the bundle's repository
// addressed by DIGEST alone, and printing `orbs/…:25.7.2503` would invent a
// reference that does not resolve. The name goes in parentheses there: this is
// what the content is, not where it is.
func jobSource(j v1.Job) string {
	if j.SourceRepository == "" {
		return "-"
	}

	out := j.SourceRepository
	repo, tag := splitRef(parentRef(j))
	if repo == "" || repo == j.SourceRepository {
		// Named here, or not named at all: the tag is this repository's own.
		if tag != "" {
			out += ":" + tag
		}
	} else {
		out += " (" + parentRef(j) + ")"
	}

	if j.Parent != nil && j.Parent.Shared {
		out += " *"
	}
	return out
}

// jobTarget is the exact path a row WRITES TO.
//
// The destination repository is per job rather than per transfer: a bundle's
// components each land in their own path under the target, reproducing the
// source's structure, and one component is often published in two places at
// once — in the bundle's repository so the index referencing it stays
// resolvable, and under its own name so it can be pulled as itself. Those are
// two jobs over one digest, and each row names its own destination.
//
// The tags are the row's OWN, never the parent's. A relocated component is
// deliberately left untagged in the bundle's repository — the name is not that
// repository's to claim — and borrowing the parent's tag to fill the column
// would advertise a reference the transfer is never going to create.
func jobTarget(j v1.Job) string {
	if j.TargetRepository == "" {
		return "-"
	}
	if len(j.TargetTags) == 0 {
		return j.TargetRepository
	}

	tags := append([]string(nil), j.TargetTags...)
	sort.Strings(tags)
	return j.TargetRepository + ":" + strings.Join(tags, ",")
}

// parentRef is the vendor's name for the artifact a row belongs to.
func parentRef(j v1.Job) string {
	if j.Parent == nil {
		return ""
	}
	return j.Parent.Ref
}

// splitRef separates `orbs/CFX-5000-k8s/nginx:1.2.3` into path and tag.
//
// The colon has to come after the last slash to be a tag separator: a registry
// host may carry a port, and `near.example.com:5000/orbs/x` is a path with no
// tag in it at all.
func splitRef(ref string) (repository, tag string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || i < strings.LastIndex(ref, "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}
