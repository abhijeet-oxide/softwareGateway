package artifactory

import (
	"context"
	"testing"
	"time"
)

// The sync this exists for: fourteen minutes, thirteen of them rediscovering
// one fact.
//
// A batch that times out has to make the NEXT batch smaller. Without that, ten
// workers each waited a full sixty-second timeout, each halved itself, and each
// discovered independently that this Xray could not answer fifty checksums -
// twenty-four times in one run.
func TestPacerShrinksTheBatchOnATimeout(t *testing.T) {
	p := newPacer(10, 50)
	if p.Batch() != 50 {
		t.Fatalf("batch starts at %d, want the configured 50", p.Batch())
	}

	p.Setback(false)
	if p.Batch() != 25 {
		t.Errorf("batch = %d after one timeout, want 25", p.Batch())
	}
	p.Setback(false)
	if p.Batch() != 12 {
		t.Errorf("batch = %d after two timeouts, want 12", p.Batch())
	}
}

// A scanner having a bad minute should get slower, not stop.
//
// A pacer that collapses to one request of one artifact turns a slow sync into
// one that cannot finish inside the claim window at all - 260 round trips where
// the timeouts it was avoiding cost fewer.
func TestPacerHasFloors(t *testing.T) {
	p := newPacer(10, 50)
	for range 20 {
		p.Setback(false)
	}
	if p.Batch() < minBatchFloor {
		t.Errorf("batch fell to %d, below the floor of %d", p.Batch(), minBatchFloor)
	}
	if p.InFlight() < minInFlightFloor {
		t.Errorf("concurrency fell to %d, below the floor of %d", p.InFlight(), minInFlightFloor)
	}
}

// A 429 and a timeout want opposite corrections.
//
// "Too many at once" is the concurrency; "too much at once" is the batch.
// Answering one with the other makes a sync slower without making it quieter.
func TestPacerAnswersRateLimitingWithConcurrency(t *testing.T) {
	p := newPacer(10, 50)
	p.Setback(true)

	if p.Batch() != 50 {
		t.Errorf("batch = %d after a 429; a rate limit is not a request that was too big", p.Batch())
	}
	if p.InFlight() != 5 {
		t.Errorf("inFlight = %d after a 429, want it halved to 5", p.InFlight())
	}
}

// A run of successes earns the speed back, so one bad minute does not slow
// every later sync against a scanner that has recovered.
func TestPacerGrowsBackAfterARunOfSuccesses(t *testing.T) {
	p := newPacer(10, 50)
	p.Setback(false)
	shrunk := p.Batch()

	// The growth backoff is what stops a recovering scanner being pushed
	// straight back into the wall, so a test that did not clear it would be
	// testing the backoff rather than the growth.
	p.mu.Lock()
	p.backoffUntil = time.Time{}
	p.mu.Unlock()

	for range winsBeforeGrowth {
		p.Win()
	}
	if p.Batch() <= shrunk {
		t.Errorf("batch = %d after %d clean requests, want it above %d",
			p.Batch(), winsBeforeGrowth, shrunk)
	}
}

// The allowance is enforced by a semaphore whose capacity moves, so shrinking
// never cancels a request that is already in flight.
func TestPacerAllowanceIsEnforced(t *testing.T) {
	p := newPacer(2, 50)
	ctx := t.Context()

	if err := p.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := p.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// The third has to wait. A context with a deadline is how that is observed
	// without the test hanging when the bound is broken.
	tight, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := p.Acquire(tight); err == nil {
		t.Error("a third request was admitted against a limit of two")
		p.Release()
	}

	p.Release()
	p.Release()
}
