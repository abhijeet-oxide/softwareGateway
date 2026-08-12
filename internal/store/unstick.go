package store

import (
	"context"
	"fmt"
)

// A transfer that cannot move must not sit there.
//
// # The deadlock this exists for
//
// Observed on a real transfer, 31 hours in: 2286 jobs done, 207 outstanding, 0
// failed, nothing running, nothing runnable, nothing in retry. All 207 were
// `blocked`. Wave 0 was 1976/1976. The worker was healthy and idle.
//
// The cause was a repair meeting a transfer planned before dependency edges
// existed. `RepairMissingBlobs` returns a manifest to `blocked` on the promise
// that its edges will promote it again — and that transfer had none, so
// `PromoteDependents` (which requires an edge) could never fire. Wave
// advancement could not help either: it runs when a completing job's wave
// drains, and the wave those manifests sat in could never drain while they were
// blocked. A closed loop with no exit.
//
// The specific bug is fixed at its source — repair now writes the edges it
// depends on. This is the general answer, and it is worth having separately:
// `blocked` is the one state nothing else times out. A leased job has a lease,
// a pending job has a backoff, a failed job has an attempt count. A blocked job
// waits for an event, and if that event cannot come, nothing anywhere notices.
// It did not notice for 31 hours.
//
// # Why promoting is safe here
//
// Two conditions, and both are required:
//
//  1. The transfer has NOTHING in `pending` or `leased`. So there is no work in
//     flight that a blocked job might legitimately be waiting for, and no
//     repair mid-way through returning content to the destination.
//  2. The job's wave has already opened (`wave <= current_wave`) and no
//     dependency of its own is outstanding.
//
// The wave test is the same guarantee `advanceWave` gives — a wave only opens
// once every wave below it has drained — so a job at or below the watermark has
// its content at the destination. The dependency test is invariant I1 stated
// directly. A job that satisfies both is runnable by every rule the system has;
// the only thing keeping it blocked is that nothing thought to say so.

// UnstuckTransfer is one transfer that could not move and now can.
type UnstuckTransfer struct {
	ID string
	// Promoted is how many blocked jobs were made runnable.
	Promoted int
}

// UnstickTransfers finds transfers with nothing runnable and promotes whatever
// is provably ready.
//
// Returns only the transfers it actually changed, so a caller can log a real
// event rather than a heartbeat.
func (p *Packages) UnstickTransfers(ctx context.Context) ([]UnstuckTransfer, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT t.id
		  FROM transfers t
		 WHERE t.state IN ('ready','running')
		   AND EXISTS (SELECT 1 FROM jobs b
		                WHERE b.transfer_id = t.id AND b.state = 'blocked')
		   AND NOT EXISTS (SELECT 1 FROM jobs r
		                    WHERE r.transfer_id = t.id
		                      AND r.state IN ('pending','leased'))`))
	if err != nil {
		return nil, fmt.Errorf("find transfers that cannot move: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a stuck transfer: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []UnstuckTransfer
	for _, id := range ids {
		n, err := p.unstick(ctx, id)
		if err != nil {
			return out, err
		}
		if n > 0 {
			out = append(out, UnstuckTransfer{ID: id, Promoted: n})
		}
	}
	return out, nil
}

func (p *Packages) unstick(ctx context.Context, transferID string) (int, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin unstick: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state = 'pending', updated_at = `+p.dialect.Now()+`
		 WHERE jobs.transfer_id = ?
		   AND jobs.state = 'blocked'
		   AND jobs.wave <= (SELECT current_wave FROM transfers WHERE id = ?)
		   AND NOT EXISTS (SELECT 1 FROM job_dependencies o
		                     JOIN jobs dep ON dep.id = o.depends_on_id
		                    WHERE o.job_id = jobs.id
		                      AND dep.state NOT IN ('succeeded','skipped'))`),
		transferID, transferID)
	if err != nil {
		return 0, fmt.Errorf("unstick transfer %s: %w", transferID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the jobs unstuck in transfer %s: %w", transferID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit the unsticking of transfer %s: %w", transferID, err)
	}
	return int(n), nil
}
