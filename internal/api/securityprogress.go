package api

import (
	"sync"
	"time"
)

// What a security retrieval is doing, while it is doing it.
//
// # Why this is not a spinner
//
// Retrieving a release's posture is a scanner query per batch of artifacts
// across a few hundred artifacts, and for a large release that is tens of
// seconds. A spinner says the same thing for those tens of seconds as it says
// for a request that has silently stopped, and "is this working?" is the only
// question anybody has while waiting.
//
// So the work reports what it is doing - resolving, reading the cache, fetching
// from Xray, correlating, comparing - and the interface says it in words. The
// difference between "Loading…" and "Fetching 42 of 157 from JFrog Xray" is the
// difference between a user who waits and a user who reloads the page, which
// starts the whole retrieval again.
//
// # Why a side channel, and why in memory
//
// Same argument as the comparison tracker beside it: the response is the
// report, and turning it into a stream would change the contract for every
// existing client to serve a progress bar. The caller mints a token, sends it
// with the request, and polls while the first request is open. Progress is
// worth nothing once the answer arrives, so it is not worth a table - the cost
// is that a poll served by a different replica finds nothing and the client
// falls back to an indeterminate bar, which is where it started.

// securityStage is one phase's position.
type securityStage struct {
	Name        string
	Done, Total int
}

// securityProgressEntry is the live state of one retrieval.
type securityProgressEntry struct {
	Stages []securityStage
	// Notes are the sentences that are not a position - "Asking JFrog Xray
	// about 157 artifacts in 4 requests."
	Notes     []string
	StartedAt time.Time
	UpdatedAt time.Time
	Done      bool
}

// securityTracker holds progress for retrievals in flight.
type securityTracker struct {
	mu sync.Mutex
	in map[string]*securityProgressEntry
}

func newSecurityTracker() *securityTracker {
	return &securityTracker{in: map[string]*securityProgressEntry{}}
}

const (
	// maxTrackedRetrievals bounds the map against a caller that mints tokens
	// and never finishes a retrieval.
	maxTrackedRetrievals = 64
	// securityTrackedFor keeps a finished retrieval briefly readable, so a poll
	// racing the response sees "done" rather than nothing.
	securityTrackedFor = 30 * time.Second
	// maxTrackedNotes bounds one retrieval's notes. A hundred batches is a
	// hundred notes, and the interface shows the last few.
	maxTrackedNotes = 20
)

// start registers a token and returns the reporter to hand to the retrieval.
//
// A nil token yields a nil reporter, and every caller of security.Progress is
// nil-safe: a caller that does not want progress must not pay for the
// bookkeeping.
func (t *securityTracker) start(token string) *trackedProgress {
	if token == "" {
		return nil
	}

	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	t.evictLocked(now)
	t.in[token] = &securityProgressEntry{StartedAt: now, UpdatedAt: now}
	return &trackedProgress{tracker: t, token: token}
}

// finish marks a retrieval done.
func (t *securityTracker) finish(token string) {
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

// read returns a copy of a retrieval's progress.
func (t *securityTracker) read(token string) (securityProgressEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.in[token]
	if !ok {
		return securityProgressEntry{}, false
	}
	out := securityProgressEntry{
		Stages:    sortStages(entry.Stages),
		Notes:     append([]string(nil), entry.Notes...),
		StartedAt: entry.StartedAt,
		UpdatedAt: entry.UpdatedAt,
		Done:      entry.Done,
	}
	return out, true
}

func (t *securityTracker) evictLocked(now time.Time) {
	for token, entry := range t.in {
		if entry.Done && now.Sub(entry.UpdatedAt) > securityTrackedFor {
			delete(t.in, token)
		}
	}
	for len(t.in) >= maxTrackedRetrievals {
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

// trackedProgress implements security.Progress against one token.
//
// Safe for concurrent use, which is not optional: the provider fetches in
// parallel and every batch reports as it lands.
type trackedProgress struct {
	tracker *securityTracker
	token   string
}

// Stage records a phase's position, replacing any previous position for the
// same phase.
//
// Replacing rather than appending, because "fetching 3 of 157" and "fetching 4
// of 157" are the same fact at two moments, and a list that kept both would
// grow by one entry per batch and render as a wall.
func (p *trackedProgress) Stage(name string, done, total int) {
	if p == nil {
		return
	}
	p.tracker.mu.Lock()
	defer p.tracker.mu.Unlock()

	entry, ok := p.tracker.in[p.token]
	if !ok {
		return
	}
	for i := range entry.Stages {
		if entry.Stages[i].Name == name {
			// Never let a position go backwards. Two sides of a comparison
			// report into one token concurrently, and the slower one's earlier
			// number would otherwise overwrite the faster one's later one and
			// make the bar stutter.
			if done >= entry.Stages[i].Done {
				entry.Stages[i].Done = done
			}
			if total > entry.Stages[i].Total {
				entry.Stages[i].Total = total
			}
			entry.UpdatedAt = time.Now()
			return
		}
	}
	entry.Stages = append(entry.Stages, securityStage{Name: name, Done: done, Total: total})
	entry.UpdatedAt = time.Now()
}

// Note records something worth saying that is not a position.
func (p *trackedProgress) Note(s string) {
	if p == nil || s == "" {
		return
	}
	p.tracker.mu.Lock()
	defer p.tracker.mu.Unlock()

	entry, ok := p.tracker.in[p.token]
	if !ok {
		return
	}
	if len(entry.Notes) >= maxTrackedNotes {
		entry.Notes = entry.Notes[1:]
	}
	entry.Notes = append(entry.Notes, s)
	entry.UpdatedAt = time.Now()
}
