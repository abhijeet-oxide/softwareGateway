package main

import (
	"fmt"
	"strconv"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The numbers a person actually watches: how far along, how fast, how long.
//
// # Why none of them is stored
//
// Progress is a rollup over jobs, never a maintained counter (invariant I6),
// and the same applies to everything derived from it. Elapsed, percentage and
// throughput are computed from three facts the server already holds - bytes
// moved, bytes planned, and when the first job was leased - so they cannot
// drift from the jobs they describe.
//
// # What is deliberately absent
//
// An ETA when there is nothing to base one on. A transfer that has moved no
// bytes has no observed rate, and the honest output is "unknown" rather than a
// number computed from a guess. docs/design/05 §7 says it plainly: a
// confidently wrong estimate is worse than none.

// percentComplete is progress by JOBS rather than by bytes.
//
// Jobs is the measure that reaches 100%. Bytes does not: a job skipped at the
// worker because the destination already had the content moves zero bytes and
// still counts in plannedBytes, so a bytes-based percentage on a heavily
// deduplicated transfer stops short of complete and looks stuck.
func percentComplete(p v1.TransferProgress) float64 {
	if p.JobsPlanned == 0 {
		return 0
	}
	pct := float64(p.JobsDone) / float64(p.JobsPlanned) * 100
	if pct > 100 {
		return 100
	}
	return pct
}

// elapsed is how long the transfer has been MOVING.
//
// Measured from the first lease, not from the request: a transfer that waited
// an hour for a worker did not spend an hour transferring, and averaging over
// that wait would report a throughput several times below the truth.
func elapsed(t *v1.Transfer) (time.Duration, bool) {
	start, ok := parseTime(t.StartedAt)
	if !ok {
		return 0, false
	}

	end := time.Now()
	if done, ok := parseTime(t.CompletedAt); ok {
		end = done
	}
	if d := end.Sub(start); d > 0 {
		return d, true
	}
	return 0, false
}

// averageRate is bytes moved per second over the whole transfer.
func averageRate(t *v1.Transfer) (float64, bool) {
	d, ok := elapsed(t)
	if !ok {
		return 0, false
	}
	moved := int64Of(progressOf(t).BytesTransferred)
	if moved <= 0 {
		return 0, false
	}
	return float64(moved) / d.Seconds(), true
}

// estimate is how long the remaining work will take at the observed rates.
//
// A zero byte rate is passed through rather than refused. It means nothing has
// crossed the wire - the ordinary state of a transfer whose content is already
// at the destination - and that transfer still has jobs completing at a
// measurable rate, which is the thing actually governing when it finishes. The
// first few seconds of every transfer, where neither rate exists yet, still
// answer "unknown": an estimate from a rate measured over almost no time swings
// wildly and is believed anyway.
func estimate(t *v1.Transfer) (time.Duration, bool) {
	rate, _ := averageRate(t)
	return estimateAt(t, rate)
}

// estimateAt extrapolates the remaining bytes at a rate the caller supplies.
//
// # Why a caller ever supplies one
//
// The average is a CUMULATIVE average, and that makes it slow to react by
// construction: it carries the whole history, so a transfer that spent ten
// minutes crawling and then got ten times faster needs well over an hour of the
// new speed before the average is within 10% of it. Someone who has just added
// a worker, or bypassed a proxy, is watching precisely to find out whether it
// helped - and would be reading a number that mostly reflects the period before
// they changed anything.
//
// A watcher takes samples, so it can do better: it passes the smoothed rate
// from its rateTracker, which follows a real change within about half a minute.
// Without --watch there is nothing but the average, and the average is honest
// about what it is.
func estimateAt(t *v1.Transfer, rate float64) (time.Duration, bool) {
	byBytes, hasBytes := byteEstimate(t, rate)
	byJobs, hasJobs := jobEstimate(t)

	// THE LATER OF THE TWO, because both are lower bounds and the transfer is
	// not finished until both are satisfied. See jobEstimate for why bytes
	// alone is not enough.
	switch {
	case hasBytes && hasJobs:
		return max(byBytes, byJobs), true
	case hasBytes:
		return byBytes, true
	case hasJobs:
		return byJobs, true
	default:
		return 0, false
	}
}

// byteEstimate is how long the bytes still to move will take at a given rate.
func byteEstimate(t *v1.Transfer, rate float64) (time.Duration, bool) {
	if rate <= 0 {
		return 0, false
	}

	remaining := movableBytes(t)
	if remaining <= 0 {
		return 0, false
	}
	// Scaled INSIDE the conversion. `time.Duration(x) * time.Second` truncates
	// x to an integer first, so anything under a second became zero and printed
	// as "-" - an estimate of "unknown" for the one case where it is most
	// certain.
	return time.Duration(float64(remaining) / rate * float64(time.Second)), true
}

// jobEstimate is how long the remaining JOBS will take at the rate jobs have
// been completing.
//
// # Why bytes alone cannot answer this
//
// A transfer whose content is already at the destination moves almost nothing
// and is not therefore almost finished: each of its remaining jobs still costs
// a HEAD, a decision and a row, so its duration is bounded by ROUND TRIPS ×
// jobs, and bytes do not appear in that product at all.
//
// Estimating such a transfer on bytes said `<1s` with 794 jobs left to run -
// the same failure as the hours-long estimate it replaced, in the opposite
// direction and from the same cause: one quantity being used to describe work
// whose cost is governed by another.
//
// Cumulative rather than smoothed, and that is not a compromise here. Job
// throughput is dominated by concurrency and latency, both of which hold steady
// across a run, where byte throughput swings with the size of whatever is in
// flight - which is exactly why the byte estimate takes a live rate and this
// one does not need to.
func jobEstimate(t *v1.Transfer) (time.Duration, bool) {
	outstanding := progressOf(t).JobsOutstanding
	if outstanding <= 0 || progressOf(t).JobsDone <= 0 {
		return 0, false
	}

	d, ok := elapsed(t)
	if !ok {
		return 0, false
	}

	perSecond := float64(progressOf(t).JobsDone) / d.Seconds()
	if perSecond <= 0 {
		return 0, false
	}
	return time.Duration(float64(outstanding) / perSecond * float64(time.Second)), true
}

// remainingBytes is what is actually left to move.
//
// PLANNED MINUS TRANSFERRED IS NOT THAT, and the gap is not small. That
// difference counts every byte that will never move: content the destination
// already had, blobs the registry relocated internally, work deduplicated away
// at plan time. A transfer 98% done with 43 manifest jobs left - about 233 KiB
// of real work - reported a hundred megabytes remaining and, at the kilobyte
// rate of a manifest phase, an ETA of eleven hours. Both numbers were arithmetic
// on the wrong quantity.
//
// The server reports the real figure: the size of every outstanding job, less
// what each has already sent. The old subtraction survives only as a fallback
// for a Coordinator too old to send it.
// ABSENT, not zero. The fallback is for a Coordinator too old to send the
// figure at all, and testing it with `> 0` fired on every transfer that had
// genuinely finished its bytes: outstanding really was zero, the subtraction
// then returned the whole plan minus the little that had moved, and a transfer
// with nothing left to do reported thousands of hours remaining.
func remainingBytes(t *v1.Transfer) int64 {
	if progressOf(t).OutstandingBytes != "" {
		return int64Of(progressOf(t).OutstandingBytes)
	}
	return int64Of(progressOf(t).PlannedBytes) - int64Of(progressOf(t).BytesTransferred)
}

// packageBytes is the size of the RELEASE, which is not what was planned.
//
// # Two quantities that were being used as one
//
// `plannedBytes` is the work the plan QUEUED. Content the destination already
// held when the transfer was planned is never queued at all, so it is not in
// that number - it is in `dedupeSkippedBytes`, and the release is the sum of
// the two.
//
// Using the planned figure as a denominator produced a row that could not be
// true: `COPIED 490 KiB/29.8 GiB · SAVED 63.7 GiB`, a transfer that had saved
// more than the whole of what it was apparently moving. Both numbers were
// right and they were measured against different things.
//
// Against the release, the arithmetic closes: what crosses the wire plus what
// did not have to equals the release, once the outstanding work is done.
func packageBytes(p v1.TransferProgress) int64 {
	return int64Of(p.PlannedBytes) + int64Of(p.DedupeSkippedBytes)
}

// movableBytes is how much of what is left is expected to CROSS THE WIRE.
//
// # The estimate this exists to stop
//
// Outstanding bytes is the size of the work remaining, and on a re-transfer
// almost none of that work is a transfer. The second run of a release finds its
// content already at the destination and skips it - thousands of jobs, sixty
// gigabytes nominally outstanding, and perhaps eight megabytes that genuinely
// have to move. Dividing sixty gigabytes by a rate measured while moving eight
// megabytes produced the reported symptom: hours of ETA on a transfer that
// finished in under a minute, and the arithmetic was correct in both halves.
//
// So the remaining work is scaled by the proportion of the work ALREADY DONE
// that turned out to need moving. A transfer that has skipped 95% of what it
// has looked at is estimated on the assumption it will go on skipping about 95%
// - which is what the plan and the destination both imply, and what the first
// version assumed to be 0%.
//
// # Why this is measured rather than asked for
//
// Nothing knows in advance which blobs the destination holds; that is settled
// per job, by a HEAD or a mount, as the transfer runs. The observed ratio is
// the only evidence there is, it improves as the run proceeds, and before there
// is any the honest answer is the unscaled figure rather than a guess in either
// direction.
func movableBytes(t *v1.Transfer) int64 {
	outstanding := remainingBytes(t)
	if outstanding <= 0 {
		return 0
	}

	planned := int64Of(progressOf(t).PlannedBytes)
	moved := int64Of(progressOf(t).BytesTransferred)

	// What the completed jobs were PLANNED to weigh, against what they actually
	// sent. Both sides have to come from the same population or the ratio is
	// between two unrelated quantities.
	settled := planned - outstanding
	if settled <= 0 || moved <= 0 {
		// Nothing has finished yet, or nothing has moved. No ratio to apply,
		// and inventing one in either direction would be worse than the
		// unscaled number a reader can at least reason about.
		return outstanding
	}

	moving := float64(moved) / float64(settled)
	if moving >= 1 {
		// Everything looked at so far had to move. Common, and the case where
		// the old arithmetic was already right.
		return outstanding
	}
	return int64(float64(outstanding) * moving)
}

// humanRate renders bytes per second.
func humanRate(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "-"
	}
	return humanBytes(v1.Int64String(strconv.FormatInt(int64(bytesPerSecond), 10))) + "/s"
}

// humanDuration renders a duration at a granularity a person reads.
//
// Seconds below a minute, minutes and seconds below an hour, hours and minutes
// above. Sub-second precision on a transfer measured in minutes is noise.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// parseTime reads a timestamp the server produced.
//
// Two layouts, because the two databases store timestamps differently and the
// value reaches here as whichever the deployment uses. A value that parses as
// neither is treated as absent rather than as an error: a missing elapsed time
// is a cosmetic gap, and refusing to render a whole transfer over it would not
// be.
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999Z",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func int64Of(v v1.Int64String) int64 {
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// progressOf is a transfer's job rollup, or an empty one.
//
// The field is ABSENT on a listing asked for with `view=summary`, which this
// CLI never asks for - every command here wants the numbers. So a nil means an
// unusual or older server rather than a view this code chose, and an empty
// rollup renders as dashes: the honest thing to show for figures nobody sent.
func progressOf(t *v1.Transfer) v1.TransferProgress {
	if t == nil || t.Progress == nil {
		return v1.TransferProgress{}
	}
	return *t.Progress
}
