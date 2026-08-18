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
		newTransfersFailuresCommand(),
		newTransfersRetryCommand(),
		newTransfersPauseCommand(),
		newTransfersResumeCommand(),
		newTransfersStopCommand(),
		newTransfersDeleteCommand(),
		newTransfersSetPriorityCommand(),
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
		all         bool
		watch       bool
		interval    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List transfers, newest first",
		Long: "Shows what is still moving. A transfer that has finished with\n" +
			"nothing left to act on is counted at the bottom rather than listed;\n" +
			"--all lists every transfer, and so does naming a state with --state.\n\n" +
			"A FAILED transfer is never hidden: it has finished, and it is the\n" +
			"one thing this listing exists to put in front of somebody.",
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
				// A state named explicitly is a state somebody asked to see, so
				// the default hiding does not apply to it: `--state succeeded`
				// filtering out everything succeeded would be an answer to a
				// question nobody asked.
				showAll := all || strings.TrimSpace(state) != ""
				if err := render(w, opts.output, resp, func(w io.Writer) error {
					return renderTransferList(w, resp, rates, listView{
						all: showAll, watching: watch,
					})
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
	cmd.Flags().BoolVar(&all, "all", false,
		"list transfers that have already finished, not only what is still moving")
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

// rateFor is the smoothed rate for one transfer, or zero when there is none —
// no watch, or only one reading so far.
func (r rateTrackers) rateFor(id string) float64 {
	if tracker, ok := r[id]; ok {
		return tracker.smoothed
	}
	return 0
}

// transferElastic are the columns of this listing that may be shortened to make
// a row fit: the two holding a registry path, which is where the width in this
// table actually is and which stays recognisable when its middle is removed.
//
// PRODUCT and TAG are deliberately NOT offered. They are a few characters
// longer than their headers and identical down the page, so shortening them
// reclaims almost nothing — and the first version of this did offer them, which
// on a narrow terminal turned `cfx-5000-product` into `cfx-50…product` on every
// row on the way to a row that wrapped anyway. A column should only be spent
// where spending it buys the fit.
var transferElastic = []int{3, 4} // FROM, TO

// listView is how this listing is being read, which decides what belongs on it
// beyond the rows.
type listView struct {
	// all suppresses the hiding of finished transfers.
	all bool
	// watching says the page is being redrawn on a timer.
	//
	// A hint about a flag is worth printing once. Redrawn every few seconds
	// under the reader's eyes it is a fixed line of advice they have already
	// read and cannot act on without stopping the watch — so the footer is left
	// off, and the rows keep the whole screen.
	watching bool
}

// delegated reports whether the REGISTRY moved this transfer's bytes.
//
// Read from the recorded strategy rather than inferred from an empty job list:
// a copy transfer that has not been planned yet also has no jobs, and the two
// must never render the same way.
func delegated(t *v1.Transfer) bool {
	return t.Strategy != "" && t.Strategy != "copy"
}

// dashDelegatedCells blanks the measured columns of one list row.
//
// The indices are the positions in the header, and they are listed rather than
// computed so that adding a column cannot silently start reporting a synthesised
// number for a delegated transfer.
func dashDelegatedCells(row []string) {
	const (
		colDone    = 6
		colJobs    = 7
		colFailed  = 8
		colCopied  = 9
		colSaved   = 10
		colLeft    = 11
		colPlanned = 12
		colSpeed   = 13
		colRunning = 14
		colETA     = 16
	)
	for _, i := range []int{colDone, colJobs, colFailed, colCopied, colSaved,
		colLeft, colPlanned, colSpeed, colRunning, colETA} {
		if i < len(row) {
			row[i] = "—"
		}
	}
}

func renderTransferList(
	w io.Writer, resp *v1.ListTransfersResponse, rates rateTrackers, view listView,
) error {
	if len(resp.Transfers) == 0 {
		fmt.Fprintln(w, "No transfers yet.")
		return nil
	}

	rows := [][]string{{
		"ID", "PRODUCT", "TAG", "FROM", "TO", "STATE", "DONE", "JOBS", "FAILED",
		"COPIED", "SAVED", "LEFT", "PLANNED", "SPEED", "RUNNING", "ELAPSED", "ETA",
	}}

	finished := 0
	for i := range resp.Transfers {
		t := &resp.Transfers[i]
		if !view.all && isFinished(t) {
			finished++
			continue
		}
		row := []string{
			shortID(t.ID),
			t.Product,
			transferTag(t),
			endpointName(t.SourceName, t.Source),
			endpointName(t.TargetName, t.Target),
			strings.ToLower(string(t.State)),
			fmt.Sprintf("%.0f%%", percentComplete(t.Progress)),
			jobProgress(t.Progress),
			failedJobs(t.Progress),
			copiedBytes(t.Progress),
			savedBytes(t.Progress),
			leftBytes(t.Progress),
			plannedBytes(t.Progress),
			speedOf(t, rates.rateFor(t.ID)),
			fmt.Sprint(t.Progress.JobsInFlight),
			elapsedOf(t),
			etaOf(t, rates.rateFor(t.ID)),
		}
		// EVERY measured column becomes an em dash for a transfer whose bytes
		// we did not move. They would all be structurally zero otherwise, and
		// a zero in a byte column reads as "nothing has happened" rather than
		// as "we cannot count this" — which is the difference between a
		// reader waiting patiently and a reader raising an incident.
		if delegated(t) {
			dashDelegatedCells(row)
		}
		rows = append(rows, row)
	}

	if len(rows) > 1 {
		fitCells(rows, transferElastic, terminalWidth(w))

		tw := newTabWriter(w)
		for _, row := range rows {
			fmt.Fprintln(tw, strings.Join(row, "\t"))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	} else {
		// Everything there is has finished. Saying so beats a bare column
		// header, which reads as a command that failed to produce output.
		fmt.Fprintln(w, "Nothing in flight.")
	}

	if finished > 0 && !view.watching {
		fmt.Fprintf(w, "%s not shown; --all lists every transfer.\n",
			plural(finished, "finished transfer", "finished transfers"))
	}
	return nil
}

// isFinished reports a transfer with nothing left to act on.
//
// `failed` is deliberately NOT one of them. It has finished in the sense that
// nothing more will happen to it on its own, and it is precisely what somebody
// scanning this listing needs to see — hiding it would leave the default view
// showing everything except the problem.
func isFinished(t *v1.Transfer) bool {
	switch t.State {
	case v1.TransferSucceeded, v1.TransferCancelled:
		return true
	default:
		return false
	}
}

// endpointName is where a transfer reads from, or writes to, in one column.
//
// The configured name is what an operator typed into --from or --to, so it is
// the spelling they can act on: it goes back into another command unchanged. The
// resolved host and path is the fallback, and it is a fallback rather than the
// first choice because it is a hundred characters wide and mostly identical
// down the page — every row of one product shares a registry.
func endpointName(name, resolved string) string {
	if name != "" {
		return name
	}
	if resolved == "" {
		return "-"
	}
	return resolved
}

// savedBytes is what the transfer did not have to move.
//
// The whole saving, not the part known at planning time. Reading only
// DedupeSkippedBytes reported `0 B` for a transfer that skipped 32 GiB, and it
// did so in exactly the case the column exists for: on a fresh database nothing
// is deduplicated during planning, so every byte saved is saved by a worker.
func savedBytes(p v1.TransferProgress) string {
	if int64Of(p.SavedBytes) == 0 {
		return "-"
	}
	return humanBytes(p.SavedBytes)
}

// failedJobs is the column that was missing.
//
// A transfer whose jobs are failing looked EXACTLY like one that was working:
// the state stayed `running`, DONE crept along, and the only place a failure
// appeared at all was `transfers jobs --failed`, which nobody runs against a
// transfer they believe is fine. Now the count is on the row, and a dash when
// there is nothing to report so a healthy fleet does not grow a column of
// zeroes to read past.
func failedJobs(p v1.TransferProgress) string {
	if p.JobsFailed == 0 {
		return "-"
	}
	return fmt.Sprint(p.JobsFailed)
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
// Three byte columns that add up, and why COPIED is not a fraction.
//
// # The fraction was the problem, not its denominator
//
// COPIED read `X of Y`, and every candidate for Y was wrong in a way the reader
// could see. Against the QUEUED work it produced `490 KiB/29.8 GiB · SAVED 63.7
// GiB` — saving more than the whole of what was being moved. Against the
// RELEASE it produced `5.4 KiB/63.7 GiB · SAVED 63.7 GiB`, which asks the
// obvious question: if all of it was saved, what is the 63.7 GiB being copied?
//
// Nothing. Y does not exist. How much of a release has to cross the wire is not
// known when the transfer starts and is not known while it runs — each job
// settles it, one HEAD at a time — so a fraction there states a fact nobody has.
//
// So the row states the three quantities that are real:
//
//	COPIED   what has crossed the wire
//	SAVED    what did not have to: already there, or relocated inside the target
//	TOTAL    the release
//
// They add up in the only way that is true: COPIED + SAVED reaches TOTAL as the
// transfer finishes, and the gap between them at any moment is the work not yet
// decided either way. A reader who wants a percentage has DONE, which counts
// jobs and reaches 100%.

// copiedBytes is what has actually crossed the wire.
func copiedBytes(p v1.TransferProgress) string {
	if int64Of(p.BytesTransferred) == 0 {
		return "-"
	}
	return humanBytes(p.BytesTransferred)
}

// leftBytes is the work whose outcome nobody knows yet.
//
// # The column that closes the row
//
// COPIED and SAVED are both DECIDED: bytes that crossed the wire, and bytes
// that turned out not to need to. Neither says how much is still to come, and
// without that the row could not be reconciled — a reader seeing `SAVED 63.7
// GiB` beside `PLANNED 63.7 GiB` concludes there is nothing left to copy, and
// then watches COPIED climb.
//
// The two are not equal. They differ by exactly this column plus whatever
// failed, and at one decimal place in gigabytes a difference of a few hundred
// megabytes is invisible — so the row looked like a contradiction while every
// number in it was right.
//
// With it, the plan is accounted for: COPIED plus SAVED plus LEFT is the plan,
// less anything that failed outright.
//
// # What it is not
//
// It is not "the bytes still to copy". Nothing knows that: each job settles it
// for itself, with a HEAD at the destination, as it runs. What is left will
// divide between COPIED and SAVED in a proportion nobody can state in advance,
// and this is the honest bound on it — everything still to copy is in here, and
// so is everything still to be skipped.
func leftBytes(p v1.TransferProgress) string {
	if int64Of(p.OutstandingBytes) == 0 {
		return "-"
	}
	return humanBytes(p.OutstandingBytes)
}

// plannedBytes is the WORK: every byte the transfer has to account for, queued
// or already accounted for at planning time.
//
// PLANNED rather than TOTAL, and the rename is the point. A component of a
// bundle is published twice — inside the bundle so its index resolves, and
// under the name the vendor gave it — and a registry stores blobs per
// repository, so one blob landing in two repositories is two placements and two
// jobs. Its bytes are counted twice here, and a base layer shared by fifty
// components is counted fifty times.
//
// Called TOTAL it invited the only comparison that makes it look wrong: a
// listing reporting a 29.8 GiB orb, and a transfer of that orb reporting 63.7
// GiB. Both are right. One is what the release weighs and the other is what the
// transfer has to do, and only the second belongs beside COPIED and SAVED —
// which are also per-job, and which add up to exactly this.
//
// The release's own size is on `describe`, next to this one, where the two can
// be seen to be different quantities rather than inconsistent ones.
func plannedBytes(p v1.TransferProgress) string {
	total := packageBytes(p)
	if total <= 0 {
		return "-"
	}
	return humanBytesOf(total)
}

// speedOf is how fast this is going: the live rate under a watch, the average
// otherwise.
//
// The two are labelled differently in the footnote rather than shown in two
// columns. A listing is a scan for the row that is wrong, and a second rate
// column would cost the width of one that matters more.
// isSettled reports that a transfer has nothing left to do.
//
// The gate on the ETA, and it used to be `state == running` — which is a
// different question and answered wrongly for a real transfer. A transfer is
// `ready` from the moment it is planned until its first job COMPLETES, and with
// multi-gigabyte blobs that window is long; a resumed one starts inside it. So a
// transfer with ten blobs in flight showed a speed and a blank ETA at the same
// time, which reads as the estimate being broken rather than as the state word
// lagging.
//
// Asking what a transfer has LEFT rather than what it is called is also the
// question that keeps answering itself correctly: `paused` and `verifying` are
// states this list will meet later, and both have remaining work to estimate.
// The states below have none, and estimate() declines the rest on its own when
// there is no rate to extrapolate from.
func isSettled(state v1.TransferState) bool {
	switch state {
	case v1.TransferSucceeded, v1.TransferFailed, v1.TransferCancelled:
		return true
	default:
		return false
	}
}

// speedOf answers "how fast is this going", which is a question about NOW.
//
// The average over the whole transfer is not an answer to it. A transfer with
// nothing in flight is going at zero, whatever it averaged over the previous
// thirty-one hours, and printing 581.9 KiB/s next to a RUNNING column reading 0
// is the table contradicting itself on one line. The average is still a fact,
// and `transfers describe` is where it belongs, labelled as what it is.
// speedOf is how fast this went: the live rate while it is running, the
// average over the whole run once it has finished.
//
// # Why a finished transfer has a speed at all
//
// It used to print a dash, on the rule that nothing in flight means no rate to
// report. That rule is right for a transfer that is STUCK — a rate measured
// before it stalled describes a period that has ended, and printing it invites
// the reader to extrapolate from it — and wrong for one that has finished, where
// the average over the whole run is not a stale sample but the answer: this is
// what the link did, and it is the number somebody compares against the next
// run and against the other target.
//
// A transfer that moved no bytes still has no rate, and says so. Dividing zero
// by the elapsed time would print `0 B/s` for a delta transfer that did exactly
// what it should have, which reads as a link that was not working.
func speedOf(t *v1.Transfer, live float64) string {
	if !isSettled(t.State) && t.Progress.JobsInFlight == 0 {
		return "-"
	}
	if live > 0 && !isSettled(t.State) {
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
	if isSettled(t.State) {
		return "-"
	}

	// NOTHING IN FLIGHT MEANS NO ESTIMATE, and the reason is the difference
	// between an arithmetic answer and a true one. Extrapolating the remaining
	// bytes over the average rate gave "~1m22s" for a transfer that had been
	// motionless for hours: the arithmetic was right and the sentence was
	// false, because it assumed a rate that no longer applied.
	//
	// What is printed instead says WHY there is no estimate, because that is
	// the actionable half. A job in backoff will start again on its own; a
	// transfer with nothing running and nothing waiting will not.
	if t.Progress.JobsInFlight == 0 {
		switch {
		case t.Progress.JobsOutstanding == 0:
			return "-"
		case t.Progress.JobsWaiting > 0:
			return "waiting"
		default:
			return "stalled"
		}
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

// renderContent says what the transfer is made OF, and how each kind went.
//
// # The question the byte counts cannot answer
//
// `63.7 GiB` and `2486/2489 jobs` are both true and neither says whether the
// charts arrived. An orb is images, charts and configuration files; those are
// what somebody releases and what they are asked about afterwards, and until
// this table existed the only way to count them was to read three thousand job
// rows.
//
// COPIED and PRESENT are the pair that makes a re-transfer legible: `6 copied,
// 253 present` is the whole story of a delta, and it is invisible in a jobs
// count where both are simply `done`.
//
// Components, not jobs: a component published under two names is one thing a
// person can name, and counting its jobs would report it twice.
func renderContent(w io.Writer, content []v1.ContentGroup) error {
	if len(content) == 0 {
		return nil
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Content")
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  KIND\tTOTAL\tCOPIED\tPRESENT\tFAILED\tOUTSTANDING")

	var total v1.ContentGroup
	for _, g := range content {
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\n",
			kindLabel(g.Kind, g.Total), g.Total,
			count(g.Copied), count(g.Present), count(g.Failed), count(g.Outstanding))

		total.Total += g.Total
		total.Copied += g.Copied
		total.Present += g.Present
		total.Failed += g.Failed
		total.Outstanding += g.Outstanding
	}

	// A total only where there is more than one row to add up. On a single-kind
	// transfer it would restate the line above it.
	if len(content) > 1 {
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\n",
			"all", total.Total,
			count(total.Copied), count(total.Present),
			count(total.Failed), count(total.Outstanding))
	}
	return tw.Flush()
}

// kindPlural holds the kinds whose plural is not the singular plus `s`.
//
// One entry, and it earns its map: appending `s` to every kind rendered
// `indexs`, which is the sort of detail a reader notices before any of the
// numbers and which makes them trust none of them.
var kindPlural = map[string]string{"index": "indexes"}

// kindLabel names a kind for a column of them.
//
// Plural where there is more than one, because `1 charts` is the same small
// wrongness in the other direction. The plural is formed here rather than
// carried in the API: these are a fixed set of English words, and a plural
// field on the wire would be a second thing to keep in step with the first.
func kindLabel(kind string, n int) string {
	if n == 1 {
		return kind
	}
	if p, ok := kindPlural[kind]; ok {
		return p
	}
	return kind + "s"
}

// renderWaves is the breakdown that makes an idle-looking transfer legible.
//
// The totals cannot do this. "519 outstanding, 2 in flight" mixes three
// populations that behave completely differently — runnable, waiting out a
// backoff, and GATED behind a wave that has not drained — and only the last one
// explains why a fleet with capacity to spare is running two jobs. Per wave it
// is immediate: wave 0 has two blobs left, waves 1 to 3 are full and cannot
// start until it finishes.
func renderWaves(w io.Writer, waves []v1.TransferWave) error {
	if len(waves) == 0 {
		return nil
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Waves")
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  WAVE\tKIND\tDONE\tRUNNING\tRUNNABLE\tWAITING\tBLOCKED\tFAILED\tCOPIED")
	for _, k := range waves {
		marker := "  "
		if k.Current {
			// A wave with work that can move right now. There can be more than
			// one: per-artifact readiness lets a manifest run before its wave
			// opens, and a repair sends blobs back to a wave already passed.
			marker = "> "
		}
		fmt.Fprintf(tw, "%s%d\t%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			marker, k.Wave, k.Kind, k.Done, k.Total,
			count(k.Running), count(k.Pending), count(k.Waiting),
			count(k.Blocked), count(k.Failed),
			bytesOfWave(k))
	}
	return tw.Flush()
}

// renderSkips says what the transfer did not move, and on whose word.
//
// "1976 of 1976 done" is four different claims wearing one word. Streaming a
// blob is proof it arrived; a mount is something the registry DID; a placement
// hit and a HEAD are things it or we SAID, and against a destination that
// answers about its whole storage rather than the repository asked about, the
// last one is worth nothing.
//
// The distinction is not academic — it is the answer to "why is it re-sending
// blobs that already succeeded?". The blobs a repair sends back are exactly the
// ones counted on an untrusted line here.
// renderNotTransferred accounts for the planned bytes that did not move.
//
// One heading with the reasons indented under it, rather than a flat line per
// reason. They were flat, and two adjacent lines whose labels differed while
// their meaning had to be read off a trailing clause is a layout that hides the
// structure: these are three members of one set, and the set is "planned, and
// not sent".
//
// Each reason gets a short qualifier because the label alone does not carry it
// — `Deduplicated` in particular says nothing about WHEN. The qualifier is a
// noun phrase, not a sentence: the tool states what happened and leaves the
// reader to draw conclusions.
func renderNotTransferred(w io.Writer, p v1.TransferProgress) {
	type row struct {
		label, amount, note string
	}
	var rows []row

	if int64Of(p.DedupeSkippedBytes) > 0 {
		rows = append(rows, row{
			label:  "Deduplicated",
			amount: humanBytes(p.DedupeSkippedBytes),
			note:   "not queued; already at target when planned",
		})
	}
	for _, k := range p.Skips {
		switch k.Reason {
		case "mounted":
			rows = append(rows, row{
				label:  "Mounted",
				amount: humanBytes(k.Bytes),
				note: fmt.Sprintf("%s; copied within the target registry",
					plural(k.Jobs, "job", "jobs")),
			})
		default:
			rows = append(rows, row{
				label:  "Present at target",
				amount: humanBytes(k.Bytes),
				note: fmt.Sprintf("%s; reported present, not re-checked",
					plural(k.Jobs, "job", "jobs")),
			})
		}
	}

	tw := newTabWriter(w)
	for _, r := range rows {
		fmt.Fprintf(tw, "    %s\t%s\t%s\n", r.label, r.amount, r.note)
	}
	_ = tw.Flush()
}

// activeWaves names the waves with work that can move right now.
//
// "1 of 3" while thirteen wave-0 jobs are running two lines below is a page
// disagreeing with itself, and the reader is right to distrust the rest of it.
// Where the transfer really is working on one wave this reads exactly as it
// did; where it is working on two, it says so.
func activeWaves(t *v1.Transfer) string {
	var active []string
	for _, k := range t.Waves {
		if k.Current {
			active = append(active, fmt.Sprint(k.Wave))
		}
	}
	if len(active) == 0 {
		// Nothing runnable: either finished, or everything is blocked or
		// backing off. The watermark is then the best available answer.
		return fmt.Sprint(t.CurrentWave)
	}
	return strings.Join(active, " and ")
}

// count renders a zero as a dash, so the eye lands on the numbers that are not
// zero — which in a wave table is nearly all of the information.
func count(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

// bytesOfWave shows copied against planned for that wave.
//
// Manifest waves are kilobytes and blob waves are the whole transfer, which is
// worth seeing side by side: it is why wave 0 takes hours and waves 1 to 3 take
// seconds once they open.
func bytesOfWave(k v1.TransferWave) string {
	if int64Of(k.PlannedBytes) == 0 {
		return "-"
	}
	return humanBytes(k.TransferredBytes) + "/" + humanBytes(k.PlannedBytes)
}

// describeSide names one end of a transfer both ways.
//
// On a single transfer there is room for both, and both are wanted: the
// configured name is what goes back into a command, the host and path is what
// goes into a registry browser.
func describeSide(name, resolved string) string {
	switch {
	case name == "":
		return resolved
	case resolved == "":
		return name
	default:
		return name + "  " + resolved
	}
}

func settled(s v1.TransferState) bool {
	switch s {
	case v1.TransferSucceeded, v1.TransferFailed, v1.TransferCancelled:
		return true
	default:
		return false
	}
}

// describeDelegated renders a transfer the registry performed.
//
// A separate rendering rather than the usual one with empty numbers, because
// the usual one is a page of measurements and every one of them would be a
// zero that means something other than zero. What this has instead is a state
// and a sentence saying whose bytes they were.
func describeDelegated(w io.Writer, t *v1.Transfer) error {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Delegated to the %s at %s.\n", strategyPhrase(t.Strategy),
		endpointName(t.TargetName, t.Target))
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	fmt.Fprintf(tw, "  Progress:\t%s\n", delegatedProgress(t))
	fmt.Fprintf(tw, "  Bytes:\t— we did not move them and cannot count them\n")
	if t.FailureReason != "" {
		fmt.Fprintf(tw, "  Detail:\t%s\n", t.FailureReason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  transferctl targets describe %s %s   what the registry is configured to do\n",
		t.Product, t.TargetName)
	return nil
}

func strategyPhrase(strategy string) string {
	switch strategy {
	case "mirror":
		return "registry's own mirror"
	case "proxy":
		return "registry's proxy cache"
	default:
		return "registry"
	}
}

// delegatedProgress is a STATE, never a percentage.
func delegatedProgress(t *v1.Transfer) string {
	switch t.State {
	case v1.TransferSyncing:
		return "the registry is syncing; there is no percentage to report"
	case v1.TransferSucceeded:
		return "complete, and the destination holds the digest we asked for"
	case v1.TransferDiverged:
		return "complete, and the destination holds a DIFFERENT digest — the upstream tag moved"
	case v1.TransferFailed:
		return "the sync did not complete"
	default:
		return strings.ToLower(string(t.State))
	}
}

func describeTransfer(w io.Writer, t *v1.Transfer, rates *rateTracker, watching bool) error {
	tw := newTabWriter(w)
	fmt.Fprintf(tw, "ID:\t%s\n", t.ID)
	fmt.Fprintf(tw, "Product:\t%s\n", t.Product)
	// The vendor's shortened spelling, the same one the listing shows. `describe`
	// printed the stored tag — `orb_25.7_mp2604_2131` where the listing said
	// `25.7_mp2604_2131` — so the two pages named the same package differently
	// and the reader had to work out that they agreed.
	fmt.Fprintf(tw, "Package:\t%s\n", transferTag(t))
	fmt.Fprintf(tw, "Source:\t%s\n", describeSide(t.SourceName, t.Source))
	fmt.Fprintf(tw, "Target:\t%s\n", describeSide(t.TargetName, t.Target))
	fmt.Fprintf(tw, "State:\t%s\n", strings.ToLower(string(t.State)))
	fmt.Fprintf(tw, "Priority:\t%d\n", t.Priority)
	// The WAVES table below is the honest account; this line is a one-glance
	// summary and has to agree with it. `current_wave` alone does not: it is a
	// watermark, and work legitimately runs above and below it — a manifest
	// promoted before its wave opened, or a blob sent back by a repair.
	fmt.Fprintf(tw, "Wave:\t%s of %d\n", activeWaves(t), t.MaxWave)
	if err := tw.Flush(); err != nil {
		return err
	}

	if delegated(t) {
		return describeDelegated(w, t)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Progress")
	pw := newTabWriter(w)
	p := t.Progress
	fmt.Fprintf(pw, "  Jobs:\t%d done, %d outstanding, %d failed (of %d planned, %.0f%%)\n",
		p.JobsDone, p.JobsOutstanding, p.JobsFailed, p.JobsPlanned, percentComplete(p))
	fmt.Fprintf(pw, "  In flight:\t%s%s\n", inFlight(p), quietFor(p))
	if p.JobsWaiting > 0 {
		fmt.Fprintf(pw, "  Waiting:\t%d in retry backoff\n", p.JobsWaiting)
	}
	if p.JobsBlocked > 0 {
		// The line that answers "why is only one job running". Outstanding
		// counts everything unfinished, and most of it is usually manifests
		// that CANNOT be leased yet — a manifest is pushed only once every blob
		// beneath it has landed. Without this the reader sees five hundred
		// outstanding, one running, and concludes the fleet is broken.
		fmt.Fprintf(pw, "  Blocked:\t%d awaiting referenced content\n", p.JobsBlocked)
	}
	if p.JobsRepaired > 0 {
		// Shown only when it happened, and stated as a fact rather than
		// explained. It is here because it is the only thing that makes the
		// done count fall.
		fmt.Fprintf(pw, "  Re-sent:\t%s; target reported content it did not hold\n",
			plural(p.JobsRepaired, "job", "jobs"))
	}
	// The release first, then the two ways its bytes are accounted for. Three
	// numbers that add up, rather than a fraction whose denominator nobody has:
	// how much of a release must cross the wire is settled one job at a time as
	// the transfer runs, so `X of Y` states a fact that does not exist yet.
	//
	// The percentage that used to sit on this line was progress by JOBS,
	// printed between two byte figures, and read as arithmetic on them: `0 B of
	// 63.7 GiB planned (100%)` cannot be true however carefully it is
	// explained. It is on the Jobs line, which is what it measures.
	// BOTH totals, adjacent, because they differ by design and a reader who
	// meets only one of them will eventually meet the other and conclude the
	// tool cannot count. The release is every distinct digest once; the planned
	// work counts a blob per repository it lands in, and a bundle's components
	// land in two.
	if content := int64Of(p.ContentBytes); content > 0 {
		fmt.Fprintf(pw, "  Release:\t%s\tdistinct content\n", humanBytesOf(content))
	}
	fmt.Fprintf(pw, "  Planned:\t%s\tcontent counted once per repository it lands in\n",
		humanBytesOf(packageBytes(p)))
	fmt.Fprintf(pw, "  Transferred:\t%s\n", humanBytes(p.BytesTransferred))
	// The TOTAL sits on the heading, aligned with Transferred above it, because
	// those two are the pair a reader compares: what crossed the network, and
	// what did not have to. The breakdown underneath answers the follow-up.
	fmt.Fprintf(pw, "  Not transferred:\t%s\n", humanBytes(p.SavedBytes))

	// The sub-block gets a tabwriter of its own. Sharing one would align its
	// three columns against the two above it, padding "Jobs:" to the width of
	// "Present at target" and opening a hole down the middle of the block.
	if err := pw.Flush(); err != nil {
		return err
	}
	renderNotTransferred(w, p)
	pw = newTabWriter(w)

	// The gap, named. Transferred plus not-transferred only reaches the release
	// once every job has settled, and until then the difference is work whose
	// outcome nobody knows: each job decides for itself, with a HEAD at the
	// destination, as it runs. Leaving that difference unexplained is what makes
	// three correct numbers look like they do not add up.
	if left := int64Of(p.OutstandingBytes); left > 0 {
		fmt.Fprintf(pw, "  Left:\t%s\tqueued; not yet copied or skipped\n",
			humanBytesOf(left))
	}

	if d, ok := elapsed(t); ok {
		fmt.Fprintf(pw, "  Elapsed:\t%s\n", humanDuration(d))
	}
	if !isSettled(t.State) {
		if d, ok := estimateAt(t, rates.smoothed); ok {
			fmt.Fprintf(pw, "  Remaining:\t~%s  (current rate)\n", humanDuration(d))
		} else if d, ok := estimate(t); ok {
			fmt.Fprintf(pw, "  Remaining:\t~%s  (average rate)\n", humanDuration(d))
		}
	}
	if err := pw.Flush(); err != nil {
		return err
	}

	if err := renderContent(w, t.Content); err != nil {
		return err
	}

	if err := renderWaves(w, t.Waves); err != nil {
		return err
	}

	if err := describeThroughput(w, t, rates, watching); err != nil {
		return err
	}

	if t.FailureReason != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Failed: %s\n", t.FailureReason)
		// The one place a next step earns its line: a failed transfer is not
		// finished with, it is waiting for somebody to decide whether the
		// cause is gone. Naming the command is the difference between that
		// decision being acted on and the transfer being re-created from
		// scratch, which re-copies what already landed.
		if p.JobsFailed > 0 {
			fmt.Fprintf(w, "Resume with: transferctl transfers retry %s\n", shortID(t.ID))
		}
	}

	// The one thing still worth saying in prose, and only when it is true: a
	// stalled transfer looks identical to a working one at a glance, and the
	// two causes need different actions. One line each, because this is a
	// diagnosis rather than an explanation of the table.
	if t.State == v1.TransferRunning && p.JobsOutstanding > 0 && p.JobsInFlight == 0 {
		fmt.Fprintln(w)
		if p.JobsWaiting > 0 {
			fmt.Fprintf(w, "Stalled: %d job(s) in retry backoff — `transfers failures %s`\n",
				p.JobsWaiting, shortID(t.ID))
		} else {
			fmt.Fprintf(w, "Stalled: no job running and none waiting — check a worker is up "+
				"(`transferctl health`)\n")
		}
	}

	// Failures are worth a pointer BEFORE the transfer settles, not only after.
	// A transfer that is 92% done with eighteen failures and twenty-two in
	// backoff still reads as `running`, and everything above this line reports
	// on the part that is working. The one line that matters is where to look
	// at the part that is not.
	//
	// It names the summary rather than the job listing on purpose: the listing
	// answers "which jobs", and when one cause is holding five hundred
	// manifests that answer is five hundred rows saying the same thing.
	if t.FailureReason == "" && (p.JobsFailed > 0 || p.JobsWaiting > 0) &&
		p.JobsInFlight > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Why jobs are failing: transferctl transfers failures %s\n", shortID(t.ID))
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
			fmt.Fprintf(tw, "  Recent:\t%s\tover the last %s; used for Remaining\n",
				humanRate(rates.smoothed), humanDuration(smoothingWindow))
			fmt.Fprintf(tw, "  Peak:\t%s\thighest seen while watching\n", humanRate(rates.peak))
		} else {
			fmt.Fprintf(tw, "  Current:\t-\tmeasured from the next sample\n")
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	throughputNote(w, t)
	return nil
}

// throughputNote explains a rate that looks like a collapse and is not.
//
// A manifest is about a kilobyte. When the in-flight work is manifests, the
// cost of each one is a ROUND TRIP, not its size, so the rate is bounded by
// latency × concurrency and lands in the low kilobytes per second on a link
// where blobs moved at hundreds. Nothing has gone wrong, nothing is
// misconfigured, and no amount of extra bandwidth would move it — but a reader
// watching 577 KiB/s become 1.2 KiB/s has every reason to think otherwise, and
// would go looking for a fault that is not there.
func throughputNote(w io.Writer, t *v1.Transfer) {
	kinds := map[string]bool{}
	for _, k := range t.Waves {
		if k.Running > 0 {
			kinds[k.Kind] = true
		}
	}
	if len(kinds) != 1 || !kinds["manifest"] {
		return
	}

	fmt.Fprintln(w, "  Manifest phase: bounded by round trips, not bandwidth.")
}

// quietFor reports how long the least active in-flight job has been silent.
//
// Only past a threshold, because on a healthy transfer it is noise: jobs report
// progress every couple of seconds and the answer is always "moments ago". Past
// it, it is the whole diagnosis — a worker holding a job that has not moved in
// hours is the one failure the lease machinery cannot see, because a lease is
// renewed by the worker being alive rather than by the job going anywhere.
func quietFor(p v1.TransferProgress) string {
	if p.JobsInFlight == 0 || p.QuietestInFlight == "" {
		return ""
	}
	since, err := time.Parse(time.RFC3339Nano, p.QuietestInFlight)
	if err != nil {
		return ""
	}

	quiet := time.Since(since)
	if quiet < quietThreshold {
		return ""
	}
	return fmt.Sprintf("; quietest silent for %s", humanDuration(quiet))
}

// quietThreshold is when silence stops being normal and starts being the
// answer. Comfortably above the progress interval, well below the worker's
// stall timeout, so it is a warning rather than a duplicate of it.
const quietThreshold = 2 * time.Minute

// inFlight renders concurrency in the terms a reader is asking about.
//
// "3 across 1 worker" answers the question directly; a bare count leaves them
// wondering whether the fleet or the queue is the limit.
func inFlight(p v1.TransferProgress) string {
	if p.JobsInFlight == 0 {
		return "nothing running"
	}
	return fmt.Sprintf("%s across %s",
		plural(p.JobsInFlight, "job", "jobs"), plural(p.Workers, "worker", "workers"))
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
//
// The failure count used to be parenthesised in here, which put it in a cell
// that widens the whole column and reads as a footnote to progress. It is its
// own column now — see failedJobs — because a failure is not a kind of
// progress.
func jobProgress(p v1.TransferProgress) string {
	if p.JobsPlanned == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", p.JobsDone, p.JobsPlanned)
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
