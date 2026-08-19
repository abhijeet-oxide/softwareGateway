package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Operational rollups over transfers and jobs.
//
// # Everything here is computed at request time
//
// Invariant I6: progress is a rollup over jobs, never a maintained counter. The
// same rule applies to reporting. A nightly-materialised metrics table would be
// a second source of truth for facts the jobs table already holds, and it would
// drift exactly when somebody was relying on it.
//
// # What is deliberately absent
//
// There is no verification metric. The `verifications` table exists and nothing
// in the product writes to it yet, so any figure derived from it would be a
// confident zero rather than a measurement. Reporting "0 verification failures"
// when nothing has ever been checked is the precise failure the honest-numbers
// rule exists to prevent (docs/design/19 section 6), so the field is not there
// at all and the UI renders the absence.

// ReportFilter bounds a report.
//
// Products is the same authorization seam as AuditFilter.Products: a caller
// scoped to two products has those two set here, server-side, and every
// aggregate below is scoped without a change to the SQL.
type ReportFilter struct {
	Products []string
	// Since and Until are RFC 3339 bounds on the transfer's completion.
	Since string
	Until string
}

// ProductReport is one product's operational summary for the period.
//
// Counts and byte totals only. Derived figures - average speed above all - are
// computed by the caller from these, so the rule about when a speed may be
// stated lives in one place rather than in the SQL.
type ProductReport struct {
	Product string

	// Transfers by outcome, counted on transfers that COMPLETED in the period.
	Completed int
	Failed    int
	Cancelled int

	// Running counts what is in flight NOW, regardless of the period: an
	// in-flight transfer has no completion time to fall inside one.
	Running int

	// BytesTransferred is what our workers actually moved: the sum of
	// per-job progress over succeeded jobs. Not planned bytes, and not the
	// release size.
	BytesTransferred int64
	// SavedBytes is what did not have to move - deduplicated at planning time
	// plus skipped at run time.
	SavedBytes int64

	// CopySeconds is summed wall-clock time over completed transfers whose
	// strategy was `copy`, and is the ONLY legitimate denominator for a speed.
	//
	// Mirror and proxy transfers are excluded on purpose: the registry moved
	// those bytes, we did not count them, and dividing a byte total we do not
	// have by a duration we do would manufacture a number
	// (docs/design/18 section 6.1).
	CopySeconds float64
	// CopyBytes is the numerator that matches CopySeconds.
	CopyBytes int64

	// Promotions is how many promote requests completed in the period.
	Promotions int
}

// FailureCause groups failed jobs by the classification the worker recorded.
type FailureCause struct {
	Product string
	// Class is the worker's error classification. Empty where a job failed
	// before one was assigned, which is reported as "unclassified" rather than
	// silently folded into another bucket.
	Class string
	Jobs  int
}

