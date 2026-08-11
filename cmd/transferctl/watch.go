package main

import (
	"context"
	"fmt"
	"io"
	"time"
)

// `--watch`, and the rate samples only a watcher can take.
//
// # Why current and peak throughput live here rather than on the server
//
// Average throughput is derivable from state the server already holds: bytes
// moved over time elapsed. CURRENT is not — it needs two observations and the
// gap between them, and the server keeps no time series. Storing one would
// mean a maintained counter, which is the thing invariant I6 exists to
// prevent, for a number that is only ever looked at live.
//
// So the watcher measures it: each poll subtracts the previous sample and
// divides by the interval. Peak is the highest current seen during the watch,
// which is why it is reported as "peak while watching" rather than as a
// property of the transfer. Both are honest about their scope.

// DefaultWatchInterval is how often a watch re-reads.
//
// Two seconds: fast enough to feel live, slow enough that a rate computed over
// the gap is a rate rather than noise, and slow enough that forty terminals
// watching do not become a load problem of their own.
const DefaultWatchInterval = 2 * time.Second

// smoothingWindow is roughly how long the smoothed rate takes to follow a
// change in the real one.
//
// Thirty seconds, which is a compromise between two failure modes that are both
// worse. A raw two-second sample is far too jumpy to extrapolate an ETA from —
// one large blob finishing makes it spike, and an ETA that swings by an hour
// between redraws is not believed, or worse, is. The cumulative average has the
// opposite problem: it is dragged by the whole history and takes longer to
// forget a slow start than the slow start itself lasted.
const smoothingWindow = 30 * time.Second

// rateTracker turns successive byte totals into a throughput.
type rateTracker struct {
	lastBytes int64
	lastAt    time.Time
	current   float64
	peak      float64
	// smoothed is an exponentially weighted average of the samples, and it is
	// the one an ETA should extrapolate from: responsive enough to notice a
	// second worker starting, steady enough that the number stays readable.
	smoothed float64
}

// observe records a sample and returns the rate since the previous one.
//
// The first sample establishes a baseline and reports nothing: a rate needs an
// interval, and inventing one from a single reading would report the
// transfer's whole history as if it had happened in the last two seconds.
func (r *rateTracker) observe(bytes int64, at time.Time) {
	defer func() {
		r.lastBytes, r.lastAt = bytes, at
	}()

	if r.lastAt.IsZero() {
		return
	}
	seconds := at.Sub(r.lastAt).Seconds()
	if seconds <= 0 {
		return
	}

	moved := bytes - r.lastBytes
	if moved < 0 {
		// Bytes went backwards, which happens when a job is retried and its
		// counter resets. Not a negative rate — just a sample to skip.
		return
	}

	r.current = float64(moved) / seconds
	if r.current > r.peak {
		r.peak = r.current
	}

	// Weighted by the ACTUAL gap rather than by a fixed per-sample constant, so
	// `--interval 10s` and `--interval 1s` describe the same thirty seconds of
	// history. A poll that arrives late — a slow Coordinator, a laptop that
	// slept — then counts for what it covers instead of for one tick.
	if r.smoothed == 0 {
		r.smoothed = r.current
		return
	}
	alpha := seconds / (seconds + smoothingWindow.Seconds())
	r.smoothed += alpha * (r.current - r.smoothed)
}

// watchLoop re-renders until the context is cancelled or `done` says to stop.
//
// render is called once immediately, so a watch shows something before the
// first interval elapses rather than an empty screen for two seconds.
func watchLoop(
	ctx context.Context, w io.Writer, interval time.Duration,
	render func(io.Writer) (done bool, err error),
) error {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		clearScreen(w)
		done, err := render(w)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			// Ctrl-C during a watch is how a watch ends. Reporting it as a
			// failure would make every use of --watch exit non-zero, which a
			// script cannot distinguish from a transfer that actually failed.
			return nil
		case <-ticker.C:
		}
	}
}

// clearScreen redraws in place on a terminal, and appends elsewhere.
//
// Piping `--watch` into a file or another command is a legitimate thing to do,
// and escape sequences in that stream are noise. A rule and a timestamp make
// successive snapshots readable instead. isTerminal is the one in progress.go,
// which already draws a live line and had to answer the same question.
func clearScreen(w io.Writer) {
	if isTerminal(w) {
		fmt.Fprint(w, "\033[H\033[2J")
		return
	}
	fmt.Fprintf(w, "\n--- %s ---\n", time.Now().Format(time.RFC3339))
}
