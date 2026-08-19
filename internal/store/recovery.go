package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Getting a transfer out of the state an outage leaves it in.
//
// # The two halves of the problem
//
// A job that exhausts its attempts becomes `failed`, which is terminal. The
// wave-drain check counts only `succeeded` and `skipped`, so one failed job
// means the wave never drains, the next wave never opens, and the transfer sits
// at `running` - correctly refusing to push a manifest whose blobs are missing
// (docs/design/04 §3.4), and indefinitely.
//
// That is right about the data and wrong about the reporting. Until this file
// existed, a transfer whose every job had failed still listed as `running` with
// nothing in flight, forever: nothing anywhere moved a transfer to `failed`
// because of its jobs. The state machine had a terminal job state and no
// terminal transfer state to match it, and the only symptom was a progress bar
// that stopped.
//
// So there are two operations here, and they are the outage story end to end:
//
//	SettleStalledTransfers   say that it has stopped
//	RetryTransfer            start it again
//
// # Why the retry is a requeue and not a re-plan
//
// Every job already carries everything needed to run it - endpoints, digest,
// size, wave - and `bytes_transferred` besides. Requeueing sets the failed rows
// back to `pending` and the work resumes from what is already at the
// destination: blobs that landed before the outage are found by the placement
// check or a HEAD and cost nothing the second time. Re-planning would throw
// that away and walk the manifest tree again for a set of jobs that is
// identical by construction.

// StalledTransfer is one transfer that has stopped and cannot restart itself.
type StalledTransfer struct {
	ID     string
	Failed int
	Reason string
}

// SettleStalledTransfers moves transfers that can no longer make progress into
// `failed`.
//
// The condition is exact and deliberately conservative: NO job of the transfer
// is `pending`, `blocked` or `leased`, and at least one is `failed`. A job
// sitting out a retry backoff is `pending`, so a transfer mid-backoff is not
// stalled - it is waiting, which is a different thing and must not be reported
// as an ending.
//
// # Why this is a sweep and not only an event
//
// CompleteJob settles the transfer inline, which handles the ordinary case: the
// last job to give up fails the transfer immediately. But the failure that
// matters most does not arrive that way. In a network outage the workers stop
// answering altogether, so nothing completes - the REAPER expires the leases,
// and a lease expiring on a job with no attempts left makes it `failed` without
// any completion being reported at all. A Coordinator restart mid-outage has
// the same shape. A periodic sweep catches every path, including ones added
// later, which is why it exists in addition to the inline check rather than
// instead of it.
func (p *Packages) SettleStalledTransfers(ctx context.Context) ([]StalledTransfer, error) {
	// Propagate permanent failures first. The reaper fails a job whose attempts
	// are exhausted without any completion being reported, so the inline
	// cascade in settleTransfer never runs for it - and a transfer left holding
	// blocked jobs behind a failed dependency looks runnable to the query
	// below and would never be settled at all.
	if err := p.failUnreachableEverywhere(ctx); err != nil {
		return nil, err
	}

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT t.id
		  FROM transfers t
		 WHERE t.state IN ('ready','running')
		   AND EXISTS (SELECT 1 FROM jobs j
		                WHERE j.transfer_id = t.id AND j.state = 'failed')
		   AND NOT EXISTS (SELECT 1 FROM jobs j
		                    WHERE j.transfer_id = t.id
		                      AND j.state IN ('pending','blocked','leased'))`))
	if err != nil {
		return nil, fmt.Errorf("find stalled transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stalled transfer: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]StalledTransfer, 0, len(ids))
	for _, id := range ids {
		failed, reason, err := p.failureSummary(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := p.FailTransfer(ctx, id, reason); err != nil {
			return nil, err
		}
		out = append(out, StalledTransfer{ID: id, Failed: failed, Reason: reason})
	}
	return out, nil
}

// failUnreachableEverywhere runs the dependency cascade for every live transfer
// that holds a permanent failure.
//
// Scoped to transfers that actually have one, so the common case - a fleet of
// healthy transfers - costs a single indexed query and no writes.
func (p *Packages) failUnreachableEverywhere(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT DISTINCT t.id
		  FROM transfers t
		  JOIN jobs f ON f.transfer_id = t.id AND f.state = 'failed'
		 WHERE t.state IN ('ready','running')
		   AND EXISTS (SELECT 1 FROM jobs b
		                WHERE b.transfer_id = t.id AND b.state = 'blocked')`))
	if err != nil {
		return fmt.Errorf("find transfers holding a permanent failure: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan a transfer holding a permanent failure: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin dependency cascade: %w", err)
		}
		if _, err := p.FailUnreachableJobs(ctx, tx, id); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit the dependency cascade of transfer %s: %w", id, err)
		}
	}
	return nil
}

