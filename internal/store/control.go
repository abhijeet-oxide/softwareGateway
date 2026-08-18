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

// ErrInvalidPriority is a priority outside the band the schema allows.
var ErrInvalidPriority = errors.New("invalid priority")

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

// MinPriority and MaxPriority bound what an operator may ask for.
//
// The database says the same thing (docs/design/04 §6), and saying it here too
// is not duplication for its own sake: a CHECK violation arrives as an opaque
// constraint error naming a column, and the caller asked a question that
// deserves an answer in the terms they asked it.
const (
	MinPriority = 0
	MaxPriority = 1000
)

// SetTransferPriority reorders a transfer's remaining work.
//
// # It changes the JOBS, and that is the whole mechanism
//
// The dequeue orders by `jobs.priority` and never reads the transfer's, so a
// change written only to the transfer row would be visible on every page and
// affect nothing at all. The transfer's own column is updated too, because it
// is what a listing shows and what a later step of the same request inherits.
//
// # Only what has not been leased
//
// A leased job belongs to a worker and is already moving bytes. Preempting a
// 40 GB blob at 90% to start a higher-priority one throws away more work than
// the reordering could recover (docs/design/04 §6), so in-flight work runs to
// completion and the new order applies to everything behind it.
//
// # Why a settled transfer is refused
//
// There is nothing left to order. Accepting it would report a change that
// changed nothing, which is worse than a conflict: the caller believes their
// request took effect.
func (p *Packages) SetTransferPriority(
	ctx context.Context, transferID string, priority int,
) (ControlResult, error) {
	res := ControlResult{TransferID: transferID}

	if priority < MinPriority || priority > MaxPriority {
		return res, fmt.Errorf("%w: priority %d is outside %d-%d",
			ErrInvalidPriority, priority, MinPriority, MaxPriority)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin setPriority: %w", err)
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

	if contains([]string{"succeeded", "failed", "cancelled"}, state) {
		return res, fmt.Errorf(
			"%w: transfer %s is %s, so it has no work left to order",
			ErrIllegalTransition, transferID, state)
	}

	result, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE jobs SET priority = ?, updated_at = `+p.dialect.Now()+`
		  WHERE transfer_id = ? AND state IN ('pending','blocked')`),
		priority, transferID)
	if err != nil {
		return res, fmt.Errorf("reprioritize jobs of transfer %s: %w", transferID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return res, fmt.Errorf("count reprioritized jobs of transfer %s: %w", transferID, err)
	}
	res.Jobs = int(affected)

	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'leased'`),
		transferID).Scan(&res.InFlight); err != nil {
		return res, fmt.Errorf("count leased jobs of transfer %s: %w", transferID, err)
	}

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET priority = ?, updated_at = `+p.dialect.Now()+
			` WHERE id = ?`), priority, transferID); err != nil {
		return res, fmt.Errorf("set priority of transfer %s: %w", transferID, err)
	}

	// The REQUEST too, so the rest of a chain inherits the new order. A
	// download of three steps creates the later transfers when the earlier ones
	// finish, from the request's priority — raise only this transfer and the
	// next step silently drops back to where it was, which reads as the change
	// having been undone by something.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfer_requests SET priority = ?, updated_at = `+p.dialect.Now()+`
		  WHERE id = (SELECT request_id FROM transfers WHERE id = ?)`),
		priority, transferID); err != nil {
		return res, fmt.Errorf("set priority of the request behind transfer %s: %w",
			transferID, err)
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit setPriority of transfer %s: %w", transferID, err)
	}
	return res, nil
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

// DeleteTransfer removes a transfer's RECORD, and nothing else.
//
// # What this is not
//
// It is not a rollback. Nothing at the destination is removed, and nothing
// could be: what a transfer put there is content-addressed, shared with every
// other release that references the same layers, and — where the transfer did
// not finish — untagged and invisible to consumers already. A delete that
// reached into a registry to unpick that would be the most dangerous operation
// in this system, and it is not what anybody asking for one wants: they want
// the row out of their listing.
//
// So this is bookkeeping. The transfer, its jobs and their dependency edges go;
// the placements do not, because they describe the destination rather than the
// transfer, and a later transfer of the same content still wants to know the
// bytes are there.
//
// # Only a settled transfer
//
// A running transfer's jobs are LEASED — a worker holds them and will report on
// them — and deleting the rows underneath it turns every one of those reports
// into an update of nothing, silently. `stop` exists, it is one word, and it
// leaves a transfer this will accept. Refusing is a second's inconvenience
// against a class of corruption that would be very hard to explain afterwards.
func (p *Packages) DeleteTransfer(ctx context.Context, transferID string) (ControlResult, error) {
	res := ControlResult{TransferID: transferID}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin delete: %w", err)
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

	if !contains([]string{"succeeded", "failed", "cancelled"}, state) {
		return res, fmt.Errorf(
			"%w: transfer %s is %s, so it still has work a worker may be holding; "+
				"stop it first", ErrIllegalTransition, transferID, state)
	}

	// Counted before the delete, because afterwards there is nothing to count
	// and "deleted 0 jobs" would be indistinguishable from a transfer that
	// never had any.
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ?`), transferID).Scan(&res.Jobs); err != nil {
		return res, fmt.Errorf("count jobs of transfer %s: %w", transferID, err)
	}

	// The jobs go explicitly rather than by cascade. Both databases declare it,
	// but SQLite enforces a foreign key only when `PRAGMA foreign_keys` is on
	// for that connection — so relying on the declaration would leave orphaned
	// job rows on one backend and not the other, which is the kind of
	// difference nobody finds until a count is wrong months later.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`DELETE FROM job_dependencies
		  WHERE job_id IN (SELECT id FROM jobs WHERE transfer_id = ?)
		     OR depends_on_id IN (SELECT id FROM jobs WHERE transfer_id = ?)`),
		transferID, transferID); err != nil {
		return res, fmt.Errorf("delete job edges of transfer %s: %w", transferID, err)
	}
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`DELETE FROM jobs WHERE transfer_id = ?`), transferID); err != nil {
		return res, fmt.Errorf("delete jobs of transfer %s: %w", transferID, err)
	}
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`DELETE FROM transfers WHERE id = ?`), transferID); err != nil {
		return res, fmt.Errorf("delete transfer %s: %w", transferID, err)
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit delete of transfer %s: %w", transferID, err)
	}
	return res, nil
}
