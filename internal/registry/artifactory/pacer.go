package artifactory

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// The pacer: how hard to push one Xray, decided from what it has just done
// rather than from what the configuration guessed.
//
// # The sync this exists for
//
// A real release, 260 artifacts, asked in batches of fifty, ten in flight:
//
//	2:00:06  Sync started. 260 artifacts.
//	2:00:08  Requesting scan results for 157 images.
//	2:13:18  JFrog Xray timed out on 13 artifacts. Retrying as two smaller
//	         requests. (x24)
//	2:14:28  Sync finished.
//
// Fourteen minutes, thirteen of them spent discovering the same fact over and
// over. Every batch was sent at fifty, every batch waited its full sixty-second
// timeout, and every batch then halved itself - SEQUENTIALLY, so a batch that
// eventually needed four splits paid four timeouts one after another. Twenty-
// four of those messages is twenty-four independent rediscoveries that this
// Xray, right now, cannot answer fifty checksums in a minute.
//
// Nothing in the old design could learn. The batch size was a constant, the
// concurrency was a constant, and the only feedback path was a retry that threw
// its knowledge away as soon as it returned.
//
// # What this does instead
//
// Additive increase, multiplicative decrease, on TWO dials, shared by every
// request against one scanner:
//
//   - The BATCH shrinks when a request times out and grows back after a run of
//     successes. The first timeout costs one batch; the second batch is already
//     smaller, and so is the twentieth.
//   - The CONCURRENCY shrinks when the scanner rate-limits or times out, and
//     grows back the same way. This is what stops a struggling Xray being
//     answered with more load, which is the failure mode a fixed limit walks
//     into every time.
//
// Both floors are well above one. A scanner having a bad minute should get
// slower, not stop: a pacer that collapses to a single request in flight turns
// a slow sync into one that cannot finish inside the claim window at all.
//
// # Why the concurrency limit is a semaphore rather than errgroup.SetLimit
//
// SetLimit is fixed once the group exists, and the whole point is that it is
// not. The semaphore's capacity is max, and the pacer holds the difference
// between max and the current allowance as ballast - so shrinking is acquiring
// ballast and growing is releasing it, and no in-flight request is ever
// cancelled to make a limit true.
type pacer struct {
	sem *semaphore.Weighted

	// maxInFlight is the ceiling the operator configured. The pacer never goes
	// above it; it only ever declines to use all of it.
	maxInFlight int
	minInFlight int
	maxBatch    int
	minBatch    int

	mu sync.Mutex
	// inFlight is the allowance in force, and ballast is how much of the
	// semaphore is being held back to enforce it.
	inFlight int
	ballast  int64
	batch    int
	// wins counts consecutive successful requests since the last setback. The
	// dials move up on a run rather than on a single success, because one
	// request succeeding after a timeout says nothing - the batch that
	// succeeded was the small one.
	wins int
	// backoffUntil suppresses growth for a moment after a setback, so a scanner
	// that is recovering is not immediately pushed back into the wall.
	backoffUntil time.Time

	// shrinks and grows are counted for the transcript: "the batch settled at
	// 12" is the single most useful line in a slow sync's log, and without it
	// the only evidence is the wall clock.
	shrinks int
	grows   int
}

// Pacer tuning. Constants rather than configuration, because they are the
// SHAPE of the control loop and not a property of any deployment - the
// deployment's property is the ceiling, which is configured.
const (
	// winsBeforeGrowth is how many consecutive clean requests earn a step up.
	//
	// Four, because a batch is tens of artifacts: at ten in flight that is
	// roughly half a second of evidence, which is enough to say the scanner is
	// coping and short enough that a release recovers its speed inside one sync
	// rather than three.
	winsBeforeGrowth = 4
	// growthBackoff is how long after a setback the dials stay put.
	growthBackoff = 5 * time.Second
	// minBatchFloor is the smallest batch the pacer will ask for.
	//
	// Five, not one. One artifact per request is 260 round trips for a release
	// and would be slower than the timeouts it is avoiding; five keeps the
	// request count within reason while being small enough that a genuinely
	// struggling Xray can answer it.
	minBatchFloor = 5
	// minInFlightFloor is the smallest concurrency the pacer will settle at.
	minInFlightFloor = 2
)