// settleIfStalled is the inline check, inside an existing transaction.
//
// Called from the completion path so the transfer is marked the moment its last
// job gives up, rather than up to one sweep interval later. Returns the state
// the transfer is now in.
func (p *Packages) settleIfStalled(
	ctx context.Context, tx *sql.Tx, transferID, state string,
) (string, error) {
	if state != "running" && state != "ready" {
		return state, nil
	}

	var runnable, failed int
	err := tx.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT
		  SUM(CASE WHEN state IN ('pending','blocked','leased') THEN 1 ELSE 0 END),
		  SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END)
		 FROM jobs WHERE transfer_id = ?`), transferID).Scan(&runnable, &failed)
	if err != nil {
		return state, fmt.Errorf("check transfer %s for a stall: %w", transferID, err)
	}
	if runnable > 0 || failed == 0 {
		return state, nil
	}

	reason, err := p.failureReason(ctx, tx, transferID, failed)
	if err != nil {
		return state, err
	}
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET state = 'failed', failure_reason = ?,
		       completed_at = `+p.dialect.Now()+`, updated_at = `+p.dialect.Now()+`
		 WHERE id = ? AND state IN ('ready','running')`), reason, transferID); err != nil {
		return state, fmt.Errorf("fail stalled transfer %s: %w", transferID, err)
	}
	return "failed", nil
}

