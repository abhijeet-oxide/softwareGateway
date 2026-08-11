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
// throughput are computed from three facts the server already holds — bytes
// moved, bytes planned, and when the first job was leased — so they cannot
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
	moved := int64Of(t.Progress.BytesTransferred)
	if moved <= 0 {
		return 0, false
	}
	return float64(moved) / d.Seconds(), true
}

// estimate is how long the remaining bytes will take at the observed rate.
//
// Returns false when there is no rate to extrapolate from, which is the whole
// of the first few seconds of every transfer. Saying "unknown" then is the
// point rather than a gap: the alternative is an ETA derived from a rate
// measured over almost no time, which swings wildly and is believed anyway.
func estimate(t *v1.Transfer) (time.Duration, bool) {
	rate, ok := averageRate(t)
	if !ok || rate <= 0 {
		return 0, false
	}

	planned := int64Of(t.Progress.PlannedBytes)
	moved := int64Of(t.Progress.BytesTransferred)
	remaining := planned - moved
	if remaining <= 0 {
		return 0, false
	}
	return time.Duration(float64(remaining)/rate) * time.Second, true
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
