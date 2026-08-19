package store

import (
	"context"
	"fmt"
	"time"
)

// The worker fleet, as the Coordinator knows it.
//
// # Why this file had to exist
//
// The `workers` table was created in the first migration and nothing ever
// wrote to it. Every fact needed to fill it already crossed the wire on every
// lease and every heartbeat - the worker's ID, its build, how much capacity it
// has and how much of it is in use - and all of it was read for the one
// decision at hand and then dropped. So `health` could not answer "are the
// workers up?", the fleet had no cardinality, and "only one job is running"
// was a question with no way to look.
//
// Recording it costs one upsert per lease and per heartbeat, on a row per
// worker.
//
// # What a worker row is NOT
//
// It is not a lock, not a lease, and nothing depends on it. The queue's
// crash-recovery story is the reaper and a timestamp on the JOB
// (docs/design/04 §4.3), and it stays that way: a worker row that is stale,
// missing, or wrong changes nothing about which jobs run. It is observability,
// and it is allowed to fail without failing the lease that produced it.

// WorkerRow is what a worker reports about itself.
type WorkerRow struct {
	ID      string
	Version string
	// MaxConcurrency is the worker's configured ceiling, derived from what it
	// asked for plus what it already holds. A worker with 32 configured and 30
	// running asks for 2, and the sum is the number an operator recognises
	// from their config file.
	MaxConcurrency int
	ActiveJobs     int
}

// WorkerSummary is one worker as the fleet view reports it.
type WorkerSummary struct {
	ID             string
	Version        string
	MaxConcurrency int
	ActiveJobs     int
	// LeasedJobs is counted from the JOBS table rather than taken from the
	// worker's own claim. The two disagreeing is itself worth seeing: a worker
	// that thinks it holds four jobs the Coordinator has already reaped is a
	// worker about to report completions nobody will accept.
	LeasedJobs    int
	LastHeartbeat time.Time
	// Stale means the last heartbeat is older than the lease period, so this
	// worker's jobs are being reaped or are about to be.
	Stale bool
	// State is the stored lifecycle: active, draining, stale.
	State string
}

// RecordWorker upserts what a worker just told us about itself.
//
// Deliberately best-effort at the call site: the fleet view is worth having and
// is worth nothing at the cost of a failed lease.
func (p *Packages) RecordWorker(ctx context.Context, w WorkerRow) error {
	if w.ID == "" {
		return nil
	}

	// ON CONFLICT rather than a read-then-write, so two Coordinator replicas
	// serving two leases from the same worker cannot race to create the row.
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		INSERT INTO workers (id, version, max_concurrency, active_jobs,
		                     last_heartbeat_at, state)
		     VALUES (?, ?, ?, ?, `+p.dialect.Now()+`, 'active')
		ON CONFLICT (id) DO UPDATE
		    SET version           = excluded.version,
		        max_concurrency   = excluded.max_concurrency,
		        active_jobs       = excluded.active_jobs,
		        last_heartbeat_at = excluded.last_heartbeat_at,
		        state             = 'active'`),
		w.ID, w.Version, w.MaxConcurrency, w.ActiveJobs)
	if err != nil {
		return fmt.Errorf("record worker %s: %w", w.ID, err)
	}
	return nil
}

// TouchWorker refreshes liveness and the active count from a heartbeat.
//
// Separate from RecordWorker because a heartbeat does not carry the worker's
// capacity - only a lease does - and overwriting a known ceiling with a zero
// would make the fleet view report every worker as having no capacity between
// leases, which on a multi-gigabyte blob is most of the time.
func (p *Packages) TouchWorker(ctx context.Context, id string, active int) error {
	if id == "" {
		return nil
	}
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE workers
		   SET active_jobs       = ?,
		       last_heartbeat_at = `+p.dialect.Now()+`,
		       state             = 'active'
		 WHERE id = ?`), active, id)
	if err != nil {
		return fmt.Errorf("touch worker %s: %w", id, err)
	}
	return nil
}

// ListWorkers reports the fleet, most recently heard from first.
//
// staleAfter is the lease period: a worker silent for longer than that is one
// whose jobs the reaper is already taking back, which is the single most useful
// thing this view can say.
func (p *Packages) ListWorkers(ctx context.Context, staleAfter time.Duration) ([]WorkerSummary, error) {
	if staleAfter <= 0 {
		staleAfter = 2 * time.Minute
	}

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT w.id, w.version, w.max_concurrency, w.active_jobs, w.state,
		       w.last_heartbeat_at,
		       COALESCE((SELECT count(*) FROM jobs j
		                  WHERE j.state = 'leased' AND j.lease_owner = w.id), 0)
		  FROM workers w
		 ORDER BY w.last_heartbeat_at DESC`))
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cutoff := time.Now().Add(-staleAfter)
	var out []WorkerSummary
	for rows.Next() {
		var (
			w         WorkerSummary
			heartbeat string
		)
		if err := rows.Scan(&w.ID, &w.Version, &w.MaxConcurrency, &w.ActiveJobs,
			&w.State, &heartbeat, &w.LeasedJobs); err != nil {
			return nil, fmt.Errorf("scan worker: %w", err)
		}
		if at, ok := parseStoredTime(heartbeat); ok {
			w.LastHeartbeat = at
			w.Stale = at.Before(cutoff)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ForgetWorker removes a worker row.
//
// For a worker that has been decommissioned rather than one that is merely
// quiet: nothing here reaps rows automatically, because a worker that vanished
// is exactly the thing somebody needs to see in the list.
func (p *Packages) ForgetWorker(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx,
		p.dialect.Rewrite(`DELETE FROM workers WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("forget worker %s: %w", id, err)
	}
	return nil
}

// parseStoredTime reads a timestamp in whichever spelling the dialect wrote.
func parseStoredTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999Z",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