// failureSummary counts the failures and describes them, outside a transaction.
func (p *Packages) failureSummary(ctx context.Context, transferID string) (int, string, error) {
	var failed int
	if err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'failed'`),
		transferID).Scan(&failed); err != nil {
		return 0, "", fmt.Errorf("count failed jobs of transfer %s: %w", transferID, err)
	}
	reason, err := p.failureReason(ctx, nil, transferID, failed)
	return failed, reason, err
}

// failureReason builds the sentence stored on the transfer.
//
// It names the count, the dominant error class and one verbatim message,
// because those are three different questions - how much broke, what kind of
// breakage, and what the registry actually said - and a reason that answers
// only the first sends the reader to the jobs table to learn anything at all.
func (p *Packages) failureReason(
	ctx context.Context, tx *sql.Tx, transferID string, failed int,
) (string, error) {
	query := p.dialect.Rewrite(`
		SELECT COALESCE(last_error_class,''), COALESCE(last_error,''), count(*) AS n
		  FROM jobs
		 WHERE transfer_id = ? AND state = 'failed'
		 GROUP BY last_error_class, last_error
		 ORDER BY n DESC
		 LIMIT 1`)

	var scan func(dest ...any) error
	if tx != nil {
		scan = tx.QueryRowContext(ctx, query, transferID).Scan
	} else {
		scan = p.db.QueryRowContext(ctx, query, transferID).Scan
	}

	var (
		class, message string
		n              int
	)
	switch err := scan(&class, &message, &n); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Sprintf("%d job(s) failed after exhausting their attempts", failed), nil
	case err != nil:
		return "", fmt.Errorf("summarise failures of transfer %s: %w", transferID, err)
	}

	reason := fmt.Sprintf("%d job(s) failed after exhausting their attempts", failed)
	if class != "" {
		reason += " (" + class
		if message != "" {
			reason += ": " + truncate(message, 160)
		}
		reason += ")"
	} else if message != "" {
		reason += " (" + truncate(message, 160) + ")"
	}
	return reason, nil
}

// RetryResult is what a requeue did.
type RetryResult struct {
	TransferID string
	// Requeued is how many failed jobs were returned to the queue.
	Requeued int
	// Reblocked is how many failed only because something they depend on
	// failed. They go back to waiting rather than to running, and are counted
	// separately because "forty jobs requeued" when thirty-eight of them were
	// consequences of two overstates what actually broke.
	Reblocked int
	// State is the transfer's state afterwards.
	State string
	// NoJobs distinguishes a transfer that failed before any work existed -
	// during planning - from one whose jobs failed. The two need different
	// actions and both present as `failed`.
	NoJobs bool
}

// RetryTransfer returns a transfer's failed jobs to the queue.
//
// Idempotent in the way that matters: retrying a transfer with nothing failed
// changes nothing and reports zero, rather than erroring. A caller whose
// request timed out and retried must not be punished for it.
//
// # What is reset, and what is deliberately kept
//
//	attempts          -> 0     the budget is per outage, not per lifetime
//	next_visible_at   -> now   no backoff: the operator's action IS the signal
//	                           that the cause is gone
//	state             -> pending / blocked, by wave
//	bytes_transferred KEPT     a partially uploaded blob resumes its accounting
//	last_error        KEPT     until it succeeds. A requeued job that says why
//	                           it failed last time is the difference between
//	                           "it is running again" and "it is running again
//	                           and will hit the same proxy timeout"
func (p *Packages) RetryTransfer(ctx context.Context, transferID string) (RetryResult, error) {
	res := RetryResult{TransferID: transferID}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	var currentWave int
	var startedAt sql.NullString
	err = tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT state, current_wave, started_at FROM transfers WHERE id = ?`), transferID).
		Scan(&state, &currentWave, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("transfer %s not found", transferID)
	}
	if err != nil {
		return res, fmt.Errorf("read transfer %s: %w", transferID, err)
	}

	// A settled transfer is not a candidate. Requeueing jobs under a
	// `succeeded` transfer would move bytes nobody asked for and leave the
	// transfer claiming an end it has passed; `cancelled` is somebody's
	// explicit instruction and must not be undone by a retry.
	if state == "succeeded" || state == "cancelled" || state == "cancelling" {
		return res, fmt.Errorf("transfer %s is %s, so there is nothing to retry", transferID, state)
	}

	var total int
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ?`), transferID).Scan(&total); err != nil {
		return res, fmt.Errorf("count jobs of transfer %s: %w", transferID, err)
	}
	if total == 0 {
		// It never got as far as having work. Retrying would requeue nothing
		// and report success, which is the worst of both.
		res.NoJobs, res.State = true, state
		return res, nil
	}

	// Consequences before causes. A job that failed only because a dependency
	// of its own failed goes back to WAITING, not to running: its dependency is
	// about to be requeued, not satisfied, and treating the two the same way
	// would push a manifest whose blob is still in flight.
	//
	// Done first so the requeue below sees only genuine failures, and so the
	// lowest-failed-wave calculation is not dragged down to wave 0 by a
	// cascade that reached the whole tree.
	reblocked, err := p.ClearDependencyFailures(ctx, tx, transferID)
	if err != nil {
		return res, err
	}
	res.Reblocked = reblocked

	// The lowest wave holding a failed job. The transfer's current wave moves
	// back to it: a wave only advances once drained, so a failure below the
	// current wave should be impossible - and if a future change makes it
	// possible, reopening the wave is the safe answer rather than leaving
	// orphaned work behind a gate that has already been passed.
	var lowest sql.NullInt64
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT MIN(wave) FROM jobs WHERE transfer_id = ? AND state = 'failed'`),
		transferID).Scan(&lowest); err != nil {
		return res, fmt.Errorf("find the lowest failed wave of transfer %s: %w", transferID, err)
	}
	if lowest.Valid && int(lowest.Int64) < currentWave {
		currentWave = int(lowest.Int64)
	}

	// Jobs in the current wave become runnable; anything above it stays gated,
	// so the wave ordering that keeps a manifest from being pushed before its
	// blobs survives the retry.
	result, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = CASE WHEN wave <= ? THEN 'pending' ELSE 'blocked' END,
		       attempts         = 0,
		       lease_owner      = NULL,
		       lease_expires_at = NULL,
		       next_visible_at  = `+p.dialect.Now()+`,
		       completed_at     = NULL,
		       updated_at       = `+p.dialect.Now()+`
		 WHERE transfer_id = ? AND state = 'failed'`), currentWave, transferID)
	if err != nil {
		return res, fmt.Errorf("requeue failed jobs of transfer %s: %w", transferID, err)
	}
	requeued, err := result.RowsAffected()
	if err != nil {
		return res, fmt.Errorf("count requeued jobs of transfer %s: %w", transferID, err)
	}
	res.Requeued = int(requeued)

	if requeued == 0 && reblocked == 0 {
		// Nothing was failed. Say so without touching the transfer: a
		// `running` transfer must not be knocked back to `ready` by a retry
		// that had no work to do.
		res.State = state
		if err := tx.Commit(); err != nil {
			return res, fmt.Errorf("commit retry of transfer %s: %w", transferID, err)
		}
		return res, nil
	}

	// A job returned to `blocked` whose dependencies did in fact all succeed
	// must not stay there. That is the case where the cascade over-reached: it
	// fired while a sibling was still failing, and by now the sibling has been
	// requeued and its own dependents recomputed. One sweep settles it.
	if _, err := p.PromoteReadyJobs(ctx, tx, transferID); err != nil {
		return res, err
	}

	// `ready` rather than `running`: nothing is in flight yet, and a worker
	// leasing the first requeued job is what makes it running again - through
	// the same path every other transfer takes.
	newState := "ready"
	// auto_retries goes back to zero. A retry is somebody saying the cause has
	// been dealt with, and the automatic budget is for failures nobody has
	// looked at - a transfer that has just been looked at starts again with the
	// full allowance. AutoRetryTransfers re-increments it for its own attempts.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET state          = ?,
		       current_wave   = ?,
		       failure_reason = NULL,
		       completed_at   = NULL,
		       auto_retries   = 0,
		       updated_at     = `+p.dialect.Now()+`
		 WHERE id = ?`), newState, currentWave, transferID); err != nil {
		return res, fmt.Errorf("reopen transfer %s: %w", transferID, err)
	}
	res.State = newState

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit retry of transfer %s: %w", transferID, err)
	}
	return res, nil
}