// Reports produces the summary the Reports page renders.
func (p *Packages) Reports(ctx context.Context, f ReportFilter) ([]ProductReport, error) {
	// The period bounds COMPLETION, not creation: "what did we get done this
	// week" is the question, and a transfer that started last week and landed
	// this one belongs to this one.
	//
	// Running transfers have no completed_at and would be excluded by that
	// bound, so they are counted by a separate term that ignores the period.
	elapsed := p.dialect.SecondsBetween("t.started_at", "t.completed_at")

	// Every aggregate below repeats the same period test, and each repetition
	// needs its own copy of the bounds in placeholder order. `within` returns
	// the clause AND records its arguments, so adding or removing an aggregate
	// keeps the argument list correct on its own. Go evaluates function calls
	// in lexical left-to-right order, which is what makes the recorded order
	// match the placeholder order.
	var args []any
	clause, bounds := periodBounds("t.completed_at", f)
	within := func() string {
		args = append(args, bounds...)
		return clause
	}

	query := `
		SELECT pr.name,
		       COUNT(*) FILTER (WHERE t.state = 'succeeded'` + within() + `),
		       COUNT(*) FILTER (WHERE t.state = 'failed'` + within() + `),
		       COUNT(*) FILTER (WHERE t.state = 'cancelled'` + within() + `),
		       COUNT(*) FILTER (WHERE t.state IN ('running','planning','ready','paused','verifying')),
		       COALESCE(SUM(t.dedupe_skipped_bytes) FILTER (
		           WHERE t.state = 'succeeded'` + within() + `), 0),
		       COALESCE(SUM(` + elapsed + `) FILTER (
		           WHERE t.state = 'succeeded' AND t.strategy = 'copy'
		             AND t.started_at IS NOT NULL` + within() + `), 0),
		       COUNT(*) FILTER (WHERE rq.operation = 'promote' AND t.state = 'succeeded'` +
		within() + `)
		  FROM transfers t
		  JOIN transfer_requests rq ON rq.id = t.request_id
		  JOIN products pr ON pr.id = rq.product_id
		 WHERE 1 = 1`

	if len(f.Products) > 0 {
		query += " AND pr.name IN (" + placeholders(len(f.Products)) + ")"
		for _, name := range f.Products {
			args = append(args, name)
		}
	}
	query += " GROUP BY pr.name ORDER BY pr.name"

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("report transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProductReport
	for rows.Next() {
		var r ProductReport
		if err := rows.Scan(&r.Product, &r.Completed, &r.Failed, &r.Cancelled, &r.Running,
			&r.SavedBytes, &r.CopySeconds, &r.Promotions); err != nil {
			return nil, fmt.Errorf("scan product report: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byProduct := make(map[string]*ProductReport, len(out))
	for i := range out {
		byProduct[out[i].Product] = &out[i]
	}
	if err := p.reportBytes(ctx, f, byProduct); err != nil {
		return nil, err
	}
	return out, nil
}

// reportBytes adds the job-level byte totals.
//
// A separate query rather than another join: transfers joined to jobs
// multiplies every transfer row by its job count, and the COUNT(*) FILTER
// terms above would each be inflated by that factor. Two queries that are
// each correct beat one that needs a DISTINCT to undo its own join.
func (p *Packages) reportBytes(
	ctx context.Context, f ReportFilter, byProduct map[string]*ProductReport,
) error {
	clause, bounds := periodBounds("t.completed_at", f)
	query := `
		SELECT pr.name,
		       COALESCE(SUM(j.bytes_transferred) FILTER (WHERE j.state = 'succeeded'), 0),
		       COALESCE(SUM(j.size_bytes)        FILTER (WHERE j.state = 'skipped'), 0),
		       COALESCE(SUM(j.bytes_transferred) FILTER (
		           WHERE j.state = 'succeeded' AND t.strategy = 'copy'), 0)
		  FROM jobs j
		  JOIN transfers t ON t.id = j.transfer_id
		  JOIN transfer_requests rq ON rq.id = t.request_id
		  JOIN products pr ON pr.id = rq.product_id
		 WHERE t.state = 'succeeded'` + clause
	args := append([]any{}, bounds...)

	if len(f.Products) > 0 {
		query += " AND pr.name IN (" + placeholders(len(f.Products)) + ")"
		for _, name := range f.Products {
			args = append(args, name)
		}
	}
	query += " GROUP BY pr.name"

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return fmt.Errorf("report bytes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		var transferred, skipped, copyBytes int64
		if err := rows.Scan(&name, &transferred, &skipped, &copyBytes); err != nil {
			return fmt.Errorf("scan report bytes: %w", err)
		}
		if r, ok := byProduct[name]; ok {
			r.BytesTransferred = transferred
			r.SavedBytes += skipped
			r.CopyBytes = copyBytes
		}
	}
	return rows.Err()
}

// FailureCauses groups the period's failed jobs by error class, worst first.
func (p *Packages) FailureCauses(ctx context.Context, f ReportFilter) ([]FailureCause, error) {
	clause, bounds := periodBounds("t.completed_at", f)
	query := `
		SELECT pr.name, COALESCE(j.last_error_class, ''), COUNT(*)
		  FROM jobs j
		  JOIN transfers t ON t.id = j.transfer_id
		  JOIN transfer_requests rq ON rq.id = t.request_id
		  JOIN products pr ON pr.id = rq.product_id
		 WHERE j.state = 'failed'` + clause
	args := append([]any{}, bounds...)

	if len(f.Products) > 0 {
		query += " AND pr.name IN (" + placeholders(len(f.Products)) + ")"
		for _, name := range f.Products {
			args = append(args, name)
		}
	}
	query += " GROUP BY pr.name, j.last_error_class ORDER BY COUNT(*) DESC, pr.name"

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("report failure causes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FailureCause
	for rows.Next() {
		var c FailureCause
		if err := rows.Scan(&c.Product, &c.Class, &c.Jobs); err != nil {
			return nil, fmt.Errorf("scan failure cause: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DailyVolume is one day's throughput, for the trend line.
type DailyVolume struct {
	Day              string
	BytesTransferred int64
	Transfers        int
}

// Volume returns per-day totals across the period, oldest first.
func (p *Packages) Volume(ctx context.Context, f ReportFilter) ([]DailyVolume, error) {
	clause, bounds := periodBounds("t.completed_at", f)
	// SUBSTR over the rendered timestamp rather than a dialect date function:
	// both dialects store an ISO-8601 prefix in the same order, so the first
	// ten characters are the day in each, and this needs no third dialect
	// method for a purely cosmetic grouping.
	day := "SUBSTR(CAST(t.completed_at AS CHAR(32)), 1, 10)"
	if p.dialect.Name() == DriverPostgres {
		day = "TO_CHAR(t.completed_at, 'YYYY-MM-DD')"
	}

	query := `
		SELECT ` + day + ` AS d,
		       COALESCE(SUM(j.bytes_transferred) FILTER (WHERE j.state = 'succeeded'), 0),
		       COUNT(DISTINCT t.id)
		  FROM transfers t
		  JOIN transfer_requests rq ON rq.id = t.request_id
		  JOIN products pr ON pr.id = rq.product_id
		  LEFT JOIN jobs j ON j.transfer_id = t.id
		 WHERE t.state = 'succeeded'` + clause
	args := append([]any{}, bounds...)

	if len(f.Products) > 0 {
		query += " AND pr.name IN (" + placeholders(len(f.Products)) + ")"
		for _, name := range f.Products {
			args = append(args, name)
		}
	}
	query += " GROUP BY d ORDER BY d"

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("report volume: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DailyVolume
	for rows.Next() {
		var v DailyVolume
		var d sql.NullString
		if err := rows.Scan(&d, &v.BytesTransferred, &v.Transfers); err != nil {
			return nil, fmt.Errorf("scan volume row: %w", err)
		}
		v.Day = d.String
		out = append(out, v)
	}
	return out, rows.Err()
}

// periodBounds renders the period test and the arguments it consumes.
//
// An absent bound contributes no SQL and no argument, rather than a test
// carrying a placeholder that is always empty. That keeps the planner looking
// at a plain range predicate it can use an index for, and keeps the argument
// list honest: a caller appends exactly what the clause it was handed asks
// for, however many bounds were set.
func periodBounds(column string, f ReportFilter) (string, []any) {
	var clause string
	var args []any
	if f.Since != "" {
		clause += " AND " + column + " >= ?"
		args = append(args, f.Since)
	}
	if f.Until != "" {
		clause += " AND " + column + " < ?"
		args = append(args, f.Until)
	}
	return clause, args
}
