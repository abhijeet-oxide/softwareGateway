package api

import (
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
)

// What a comparison is doing, while it is doing it.
//
// # Why this exists at all
//
// A comparison is one synchronous POST that reads two registries from end to
// end. For a real release that is minutes, and the only thing a caller could
// show for those minutes was an animation — which is exactly what it would
// show for a comparison that had silently stopped. "Is this working?" was
// unanswerable, and it is the only question anybody has while waiting.
//
// # Why a side channel rather than a streamed response
//
// The response is a report, and the report is the thing every existing client
// wants; turning it into a stream would change the contract for `transferctl`
// and the client library to serve a progress bar. So the caller mints a token,
// sends it with the request, and polls a second endpoint while the first is in
// flight. Nothing that ignores the token behaves differently.
//
// # In memory, and deliberately
//
// Progress is worth nothing once the report arrives, so it is not worth a
// table, a migration or a write per manifest. The cost of holding it in memory
// is that a poll served by a DIFFERENT replica finds nothing — the caller then
// shows an indeterminate bar, which is where it started. That is the right
// trade for a number whose whole life is four minutes.

// compareProgress is the live state of one comparison.
type compareProgress struct {
	// Sides is one entry per end, keyed by "a"/"b", so the slower registry is
	// visible as itself rather than averaged into the other.
	Sides map[string]compare.Progress
	// StartedAt and UpdatedAt are what let a caller — and the server itself —
	// tell slow from stopped.
	StartedAt time.Time
	UpdatedAt time.Time
	// Done is set when the comparison finished, so a poll that arrives after
	// the report says so rather than reporting a stale position forever.
	Done bool
}

// compareTracker holds progress for comparisons in flight.
type compareTracker struct {
	mu sync.Mutex
	in map[string]*compareProgress
}

func newCompareTracker() *compareTracker {
	return &compareTracker{in: map[string]*compareProgress{}}
}

// maxTrackedComparisons bounds the map against a caller that mints tokens and
// never finishes a comparison. Old entries are dropped oldest-first.
const maxTrackedComparisons = 64

// trackedFor is how long a finished comparison's progress is kept, so a poll
// racing the response sees "done" rather than nothing.
const trackedFor = 30 * time.Second

// start registers a token and returns the reporter to hand to the comparison.
//
// A nil token yields a nil reporter: a caller that does not want progress must
// not pay for the bookkeeping, and the comparison itself checks for nil.
func (t *compareTracker) start(token string) compare.ProgressFunc {
	if token == "" {
		return nil
	}

	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	t.evictLocked(now)
	t.in[token] = &compareProgress{
		Sides:     map[string]compare.Progress{},
		StartedAt: now,
		UpdatedAt: now,
	}

	return func(p compare.Progress) {
		t.mu.Lock()
		defer t.mu.Unlock()
		entry, ok := t.in[token]
		if !ok {
			return
		}
		// Keyed by the END rather than by its label: the two sides of a
		// version comparison are the same place and carry the same label, and
		// keying by that would have the second overwrite the first.
		entry.Sides[p.Key] = p
		entry.UpdatedAt = time.Now()
	}
}

// finish marks a comparison done and leaves it briefly readable.
func (t *compareTracker) finish(token string) {
	if token == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.in[token]; ok {
		entry.Done = true
		entry.UpdatedAt = time.Now()
	}
}

// read returns a copy of a comparison's progress.
func (t *compareTracker) read(token string) (compareProgress, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.in[token]
	if !ok {
		return compareProgress{}, false
	}
	out := compareProgress{
		Sides:     make(map[string]compare.Progress, len(entry.Sides)),
		StartedAt: entry.StartedAt,
		UpdatedAt: entry.UpdatedAt,
		Done:      entry.Done,
	}
	for k, v := range entry.Sides {
		out.Sides[k] = v
	}
	return out, true
}

// evictLocked drops finished and abandoned entries. Called on start, because a
// map that is only added to is a leak with a slow fuse.
func (t *compareTracker) evictLocked(now time.Time) {
	for token, entry := range t.in {
		if entry.Done && now.Sub(entry.UpdatedAt) > trackedFor {
			delete(t.in, token)
		}
	}
	for len(t.in) >= maxTrackedComparisons {
		var oldest string
		var at time.Time
		for token, entry := range t.in {
			if oldest == "" || entry.StartedAt.Before(at) {
				oldest, at = token, entry.StartedAt
			}
		}
		if oldest == "" {
			return
		}
		delete(t.in, oldest)
	}
}
