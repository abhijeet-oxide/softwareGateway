package store

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// How long a transfer has actually spent downloading.
//
// # The number this exists to replace
//
// Elapsed was `completed_at - started_at`. That is wall clock, and it is the
// right answer only when the fleet was working for the whole of it - which the
// queue is explicitly built not to require. A worker that crashed at midnight
// and came back at noon left a transfer reporting twelve hours, of which
// twenty minutes were spent moving bytes, and a throughput derived from it that
// was wrong by a factor of thirty-six.
//
// # Why it is accrued rather than computed
//
// The reconstruction anybody reaches for first - merge the jobs'
// [started_at, completed_at] intervals - cannot work, and it fails on exactly
// the case this is for. `jobs.started_at` is written with COALESCE on the first
// lease and never reset (see startTransfers, which does the same for the
// transfer and says why), so a job leased at midnight, orphaned by the crash,
// re-leased at noon and finished at 12:05 carries an interval spanning the
// outage. Merging those recovers wall clock, slowly.
//
// So the fact has to be recorded while it is observable, which means sampling:
// every sweep adds the time since the previous sweep to each transfer that has
// a job LEASED at that instant.
//
// # What the measure actually means
//
// "How long was there work of this transfer in the hands of a worker." That is
// the honest reading of "spent downloading": it counts a single 8 GB blob
// occupying one worker for half an hour, it counts none of the night the fleet
// was down, and it does not multiply by concurrency - sixteen workers for a
// minute is a minute, because a person waited a minute.

// activeSweepMaxGap bounds what one pass will believe.
//
// A gap longer than this is not accrued at all. The case is the Coordinator
// having been down: on restart the anchor is stale by the whole outage, and
// nothing was running for any of it. Under-counting a period nobody observed is
// the only honest option - adding it back is precisely how wall clock came to
// be the number on the page.
//
// Expressed as a multiple of the sweep interval so the two cannot drift apart:
// four passes, so a sweep delayed by a slow database or a garbage collection
// costs nothing, and a real outage is excluded within two minutes.
const activeSweepGapFactor = 4

// AccrueActiveTime records the time this pass observed, and re-anchors the rest.
//
// `since` is how long ago the previous pass ran - the sweep's own interval. It
// is passed in rather than stored because it is a property of the caller's
// loop, and a loop that changes its interval must not have its history
// reinterpreted.
//
// Returns how many transfers were credited, for the sweep's log line.
//
// Idempotent in the way every other recovery sweep here is: running it twice in
// quick succession credits the second pass with the near-zero gap since the
// first, not with the interval again.
func (p *Packages) AccrueActiveTime(ctx context.Context, since time.Duration) (int, error) {
	if since <= 0 {
		since = 30 * time.Second
	}
	maxGap := strconv.FormatFloat((since * activeSweepGapFactor).Seconds(), 'f', -1, 64)

	now := p.dialect.Now()
	sinceAnchor := p.dialect.SecondsBetween("last_active_at", now)
	leased := `EXISTS (SELECT 1 FROM jobs j
	                    WHERE j.transfer_id = transfers.id AND j.state = 'leased')`

	// 1. THE ACCRUAL. Live, anchored, plausible, and with something in a
	//    worker's hands right now.
	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET active_seconds = active_seconds + `+sinceAnchor+`,
		       last_active_at = `+now+`,
		       updated_at     = `+now+`
		 WHERE state IN (`+liveTransferStates+`)
		   AND last_active_at IS NOT NULL
		   AND `+sinceAnchor+` >= 0
		   AND `+sinceAnchor+` <= `+maxGap+`
		   AND `+leased))
	if err != nil {
		return 0, fmt.Errorf("accrue active transfer time: %w", err)
	}
	credited, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("accrue active transfer time: %w", err)
	}

	// 2. RE-ANCHOR everything else that is live: nothing in flight, no anchor
	//    yet, or a gap too old to believe. All three mean "start measuring from
	//    now", and none of them mean "add that gap".
	//
	//    After step 1 the credited rows have an anchor of now and a job leased,
	//    so they do not match this and cannot be credited twice.
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET last_active_at = `+now+`
		 WHERE state IN (`+liveTransferStates+`)
		   AND (last_active_at IS NULL
		        OR `+sinceAnchor+` < 0
		        OR `+sinceAnchor+` > `+maxGap+`
		        OR NOT `+leased+`)`)); err != nil {
		return 0, fmt.Errorf("re-anchor active transfer time: %w", err)
	}

	// 3. THE LAST FRAGMENT of a transfer that settled since the previous pass.
	//
	//    Without this a download that finished in four seconds reports none,
	//    because no sweep ever caught it with a job in flight - and a
	//    long one loses however much of its final pass fell after the last
	//    sweep. Measured to `completed_at` rather than to now, so the time the
	//    row sat settled waiting to be noticed is not counted.
	//
	//    The anchor is then cleared, which is what makes this run once: a
	//    settled transfer with no anchor matches nothing here ever again.
	toCompletion := p.dialect.SecondsBetween("last_active_at", "completed_at")
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET active_seconds = active_seconds + `+toCompletion+`,
		       last_active_at = NULL
		 WHERE state NOT IN (`+liveTransferStates+`)
		   AND last_active_at IS NOT NULL
		   AND completed_at IS NOT NULL
		   AND `+toCompletion+` >= 0
		   AND `+toCompletion+` <= `+maxGap)); err != nil {
		return 0, fmt.Errorf("close out active transfer time: %w", err)
	}

	// 4. And drop the anchor on a settled transfer the rule above could not
	//    account for - no completion time, or a gap too old to believe. It has
	//    stopped; leaving it anchored would leave step 3 examining it forever.
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET last_active_at = NULL
		 WHERE state NOT IN (`+liveTransferStates+`)
		   AND last_active_at IS NOT NULL`)); err != nil {
		return 0, fmt.Errorf("release active-time anchors: %w", err)
	}

	return int(credited), nil
}

// liveTransferStates are the states in which a transfer can still be given to a
// worker, as a SQL literal list.
//
// `paused` is in it deliberately: a paused transfer can hold jobs a worker is
// still finishing, and those seconds were spent. `cancelling` for the same
// reason - stopping does not take a blob out of a worker's hands.
const liveTransferStates = `'pending','planning','ready','running','paused',` +
	`'cancelling','verifying','promoting','syncing'`