// newPacer builds a pacer around an operator's ceilings.
func newPacer(maxInFlight, maxBatch int) *pacer {
	if maxInFlight <= 0 {
		maxInFlight = DefaultConcurrency
	}
	if maxBatch <= 0 {
		maxBatch = DefaultBatchSize
	}
	minInFlight := min(minInFlightFloor, maxInFlight)
	minBatch := min(minBatchFloor, maxBatch)

	return &pacer{
		sem:         semaphore.NewWeighted(int64(maxInFlight)),
		maxInFlight: maxInFlight,
		minInFlight: minInFlight,
		maxBatch:    maxBatch,
		minBatch:    minBatch,
		inFlight:    maxInFlight,
		batch:       maxBatch,
	}
}

// Batch is the size to ask for right now.
func (p *pacer) Batch() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batch
}

// InFlight is the concurrency allowance in force, for the transcript.
func (p *pacer) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight
}

// Acquire takes one slot, blocking until the scanner's allowance has room.
func (p *pacer) Acquire(ctx context.Context) error {
	return p.sem.Acquire(ctx, 1)
}

// Release returns a slot.
func (p *pacer) Release() { p.sem.Release(1) }

// Win records a request the scanner answered.
func (p *pacer) Win() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.wins++
	if p.wins < winsBeforeGrowth || time.Now().Before(p.backoffUntil) {
		return
	}
	p.wins = 0

	// Additive, and the batch first. Getting the batch back up is what removes
	// round trips; getting the concurrency back up is what adds load, and after
	// a scanner has just been struggling those are not equally safe bets.
	grew := false
	if p.batch < p.maxBatch {
		next := p.batch + p.batch/2
		if next <= p.batch {
			next = p.batch + 1
		}
		p.batch = min(next, p.maxBatch)
		grew = true
	} else if p.inFlight < p.maxInFlight {
		p.inFlight++
		p.releaseBallastLocked(1)
		grew = true
	}
	if grew {
		p.grows++
	}
}

// Setback records a request the scanner could not answer in time, or refused
// for being too much.
//
// `rateLimited` separates the two, because they call for different halves of
// the response. A 429 is "too many at once" and the concurrency is what is
// wrong; a timeout is "too much at once" and the batch is. Treating them as one
// thing halves the wrong dial and the sync gets slower without getting quieter.
func (p *pacer) Setback(rateLimited bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.wins = 0
	p.backoffUntil = time.Now().Add(growthBackoff)

	if rateLimited {
		if p.inFlight > p.minInFlight {
			next := max(p.inFlight/2, p.minInFlight)
			p.holdBallastLocked(int64(p.inFlight - next))
			p.inFlight = next
			p.shrinks++
		}
		return
	}

	if p.batch > p.minBatch {
		p.batch = max(p.batch/2, p.minBatch)
		p.shrinks++
		return
	}
	// The batch is already at its floor and the scanner is still timing out.
	// The remaining lever is asking for less at once overall, which is the same
	// statement made a different way.
	if p.inFlight > p.minInFlight {
		p.holdBallastLocked(1)
		p.inFlight--
		p.shrinks++
	}
}

// holdBallastLocked takes n slots out of circulation.
//
// TryAcquire rather than Acquire: this is called from a request's failure path
// and blocking there to enforce a smaller limit would hold up the very worker
// that is trying to report the problem. A slot that cannot be taken now is
// taken by the next setback, and the allowance below is honest about how much
// was actually withheld.
func (p *pacer) holdBallastLocked(n int64) {
	for i := int64(0); i < n; i++ {
		if !p.sem.TryAcquire(1) {
			return
		}
		p.ballast++
	}
}

// releaseBallastLocked puts n slots back into circulation.
func (p *pacer) releaseBallastLocked(n int64) {
	for i := int64(0); i < n && p.ballast > 0; i++ {
		p.sem.Release(1)
		p.ballast--
	}
}

// Settled describes where the dials ended up, for the sync transcript.
func (p *pacer) Settled() (batch, inFlight, shrinks, grows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batch, p.inFlight, p.shrinks, p.grows
}