// RetryableTransfers lists transfers a retry would act on.
//
// The fleet-wide case is the reason this exists: an outage does not fail one
// transfer, it fails every transfer that was running, and an operator should
// not have to page through a listing copying IDs out of it.
func (p *Packages) RetryableTransfers(ctx context.Context) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT DISTINCT t.id
		  FROM transfers t
		  JOIN jobs j ON j.transfer_id = t.id
		 WHERE t.state IN ('ready','running','failed')
		   AND j.state = 'failed'
		 ORDER BY t.id`))
	if err != nil {
		return nil, fmt.Errorf("list retryable transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan retryable transfer: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// truncate bounds a registry's error message before it is stored on a transfer.
//
// Some registries answer with a page of HTML. The reason column is read in a
// table cell, and a reason that wraps for forty lines is one nobody reads at
// all.
func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// StoppedCancellation is a transfer whose stop had not finished landing.
type StoppedCancellation struct {
	ID string
}

// CloseStalledCancellations settles transfers stuck in `cancelling` with
// nothing left in flight.
//
// # Why the inline close is not enough
//
// `cancelling` closes when the last lease REPORTS - a worker finishes or
// abandons the job, the completion arrives, and settleTransfer moves the
// transfer to `cancelled`. That is the ordinary path and it works.
//
// The paths that matter here are the ones with no completion at all. A worker
// that dies holding the last job reports nothing; the reaper cancels the job on
// lease expiry, and a reaper does not run the completion path. A Coordinator
// restarted between the stop and the last report is the same shape. In both,
// the transfer had nothing left to do and went on saying `cancelling` for as
// long as anybody left it there - which is exactly what a hang looks like, on
// the one operation somebody performs when they are already unhappy.
//
// Conservative in the same way SettleStalledTransfers is: a single leased job
// means a worker may still report, so the window stays open.
func (p *Packages) CloseStalledCancellations(ctx context.Context) ([]StoppedCancellation, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT t.id
		  FROM transfers t
		 WHERE t.state = 'cancelling'
		   AND NOT EXISTS (SELECT 1 FROM jobs j
		                    WHERE j.transfer_id = t.id AND j.state = 'leased')`))
	if err != nil {
		return nil, fmt.Errorf("find cancellations that cannot close: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a stuck cancellation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]StoppedCancellation, 0, len(ids))
	for _, id := range ids {
		res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
			`UPDATE transfers
			    SET state = 'cancelled',
			        completed_at = COALESCE(completed_at, `+p.dialect.Now()+`),
			        updated_at = `+p.dialect.Now()+`
			  WHERE id = ? AND state = 'cancelling'`), id)
		if err != nil {
			return nil, fmt.Errorf("close the cancellation of transfer %s: %w", id, err)
		}
		// Checked rather than assumed: a completion may have landed between the
		// select and this update, and reporting a close that did not happen
		// would put a line in the log for an event nobody can find.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			continue
		}
		out = append(out, StoppedCancellation{ID: id})
	}
	return out, nil
}

