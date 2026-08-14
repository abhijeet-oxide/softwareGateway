package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Stopping and starting a transfer that is already under way.
//
// Three verbs, and the distinction between them is what they do to the work
// already done rather than what they do to the work remaining:
//
//	pause    stop handing jobs out; everything already done stays done, and
//	         resuming picks up exactly where it left off
//	resume   the inverse
//	stop     give up on the rest; what already landed at the destination STAYS
//	         there, because it is content-addressed and a later transfer will
//	         find it and skip it
//
// None of them deletes anything at the destination. A stopped transfer is not a
// rollback — half a bundle at the destination is unreferenced blobs and
// untagged manifests, which are invisible to consumers (invariant I1) and
// useful to the next attempt.

// ErrIllegalTransition is a verb the transfer's current state does not admit.
//
// An error rather than a silent no-op: "pause a succeeded transfer" is somebody
// acting on a stale listing, and telling them the state has moved is the
// difference between a confusing outcome and an obvious one.
var ErrIllegalTransition = errors.New("illegal state transition")

// ControlResult is what a pause, resume or stop did.
type ControlResult struct {
	TransferID string
	// State is the transfer's state afterwards.
	State string
	// Jobs is how many job rows the verb affected.
	Jobs int
	// InFlight is how many jobs were still leased when the verb was applied.
	//
	// The number that explains why `stop` reports `cancelling` rather than
	// `cancelled`: a leased job belongs to a worker, and it stops at that
	// worker's next checkpoint rather than the instant somebody types the
	// command. Naming the window is what makes it observable instead of
	// looking stuck.
	InFlight int
}

// PauseTransfer stops jobs being handed out, leaving everything else alone.
//
// The pause is on the JOBS, not only on the transfer: the dequeue predicate
// reads `NOT paused`, so setting the flag is what actually stops work rather
// than merely recording an intention. A transfer state alone would need every
// lease path to remember to check it.
//
// Jobs already leased are NOT cancelled. They are bytes in flight, and
// abandoning a nine-gigabyte blob nine tenths of the way through to honour a
// pause a fraction of a second sooner is a bad trade. They finish, and nothing
// new starts.
func (p *Packages) PauseTransfer(ctx context.Context, transferID string) (ControlResult, error) {
	return p.control(ctx, transferID, controlSpec{
		from: []string{"ready", "running"},
		to:   "paused",
		verb: "pause",
		jobs: `UPDATE jobs SET paused = TRUE, updated_at = ` + p.dialect.Now() + `
		        WHERE transfer_id = ? AND state IN ('pending','blocked')`,
	})
}

// ResumeTransfer makes a paused transfer's jobs leasable again.
//
// It returns to `ready` rather than to `running`, for the same reason a retry
// does: nothing is in flight at that moment, and a worker leasing the first job
// is what makes it running — through the one path every other transfer takes.
func (p *Packages) ResumeTransfer(ctx context.Context, transferID string) (ControlResult, error) {
	return p.control(ctx, transferID, controlSpec{
		from: []string{"paused"},
		to:   "ready",
		verb: "resume",
		jobs: `UPDATE jobs SET paused = FALSE, updated_at = ` + p.dialect.Now() + `
		        WHERE transfer_id = ? AND paused`,
	})
}

// StopTransfer gives up on the work remaining.
//
// Everything not yet started is cancelled outright. Everything already leased
// is left to stop at its worker's next checkpoint, which is why the transfer
// lands in `cancelling` rather than `cancelled` when anything is in flight —
// see settleTransfer, which closes the window when the last lease reports.
//
// What already reached the destination stays there. It is content-addressed and
// untagged, so it is invisible to consumers and free to the next transfer;
// deleting it would throw away the only part of a stopped transfer that has any
// value.
func (p *Packages) StopTransfer(ctx context.Context, transferID string) (ControlResult, error) {
	return p.control(ctx, transferID, controlSpec{
		from: []string{"pending", "planning", "ready", "running", "paused"},
		to:   "cancelling",
		verb: "stop",
		jobs: `UPDATE jobs SET state = 'cancelled', paused = TRUE,
		              completed_at = ` + p.dialect.Now() + `,
		              updated_at = ` + p.dialect.Now() + `
		        WHERE transfer_id = ? AND state IN ('pending','blocked')`,
		settle: true,
	})
}

// controlSpec is one verb: which states admit it, what it does to the jobs, and
// what the transfer becomes.
type controlSpec struct {
	from []string
	to   string
	verb string
	jobs string
	// settle asks for the transfer to finish immediately when nothing is left
	// in flight, rather than waiting for a completion that will never come.
	settle bool
}

func (p *Packages) control(
	ctx context.Context, transferID string, spec controlSpec,
) (ControlResult, error) {
	res := ControlResult{TransferID: transferID}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin %s: %w", spec.verb, err)
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	err = tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT state FROM transfers WHERE id = ?`), transferID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("transfer %s not found", transferID)
	}
	if err != nil {
		return res, fmt.Errorf("read transfer %s: %w", transferID, err)
	}

	res.State = state
	if !contains(spec.from, state) {
		return res, fmt.Errorf("%w: cannot %s a transfer that is %s",
			ErrIllegalTransition, spec.verb, state)
	}

	result, err := tx.ExecContext(ctx, p.dialect.Rewrite(spec.jobs), transferID)
	if err != nil {
		return res, fmt.Errorf("%s jobs of transfer %s: %w", spec.verb, transferID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return res, fmt.Errorf("count %sd jobs of transfer %s: %w", spec.verb, transferID, err)
	}
	res.Jobs = int(affected)

	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'leased'`),
		transferID).Scan(&res.InFlight); err != nil {
		return res, fmt.Errorf("count leased jobs of transfer %s: %w", transferID, err)
	}

	// A stop with nothing in flight is already over. Waiting for a completion
	// that will never arrive would leave the transfer reading `cancelling`
	// forever, which is the shape of a hang.
	newState := spec.to
	completed := ""
	if spec.settle && res.InFlight == 0 {
		newState = "cancelled"
		completed = ", completed_at = " + p.dialect.Now()
	}

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET state = ?, updated_at = `+p.dialect.Now()+completed+
			` WHERE id = ?`), newState, transferID); err != nil {
		return res, fmt.Errorf("%s transfer %s: %w", spec.verb, transferID, err)
	}
	res.State = newState

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit %s of transfer %s: %w", spec.verb, transferID, err)
	}
	return res, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
