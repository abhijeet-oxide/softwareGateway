package store

import (
	"context"
	"fmt"
	"time"
)

// Keeping the database bounded.
//
// # What grows, and what must not be touched
//
// Three things grow with USE rather than with the size of the catalogue, and
// they are the whole problem:
//
//	jobs          ~2500 rows per transfer, and a transfer per release per target
//	worker_logs   one row per interesting thing a worker did
//	audit_events  one row per state transition
//
// Everything else grows with the CATALOGUE — one row per package, per artifact,
// per blob — and is the answer to "what does this vendor publish", which is the
// point of the system. None of it is swept here, and the manifest bodies that
// would otherwise dominate are already bounded by their own cache policy (see
// manifestcache.go).
//
// # Why deleting a settled transfer is safe, and deleting anything else is not
//
// A settled transfer's rows are a RECORD OF WORK, not a record of content. What
// actually landed at the destination is at the destination, and what we know
// about the source is in the catalogue. Deleting the jobs of a transfer that
// finished a month ago loses the per-blob byte counts of that run and nothing
// else.
//
// `blob_placements` is deliberately NOT swept by default, and the reason is
// worth stating because it looks like the same kind of row. It is the memory
// that makes a re-transfer nearly free: a placement is what lets the planner
// skip a blob without asking the registry. Losing one costs a HEAD per blob at
// the next transfer — never a re-upload, so it is safe — but it is the single
// highest-value small table in the schema, and it is tiny. It is swept only if
// somebody sets a TTL, and the comment on that field says what it costs.

// RetentionPolicy bounds how much history is kept.
//
// Every duration is zero-means-keep-forever, so a deployment that wants an
// unbounded audit trail gets one by leaving the field unset rather than by
// finding a flag to turn something off.
type RetentionPolicy struct {
	// Transfers deletes settled transfers, and their jobs with them, once they
	// have been finished for this long.
	Transfers time.Duration
	// WorkerLogs deletes log lines older than this. They are a convenience tail
	// for a debugging question, not a log store — cluster log aggregation
	// remains the system of record.
	WorkerLogs time.Duration
	// AuditEvents deletes audit rows older than this. Usually the LONGEST of
	// these, and often left unset: an audit trail with a short retention is not
	// an audit trail.
	AuditEvents time.Duration
	// Placements deletes blob placement records not confirmed in this long.
	//
	// Zero, and left zero unless somebody has a reason. This table is the
	// memory that makes a second transfer of a product line nearly free, and
	// losing a row costs a HEAD per blob on the next transfer. It is safe —
	// never a re-upload — but it is a poor trade for a table measured in tens
	// of thousands of rows.
	Placements time.Duration
	// BatchSize bounds one pass, so a first sweep of a database that has run
	// unbounded for a year does not take a table lock for minutes.
	BatchSize int
}

// Enabled reports whether anything would be deleted.
func (p RetentionPolicy) Enabled() bool {
	return p.Transfers > 0 || p.WorkerLogs > 0 || p.AuditEvents > 0 || p.Placements > 0
}

// RetentionResult is what one sweep removed.
type RetentionResult struct {
	Transfers   int
	Requests    int
	WorkerLogs  int
	AuditEvents int
	Placements  int
}

// Rows is the total, for deciding whether the sweep is worth a log line.
func (r RetentionResult) Rows() int {
	return r.Transfers + r.Requests + r.WorkerLogs + r.AuditEvents + r.Placements
}

