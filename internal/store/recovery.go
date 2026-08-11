package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Getting a transfer out of the state an outage leaves it in.
//
// # The two halves of the problem
//
// A job that exhausts its attempts becomes `failed`, which is terminal. The
// wave-drain check counts only `succeeded` and `skipped`, so one failed job
// means the wave never drains, the next wave never opens, and the transfer sits
// at `running` — correctly refusing to push a manifest whose blobs are missing
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
// Every job already carries everything needed to run it — endpoints, digest,
// size, wave — and `bytes_transferred` besides. Requeueing sets the failed rows
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
// stalled — it is waiting, which is a different thing and must not be reported
// as an ending.
//
// # Why this is a sweep and not only an event
//
// CompleteJob settles the transfer inline, which handles the ordinary case: the
// last job to give up fails the transfer immediately. But the failure that
// matters most does not arrive that way. In a network outage the workers stop
// answering altogether, so nothing completes — the REAPER expires the leases,
// and a lease expiring on a job with no attempts left makes it `failed` without
// any completion being reported at all. A Coordinator restart mid-outage has
// the same shape. A periodic sweep catches every path, including ones added
// later, which is why it exists in addition to the inline check rather than
// instead of it.
func (p *Packages) SettleStalledTransfers(ctx context.Context) ([]StalledTransfer, error) {
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
// because those are three different questions — how much broke, what kind of
// breakage, and what the registry actually said — and a reason that answers
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
	// State is the transfer's state afterwards.
	State string
	// NoJobs distinguishes a transfer that failed before any work existed —
	// during planning — from one whose jobs failed. The two need different
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

	// The lowest wave holding a failed job. The transfer's current wave moves
	// back to it: a wave only advances once drained, so a failure below the
	// current wave should be impossible — and if a future change makes it
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

	if requeued == 0 {
		// Nothing was failed. Say so without touching the transfer: a
		// `running` transfer must not be knocked back to `ready` by a retry
		// that had no work to do.
		res.State = state
		if err := tx.Commit(); err != nil {
			return res, fmt.Errorf("commit retry of transfer %s: %w", transferID, err)
		}
		return res, nil
	}

	// `ready` rather than `running`: nothing is in flight yet, and a worker
	// leasing the first requeued job is what makes it running again — through
	// the same path every other transfer takes.
	newState := "ready"
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfers
		   SET state          = ?,
		       current_wave   = ?,
		       failure_reason = NULL,
		       completed_at   = NULL,
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