// AutoRetried is one transfer the system restarted by itself.
type AutoRetried struct {
	ID string
	// Requeued is how many jobs went back to the queue.
	Requeued int
	// Round is which automatic attempt this was, so a log line says whether
	// the system is recovering or thrashing.
	Round int
}

// AutoRetryTransfers requeues failures a second attempt could plausibly fix.
//
// # The failures this is for
//
// A registry timing out, a connection reset, a 503 while a registry has a bad
// minute. The job retries those itself while it has attempts left; when it runs
// out, the transfer stops and - until this existed - waited for a person to
// notice and press Retry. Which they do, usually the next morning, and the
// retry then succeeds, having achieved nothing a machine could not have done at
// 3am. The classes that are NOT worth retrying - auth, configuration,
// not-found, unsupported - are left alone, because a second attempt against a
// missing credential is a second failure at a fixed interval.
//
// # Why it is bounded, and spaced
//
// A retry loop that never gives up is a denial of service against a vendor
// registry with our name on it. Two bounds:
//
//	cooldown   nothing is retried until its failure has had time to pass
//	maxRounds  after which the transfer stays failed and waits for a human
//
// A transfer that has spent its automatic retries is telling us something a
// further attempt will not change, and the honest response is to stop and say
// so rather than to keep trying quietly.
//
// The count is reset by a manual retry - somebody saying they have dealt with
// the cause - and by success.
func (p *Packages) AutoRetryTransfers(
	ctx context.Context, cooldown time.Duration, maxRounds int,
) ([]AutoRetried, error) {
	if maxRounds <= 0 {
		return nil, nil
	}
	if cooldown < 0 {
		cooldown = 0
	}

	// Candidates first, in one query, so the retry itself runs against a small
	// list rather than a scan per transfer.
	//
	// `failed` AND the live states: a partial failure under a running transfer
	// is the same problem seen earlier, and waiting for the whole transfer to
	// give up before retrying three timed-out blobs would hold the other two
	// thousand jobs behind them.
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT t.id, t.auto_retries
		  FROM transfers t
		 WHERE t.state IN ('ready','running','failed')
		   AND t.auto_retries < ?
		   AND EXISTS (SELECT 1 FROM jobs j
		                WHERE j.transfer_id = t.id
		                  AND j.state = 'failed'
		                  AND COALESCE(j.last_error_class, '') NOT IN
		                      ('auth','unsupported','not_found','configuration','dependency'))
		   AND NOT EXISTS (SELECT 1 FROM jobs j
		                    WHERE j.transfer_id = t.id
		                      AND j.state IN ('pending','leased')
		                      AND NOT j.paused)
		   AND NOT EXISTS (SELECT 1 FROM jobs j
		                    WHERE j.transfer_id = t.id
		                      AND j.state = 'failed'
		                      AND j.completed_at > `+p.dialect.TimeAgo("?")+`)`),
		maxRounds, cooldown.Seconds())
	if err != nil {
		return nil, fmt.Errorf("find transfers worth retrying: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type candidate struct {
		id    string
		round int
	}
	var found []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.round); err != nil {
			return nil, fmt.Errorf("scan a transfer worth retrying: %w", err)
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]AutoRetried, 0, len(found))
	for _, c := range found {
		// The SAME requeue a person gets. An automatic path with its own idea
		// of what a retry means is a second implementation of the hardest part
		// of this system - wave reopening and dependency cascades - and the two
		// would diverge on the day it mattered.
		res, err := p.RetryTransfer(ctx, c.id)
		if err != nil {
			// One transfer that cannot be retried must not stop the sweep:
			// this runs unattended, and the next tick would hit the same one.
			continue
		}
		if res.Requeued == 0 {
			continue
		}

		round := c.round + 1
		if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
			`UPDATE transfers SET auto_retries = ?, updated_at = `+p.dialect.Now()+
				` WHERE id = ?`), round, c.id); err != nil {
			return nil, fmt.Errorf("record the automatic retry of transfer %s: %w", c.id, err)
		}
		out = append(out, AutoRetried{ID: c.id, Requeued: res.Requeued, Round: round})
	}
	return out, nil
}

// RecoveredLease is one job whose worker is not coming back for it.
type RecoveredLease struct {
	ID         int64
	TransferID string
	Owner      string
	// State is where the job went: back to the queue, or cancelled when the
	// transfer it belongs to was already stopping.
	State string
}

// RecoverOrphanedLeases frees work held by workers that are gone.
//
// # The gap this closes
//
// A lease is a promise with a deadline, and the reaper collects it when the
// deadline passes. That is the right mechanism for a worker that dies mid-blob
// - nobody knows it is dead until it stops answering - but it is the wrong
// mechanism for a restart, where we KNOW: the process is new, it holds nothing,
// and the leases in the database are from a life it no longer remembers.
// Waiting out the lease means minutes of a transfer sitting still with nothing
// running, and for a transfer somebody has stopped it means minutes of
// `cancelling` that looks exactly like a hang.
//
// Two signals, and this handles both:
//
//   - a worker that comes back and asks for work while holding nothing;
//   - a Coordinator that comes back and finds leases owned by workers whose
//     heartbeats stopped before it did.
//
// # Why a stopped transfer's work is cancelled rather than requeued
//
// Requeueing it would undo the stop by restart, which is the same fault as
// undoing it by timeout: somebody asked for the work to end, and a process
// coming back up is not new information about their intent.
func (p *Packages) RecoverOrphanedLeases(
	ctx context.Context, staleAfter time.Duration,
) ([]RecoveredLease, error) {
	if staleAfter <= 0 {
		staleAfter = 2 * time.Minute
	}

	// A worker with no row at all counts as gone: the row is written on every
	// lease and heartbeat, so its absence means nothing from that worker has
	// reached this database in living memory.
	return p.recoverLeases(ctx, `
		SELECT j.id, j.transfer_id, COALESCE(j.lease_owner, ''), t.state
		  FROM jobs j
		  JOIN transfers t ON t.id = j.transfer_id
		  LEFT JOIN workers w ON w.id = j.lease_owner
		 WHERE j.state = 'leased'
		   AND (w.id IS NULL OR w.last_heartbeat_at < `+p.dialect.TimeAgo("?")+`)`,
		staleAfter.Seconds())
}

// RecoverWorkerLeases frees work the database thinks a worker holds and the
// worker says it does not.
//
// Called when a worker asks for work while reporting nothing active - which is
// what a restarted process looks like from here, and is a far better signal
// than a lease deadline: it is the holder itself saying the work is not being
// done.
func (p *Packages) RecoverWorkerLeases(
	ctx context.Context, owner string,
) ([]RecoveredLease, error) {
	if owner == "" {
		return nil, nil
	}
	return p.recoverLeases(ctx, `
		SELECT j.id, j.transfer_id, COALESCE(j.lease_owner, ''), t.state
		  FROM jobs j
		  JOIN transfers t ON t.id = j.transfer_id
		 WHERE j.state = 'leased' AND j.lease_owner = ?`, owner)
}

func (p *Packages) recoverLeases(
	ctx context.Context, query string, args ...any,
) ([]RecoveredLease, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("find orphaned leases: %w", err)
	}

	type held struct {
		id            int64
		transferID    string
		owner         string
		transferState string
	}
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.transferID, &h.owner, &h.transferState); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan an orphaned lease: %w", err)
		}
		found = append(found, h)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}

	out := make([]RecoveredLease, 0, len(found))
	for _, h := range found {
		state := "pending"
		if h.transferState == "cancelling" || h.transferState == "cancelled" {
			state = "cancelled"
		}

		// Guarded on `state = 'leased'` so a completion that landed while this
		// was deciding wins. The worker's own report is always the better
		// answer; this is only for the case where none is coming.
		res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
			UPDATE jobs
			   SET state            = ?,
			       lease_owner      = NULL,
			       lease_expires_at = NULL,
			       next_visible_at  = `+p.dialect.Now()+`,
			       last_error       = COALESCE(last_error, 'the worker holding this job restarted'),
			       last_error_class = 'lease_expired',
			       completed_at     = CASE WHEN ? = 'cancelled' THEN `+p.dialect.Now()+` ELSE NULL END,
			       updated_at       = `+p.dialect.Now()+`
			 WHERE id = ? AND state = 'leased'`), state, state, h.id)
		if err != nil {
			return nil, fmt.Errorf("recover job %d: %w", h.id, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			continue
		}
		out = append(out, RecoveredLease{
			ID: h.id, TransferID: h.transferID, Owner: h.owner, State: state,
		})
	}
	return out, nil
}
