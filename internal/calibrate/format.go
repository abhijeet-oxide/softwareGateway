package calibrate

import (
	"fmt"
	"time"
)

// Rendering for the strings that go INTO the report - the evidence attached to
// a suggestion is prose, and prose containing 4823419 is not evidence anybody
// reads. The CLI formats the tables; this formats the sentences.

// humanSize renders bytes in binary units.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// humanRate renders bytes per second.
func humanRate(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "nothing"
	}
	return humanSize(int64(bytesPerSecond)) + "/s"
}

// humanDuration renders a duration at a granularity a person reads: sub-second
// precision on a transfer measured in hours is noise, and milliseconds matter
// for a round trip.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
