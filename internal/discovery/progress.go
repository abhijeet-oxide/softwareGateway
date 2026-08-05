package discovery

import (
	"sync"
	"time"
)

// Phase names the stage a scan is in.
//
// Coarse on purpose. The value of progress here is answering "is it stuck, and
// on what?", and a caller cannot act on finer detail than this.
type Phase string

const (
	// PhaseIdle means no scan is running.
	PhaseIdle Phase = "IDLE"
	// PhaseEnumerating is the `/v2/_catalog` call, which happens once per scan
	// and is where a source that names no repositories spends its first — and
	// on a slow registry, its longest — minutes.
	PhaseEnumerating Phase = "ENUMERATING_REPOSITORIES"
	// PhaseListingTags is `tags/list` for one repository.
	PhaseListingTags Phase = "LISTING_TAGS"
	// PhaseResolving is fetching manifests for admitted tags. The bulk of a
	// large scan, and the phase worth showing a counter for.
	PhaseResolving Phase = "RESOLVING_TAGS"
)

// ScanProgress is a live snapshot of a scan in flight.
//
// It exists because a scan against a slow registry is indistinguishable from a
// hung one: `packages discover` blocked for two and a half minutes with a blank
// terminal and then reported a timeout, and nothing in between said whether it
// was working, waiting, or wedged. A progress snapshot is the difference
// between "be patient" and "something is wrong", and only the server can tell
// them apart.
type ScanProgress struct {
	Phase     Phase
	StartedAt time.Time

	// RepositoriesTotal is zero until enumeration finishes — which is itself
	// informative, because it means we are still waiting on `_catalog`.
	RepositoriesTotal int
	RepositoriesDone  int
	// RepositoriesInFlight is how many are being scanned RIGHT NOW.
	//
	// Without it a concurrent scan looks stalled for its first minute: with
	// sixteen repositories in flight and none finished, "0 of 48 done" is
	// accurate and reads exactly like nothing happening.
	RepositoriesInFlight int
	// CurrentRepository is whichever repository most recently STARTED. With
	// concurrency there is no single current one, so it is a hint about where
	// the scan has got to and not a position — callers should render it as
	// such.
	CurrentRepository string

	TagsListed   int
	TagsResolved int
	// TagsTotal is how many tags in the current repository survived the
	// filters, so a caller can render "17 of 240" rather than a number that
	// grows towards nothing in particular.
	TagsTotal int

	// Artifacts is manifests fetched across every tag so far.
	//
	// The counter that moves when nothing else does. A single tag with a large
	// artifact tree can take minutes, and without this the display sat on
	// "1/43 tags" while hundreds of requests completed successfully — which is
	// what "so many requests succeed but the CLI reports no progress" was.
	Artifacts int

	New    int
	Errors int
}

// Elapsed is how long the scan has been running.
func (p ScanProgress) Elapsed() time.Duration {
	if p.StartedAt.IsZero() {
		return 0
	}
	return time.Since(p.StartedAt)
}

// Running reports whether a scan is actually in flight.
func (p ScanProgress) Running() bool { return p.Phase != "" && p.Phase != PhaseIdle }

// progressTracker is the scanner's live counter.
//
// Guarded by its own mutex rather than the scanner's: it is read from HTTP
// handlers on other goroutines while the scan holds the scanner's lock for
// client construction, and sharing one lock would make a status request wait on
// a TLS handshake.
type progressTracker struct {
	mu   sync.Mutex
	prog ScanProgress
}

func (t *progressTracker) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prog = ScanProgress{Phase: PhaseEnumerating, StartedAt: time.Now()}
}

func (t *progressTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prog = ScanProgress{}
}

func (t *progressTracker) update(f func(*ScanProgress)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.prog)
}

// snapshot returns a copy, safe to hand to a caller on another goroutine.
func (t *progressTracker) snapshot() ScanProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prog
}
