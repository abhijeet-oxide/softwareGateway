package store

import "sync"

// The job rollups of transfers that have finished, remembered.
//
// # What makes this safe, and it is not a TTL
//
// A transfer's rollup is the shape of its jobs: how many succeeded, how many
// are in flight, how many bytes moved. While the transfer is running that
// changes constantly. Once it has SETTLED it cannot change again - a
// succeeded, failed or cancelled transfer has no job left that anything will
// touch, so the twelve numbers are fixed for as long as the row exists.
//
// So this is not a cache with a staleness window to tune. It is a memo of a
// pure function of immutable data, and the invariant that makes it correct is
// one line: NOTHING IS EVER STORED FOR A TRANSFER THAT IS NOT TERMINAL. A
// running transfer is computed on every read, every time, so it can never be
// served a stale number.
//
// That is deliberately a different design from keeping the counts on the
// `transfers` row. Twenty-two statements in this package mutate `jobs` and a
// dozen mutate a transfer's state; maintained counters would have to be
// correct in every one of them, and the failure - a count wrong by the rows of
// one state - is a progress bar that reads 94% forever rather than a crash.
// Here there is no write path to miss, because there is no write path: a value
// only exists once the thing it describes has stopped moving.
// See docs/design/32-performance.md.
//
// # The one way a terminal transfer changes: it stops being one
//
// A retry REOPENS a failed transfer (recovery.go RetryTransfer), it runs
// again, and it settles again - with different numbers. A memo written before
// that, keyed on the id alone, would be served afterwards and be wrong.
//
// So an entry also carries the `updated_at` it was computed at, and is used
// only when the row still carries the same one. Every statement that changes a
// transfer's STATE sets `updated_at`, so reopening invalidates the memo and so
// does settling again - without any of those statements knowing this file
// exists. That is the point: the alternative is a Forget call in every reopen
// path, which is the kind of thing that is correct until somebody adds a
// thirteenth path.
//
// (The active-time sweeps in activetime.go deliberately do NOT set
// `updated_at`. They move `active_seconds` and `last_active_at`, neither of
// which is a job rollup, so a memo surviving them is correct.)
//
// Nothing else here can go wrong: a miss costs the query that would have run
// regardless, and a restart costs the first listing after it.
//
// # Per process, not per deployment
//
// Each Coordinator keeps its own, because a pure function of immutable data
// needs no coordination: two replicas computing the same rollup get the same
// answer. It is lost on restart, and the first listing afterwards pays what
// every listing used to.
type rollupCache struct {
	mu      sync.RWMutex
	entries map[string]rollupEntry
	// insertion order, for eviction. A transfer's rollup is a few dozen bytes
	// and retention bounds how many transfers exist at all, but "bounded by
	// something else's policy" is not bounded.
	order []string
	limit int
}

// rollupEntry is a remembered rollup and the row version it describes.
type rollupEntry struct {
	rollup    transferRollup
	updatedAt string
}

// transferRollup is the twelve columns the listing projects over `jobs`.
type transferRollup struct {
	SkippedBytes     int64
	JobsDone         int
	JobsFailed       int
	JobsOutstanding  int
	BytesTransferred int64
	JobsInFlight     int
	Workers          int
	JobsWaiting      int
	JobsBlocked      int
	JobsRepaired     int
	OutstandingBytes int64
	QuietestInFlight string
}

// defaultRollupCacheSize is transfers, not bytes.
//
// A listing page is twenty-five and the Downloads history is the table people
// page through, so a few thousand covers somebody working through a week of
// them without the map becoming a thing anybody has to think about.
const defaultRollupCacheSize = 4096

func newRollupCache(limit int) *rollupCache {
	if limit <= 0 {
		limit = defaultRollupCacheSize
	}
	return &rollupCache{entries: make(map[string]rollupEntry, limit), limit: limit}
}

// terminalTransferStates are the states from which a transfer's jobs can no
// longer change.
//
// Taken from the state machine in docs/design/10, and deliberately NOT the
// complement of "live": `paused` and `diverged` are not running, and are not
// finished either - a paused transfer resumes and a diverged one is settled by
// hand - so neither may be memoised.
var terminalTransferStates = map[string]bool{
	"succeeded": true,
	"failed":    true,
	"cancelled": true,
	"skipped":   true,
}

// get returns the remembered rollup for a transfer, if there is one for THIS
// version of the row and the transfer is still terminal.
//
// Both conditions are checked here rather than by the caller, because together
// they are the whole correctness argument and a caller that checked only one
// would reintroduce exactly the staleness this design rules out.
func (c *rollupCache) get(id, state, updatedAt string) (transferRollup, bool) {
	if c == nil || !terminalTransferStates[state] {
		return transferRollup{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[id]
	if !ok || e.updatedAt != updatedAt {
		return transferRollup{}, false
	}
	return e.rollup, true
}

// put remembers a rollup, but ONLY for a transfer that has finished.
//
// The state check is here rather than at the call sites on purpose: it is the
// invariant the whole file rests on, and a caller that forgets it would
// introduce exactly the staleness this design exists to make impossible.
func (c *rollupCache) put(id, state, updatedAt string, r transferRollup) {
	if c == nil || !terminalTransferStates[state] {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := rollupEntry{rollup: r, updatedAt: updatedAt}
	if _, exists := c.entries[id]; exists {
		c.entries[id] = entry
		return
	}
	// Oldest first, which for terminal transfers is near enough to
	// least-useful: a transfer that settled longest ago is the one least
	// likely to be on a page somebody is looking at.
	for len(c.order) >= c.limit && len(c.order) > 0 {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[id] = entry
	c.order = append(c.order, id)
}

// len is the number of remembered rollups, for the tests.
func (c *rollupCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