// SweepRetention deletes history past its retention.
//
// Each pass is bounded by BatchSize and each statement stands alone, so a sweep
// that is interrupted has done part of the work rather than none — and the next
// one continues. There is nothing to be consistent ACROSS these deletions:
// removing a transfer's jobs but not yet its worker logs leaves logs about a
// transfer that no longer exists, which the log view already tolerates because
// a log line has always outlived the lease it describes.
func (p *Packages) SweepRetention(
	ctx context.Context, policy RetentionPolicy,
) (RetentionResult, error) {
	var res RetentionResult
	batch := policy.BatchSize
	if batch <= 0 {
		batch = 5000
	}

	if policy.Transfers > 0 {
		n, err := p.deleteSettledTransfers(ctx, policy.Transfers, batch)
		if err != nil {
			return res, err
		}
		res.Transfers = n

		// Requests AFTER transfers, and only those with none left. A request
		// with no transfers is not necessarily old work — it may be one that
		// has just been created and not yet planned — so it is bounded by the
		// same age as well as by being empty.
		n, err = p.deleteOrphanRequests(ctx, policy.Transfers, batch)
		if err != nil {
			return res, err
		}
		res.Requests = n
	}

	if policy.WorkerLogs > 0 {
		n, err := p.deleteBatch(ctx, "worker_logs", "id", "logged_at",
			policy.WorkerLogs, batch)
		if err != nil {
			return res, fmt.Errorf("sweep worker logs: %w", err)
		}
		res.WorkerLogs = n
	}

	if policy.AuditEvents > 0 {
		n, err := p.deleteBatch(ctx, "audit_events", "id", "occurred_at",
			policy.AuditEvents, batch)
		if err != nil {
			return res, fmt.Errorf("sweep audit events: %w", err)
		}
		res.AuditEvents = n
	}

	if policy.Placements > 0 {
		// Not batched, and it is the one delete here that is not. A placement
		// is keyed by (repository, digest), and no single column of it is
		// unique — so the bounded form every other sweep uses would either
		// over-delete or need a row-value IN that only one of the two dialects
		// is reliable about. This sweep is off unless somebody turns it on, the
		// table is small, and `verified_at` is indexed.
		result, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
			`DELETE FROM blob_placements WHERE verified_at < `+p.dialect.TimeAgo("?")),
			seconds(policy.Placements))
		if err != nil {
			return res, fmt.Errorf("sweep blob placements: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("count swept blob placements: %w", err)
		}
		res.Placements = int(n)
	}

	return res, nil
}

// deleteSettledTransfers removes transfers that finished long enough ago.
//
// SETTLED, not merely old: a transfer that has been running for a month is a
// transfer somebody is watching, and deleting it out from under them would look
// exactly like the data loss this sweep exists to avoid being blamed for. Its
// jobs, its dependencies and its placements-by-job go with it through the
// foreign keys.
func (p *Packages) deleteSettledTransfers(
	ctx context.Context, ttl time.Duration, batch int,
) (int, error) {
	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		DELETE FROM transfers
		 WHERE id IN (
		   SELECT id FROM transfers
		    WHERE state IN ('succeeded','failed','cancelled')
		      AND completed_at IS NOT NULL
		      AND completed_at < `+p.dialect.TimeAgo("?")+`
		    LIMIT ?)`), seconds(ttl), batch)
	if err != nil {
		return 0, fmt.Errorf("sweep settled transfers: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// deleteOrphanRequests removes requests nothing points at any more.
func (p *Packages) deleteOrphanRequests(
	ctx context.Context, ttl time.Duration, batch int,
) (int, error) {
	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		DELETE FROM transfer_requests
		 WHERE id IN (
		   SELECT r.id FROM transfer_requests r
		    WHERE r.created_at < `+p.dialect.TimeAgo("?")+`
		      AND NOT EXISTS (SELECT 1 FROM transfers t WHERE t.request_id = r.id)
		    LIMIT ?)`), seconds(ttl), batch)
	if err != nil {
		return 0, fmt.Errorf("sweep orphan transfer requests: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// deleteBatch removes up to batch rows of one table older than a TTL.
//
// Bounded through the table's own primary key rather than with a bare LIMIT on
// the DELETE, because Postgres has no such clause. The bound is what stops the
// first sweep of a database that has run unbounded for a year from taking a
// lock for minutes.
func (p *Packages) deleteBatch(
	ctx context.Context, table, key, timeColumn string, ttl time.Duration, batch int,
) (int, error) {
	query := p.dialect.Rewrite(fmt.Sprintf(
		`DELETE FROM %s WHERE %s IN (
		   SELECT %s FROM %s WHERE %s < %s LIMIT ?)`,
		table, key, key, table, timeColumn, p.dialect.TimeAgo("?")))

	res, err := p.db.ExecContext(ctx, query, seconds(ttl), batch)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// seconds is a TTL in the unit dialect.TimeAgo takes.
//
// Whole seconds, and a floor of one: a TTL rounding to zero would compare
// against "now" and delete rows written in the same instant, which is the
// difference between a retention policy and a bug.
func seconds(ttl time.Duration) int64 {
	if n := int64(ttl.Seconds()); n > 0 {
		return n
	}
	return 1
}
