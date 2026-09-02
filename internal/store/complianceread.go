package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Reading a compliance run back.
//
// # Why the filter is here and not in the handler
//
// A release produces ten to fifteen thousand result rows, most of them passes.
// Loading all of them to show a page of failures is the query that is fine on a
// developer's laptop and unusable on the release somebody actually cares about.
// The filter is pushed into SQL so the default view - failures, grouped by
// chart - reads a few hundred rows.

// ComplianceFilter narrows a result listing.
//
// Every field is something a reader asks for by name while working through a
// report: "just the blocking ones", "just this chart", "just the ones the
// vendor has to fix". Determinacy is in the list because it is the difference
// between the vendor's problem and the site's, and somebody triaging a report
// splits it that way first.
type ComplianceFilter struct {
	Outcomes    []string
	Severities  []string
	Checks      []string
	Charts      []string
	Kinds       []string
	Determinacy []string
	Search      string
	Limit       int
	Offset      int

	// Seq narrows to ONE result by its position in the run, which is its
	// identity there. The evidence endpoints take it rather than an address
	// from the caller: an excerpt is a claim about what a run found, so the run
	// has to be what says where to point.
	Seq *int
}

// LatestComplianceRun returns a package's most recent run, or ErrNotFound.
func (p *Packages) LatestComplianceRun(ctx context.Context, packageID int64) (ComplianceRunRow, error) {
	return p.complianceRunWhere(ctx,
		`WHERE package_id = ? ORDER BY started_at DESC LIMIT 1`, packageID)
}

// ComplianceRun returns one run by id.
func (p *Packages) ComplianceRun(ctx context.Context, id string) (ComplianceRunRow, error) {
	return p.complianceRunWhere(ctx, `WHERE id = ?`, id)
}

func (p *Packages) complianceRunWhere(ctx context.Context, where string, args ...any) (ComplianceRunRow, error) {
	// Timestamps are rendered as text by the dialect. SQLite stores them as
	// strings and Postgres returns time.Time, so a single scan target only
	// works if the query normalizes them - which is what TimestampText is for.
	var (
		r         ComplianceRunRow
		errText   sql.NullString
		logText   sql.NullString
		started   string
		finished  string
		heartbeat string
	)
	d := p.dialect
	err := p.db.QueryRowContext(ctx, d.Rewrite(`
		SELECT id, package_id, state, COALESCE(error, ''), verdict,
		       bundle_digest, helm_version, kube_version, checks,
		       pass_count, fail_count, skip_count, error_count, waived_count,
		       blocking_count, warning_count, info_count,
		       truncated, trigger, log, `+
		d.TimestampText("started_at")+`, `+
		d.TimestampText("finished_at")+`, `+
		d.TimestampText("heartbeat_at")+`
		  FROM compliance_runs `+where), args...).Scan(
		&r.ID, &r.PackageID, &r.State, &errText, &r.Verdict,
		&r.BundleDigest, &r.HelmVersion, &r.KubeVersion, &r.Checks,
		&r.Pass, &r.Fail, &r.Skip, &r.Errors, &r.Waived,
		&r.Blocking, &r.Warning, &r.Info,
		&r.Truncated, &r.Trigger, &logText, &started, &finished, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return ComplianceRunRow{}, ErrNotFound
	}
	if err != nil {
		return ComplianceRunRow{}, fmt.Errorf("read compliance run: %w", err)
	}
	r.Error = errText.String
	// A transcript that will not decode is dropped rather than failing the read:
	// the run's results are the answer, and the log is how it was reached.
	if logText.Valid && logText.String != "" {
		_ = json.Unmarshal([]byte(logText.String), &r.Log)
	}
	if t := parseComplianceTime(started); t != nil {
		r.StartedAt = *t
	}
	r.FinishedAt = parseComplianceTime(finished)
	r.HeartbeatAt = parseComplianceTime(heartbeat)
	return r, nil
}

// ComplianceRuns lists a package's history, newest first.
func (p *Packages) ComplianceRuns(ctx context.Context, packageID int64, limit int) ([]ComplianceRunRow, error) {
	if limit <= 0 {
		limit = 20
	}
	d := p.dialect
	rows, err := p.db.QueryContext(ctx, d.Rewrite(`
		SELECT id, state, COALESCE(error, ''), verdict, bundle_digest,
		       blocking_count, warning_count, error_count, pass_count, fail_count,
		       trigger, `+d.TimestampText("started_at")+`, `+d.TimestampText("finished_at")+`
		  FROM compliance_runs
		 WHERE package_id = ?
		 ORDER BY started_at DESC
		 LIMIT ?`), packageID, limit)
	if err != nil {
		return nil, fmt.Errorf("list compliance runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ComplianceRunRow
	for rows.Next() {
		var (
			r        ComplianceRunRow
			errText  sql.NullString
			started  string
			finished string
		)
		if err := rows.Scan(&r.ID, &r.State, &errText, &r.Verdict, &r.BundleDigest,
			&r.Blocking, &r.Warning, &r.Errors, &r.Pass, &r.Fail,
			&r.Trigger, &started, &finished); err != nil {
			return nil, fmt.Errorf("scan compliance run: %w", err)
		}
		r.PackageID = packageID
		r.Error = errText.String
		if t := parseComplianceTime(started); t != nil {
			r.StartedAt = *t
		}
		r.FinishedAt = parseComplianceTime(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ComplianceCharts returns what each chart contributed to a run.
func (p *Packages) ComplianceCharts(ctx context.Context, runID string) ([]ComplianceChartRow, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT name, version, artifact_digest, artifact_ref, status, COALESCE(error, ''),
		       error_kind, attempts, resources
		  FROM compliance_charts
		 WHERE run_id = ?
		 ORDER BY status DESC, name, version`), runID)
	if err != nil {
		return nil, fmt.Errorf("list compliance charts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ComplianceChartRow
	for rows.Next() {
		var c ComplianceChartRow
		if err := rows.Scan(&c.Name, &c.Version, &c.ArtifactDigest, &c.ArtifactRef,
			&c.Status, &c.Error, &c.ErrorKind, &c.Attempts, &c.Resources); err != nil {
			return nil, fmt.Errorf("scan compliance chart: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ComplianceRenderedIndex lists the documents a run kept, WITHOUT their content.
//
// Without, deliberately: the index is what a page renders beside the coverage
// table, and the content is megabytes. A listing that carried it would make
// opening the tab as expensive as downloading every chart.
func (p *Packages) ComplianceRenderedIndex(ctx context.Context, runID string) ([]ComplianceRenderedRow, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT seq, chart, chart_version, source_file, line_count, byte_count, truncated
		  FROM compliance_rendered
		 WHERE run_id = ?
		 ORDER BY chart, source_file, seq`), runID)
	if err != nil {
		return nil, fmt.Errorf("list compliance evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ComplianceRenderedRow
	for rows.Next() {
		var d ComplianceRenderedRow
		if err := rows.Scan(&d.Seq, &d.Chart, &d.ChartVersion, &d.SourceFile,
			&d.Lines, &d.Bytes, &d.Truncated); err != nil {
			return nil, fmt.Errorf("scan compliance evidence: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ComplianceRendered returns one document with its content, or ErrNotFound.
//
// `key` is a chart name or the path of a manifest the release ships as-is, and
// both are matched because those are the two ways a document is named and a
// caller holding a result's address knows only which one it has.
func (p *Packages) ComplianceRendered(ctx context.Context, runID, key string) (ComplianceRenderedRow, error) {
	var d ComplianceRenderedRow
	err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT seq, chart, chart_version, source_file, content, line_count, byte_count, truncated
		  FROM compliance_rendered
		 WHERE run_id = ? AND (chart = ? OR (chart = '' AND source_file = ?))
		 ORDER BY seq LIMIT 1`), runID, key, key).
		Scan(&d.Seq, &d.Chart, &d.ChartVersion, &d.SourceFile, &d.Content,
			&d.Lines, &d.Bytes, &d.Truncated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("read compliance evidence %q: %w", key, err)
	}
	return d, nil
}

// ComplianceRenderedAll returns every document of a run, content included.
//
// The one caller is the download of a whole release's rendered manifests, which
// is a deliberate single large read: what a vendor is sent is one file, and
// assembling it from n round trips would be slower and could interleave with a
// re-check that replaced half of them.
func (p *Packages) ComplianceRenderedAll(ctx context.Context, runID string) ([]ComplianceRenderedRow, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT seq, chart, chart_version, source_file, content, line_count, byte_count, truncated
		  FROM compliance_rendered
		 WHERE run_id = ?
		 ORDER BY chart, source_file, seq`), runID)
	if err != nil {
		return nil, fmt.Errorf("read compliance evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ComplianceRenderedRow
	for rows.Next() {
		var d ComplianceRenderedRow
		if err := rows.Scan(&d.Seq, &d.Chart, &d.ChartVersion, &d.SourceFile, &d.Content,
			&d.Lines, &d.Bytes, &d.Truncated); err != nil {
			return nil, fmt.Errorf("scan compliance evidence: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ComplianceResults returns a page of results, filtered.
//
// Total is the count BEFORE the page is taken, because a reader needs to know
// whether they are looking at 40 findings or the first 40 of 900. A page with
// no total is a page that lies by omission.
func (p *Packages) ComplianceResults(ctx context.Context, runID string, f ComplianceFilter) ([]ComplianceResultRow, int, error) {
	where, args := complianceWhere(runID, f)

	var total int
	if err := p.db.QueryRowContext(ctx,
		p.dialect.Rewrite(`SELECT COUNT(*) FROM compliance_results `+where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count compliance results: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	// Ordered by seq, which the engine assigned in reading order: failures
	// first, then the undecidable, then waivers, passes and skips, and within
	// each by chart, file and resource. Sorting in SQL by those columns instead
	// would reproduce that ordering approximately and differently.
	query := p.dialect.Rewrite(`
		SELECT seq, check_id, check_title, severity, tier, category, pack,
		       remediation, reference, outcome, determinacy,
		       chart, chart_version, subchart_path, artifact_digest, artifact_ref,
		       source_file, rendered_line, api_version, kind, namespace, name,
		       container, container_type, locus,
		       observed, expected, message, error, waiver, ` +
		p.dialect.TimestampText("waiver_expires") + `, fingerprint
		  FROM compliance_results ` + where + `
		 ORDER BY seq
		 LIMIT ? OFFSET ?`)

	rows, err := p.db.QueryContext(ctx, query, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list compliance results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ComplianceResultRow
	for rows.Next() {
		var (
			r       ComplianceResultRow
			expires string
		)
		if err := rows.Scan(&r.Seq, &r.CheckID, &r.CheckTitle, &r.Severity, &r.Tier,
			&r.Category, &r.Pack, &r.Remediation, &r.Reference, &r.Outcome, &r.Determinacy,
			&r.Chart, &r.ChartVersion, &r.SubchartPath, &r.ArtifactDigest, &r.ArtifactRef,
			&r.SourceFile, &r.RenderedLine, &r.APIVersion, &r.Kind, &r.Namespace, &r.Name,
			&r.Container, &r.ContainerType, &r.Locus,
			&r.Observed, &r.Expected, &r.Message, &r.Error,
			&r.Waiver, &expires, &r.Fingerprint); err != nil {
			return nil, 0, fmt.Errorf("scan compliance result: %w", err)
		}
		r.WaiverExpires = parseComplianceTime(expires)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// complianceWhere builds the filter clause.
//
// Every value is a placeholder. A filter arrives from a query string, and a
// filter interpolated into SQL is the same mistake as a digest taken from a
// URL - it is just harder to see because the values look like enum members.
func complianceWhere(runID string, f ComplianceFilter) (string, []any) {
	clauses := []string{"run_id = ?"}
	args := []any{runID}

	in := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		marks := make([]string, len(values))
		for i, v := range values {
			marks[i] = "?"
			args = append(args, v)
		}
		clauses = append(clauses, column+" IN ("+strings.Join(marks, ",")+")")
	}
	in("outcome", f.Outcomes)
	in("severity", f.Severities)
	in("check_id", f.Checks)
	in("chart", f.Charts)
	in("kind", f.Kinds)
	in("determinacy", f.Determinacy)

	if f.Seq != nil {
		clauses = append(clauses, "seq = ?")
		args = append(args, *f.Seq)
	}

	if s := strings.TrimSpace(f.Search); s != "" {
		// Matched against the fields a person types into a search box while
		// reading a report: the object's name, the file, the check, the
		// sentence. Not the remediation - that is the same text on hundreds of
		// rows and would match everything.
		like := "%" + strings.ToLower(s) + "%"
		clauses = append(clauses,
			`(LOWER(name) LIKE ? OR LOWER(source_file) LIKE ? OR LOWER(check_id) LIKE ?
			  OR LOWER(message) LIKE ? OR LOWER(chart) LIKE ? OR LOWER(container) LIKE ?)`)
		args = append(args, like, like, like, like, like, like)
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

// PackageComplianceRow is the listing summary.
type PackageComplianceRow struct {
	PackageID int64
	RunID     string
	State     string
	Verdict   string
	Blocking  int
	Warning   int
	Errors    int
	Pass      int
	CheckedAt *time.Time
}

// PackageCompliance returns the summary for a set of packages.
//
// Absent means NOT CHECKED, and the interface must render it as such - never
// as a pass. That distinction is the whole of Rule 2 applied to a listing
// column, and it is the one a reader is most likely to get wrong at a glance.
func (p *Packages) PackageCompliance(ctx context.Context, packageIDs []int64) (map[int64]PackageComplianceRow, error) {
	out := map[int64]PackageComplianceRow{}
	if len(packageIDs) == 0 {
		return out, nil
	}
	marks := make([]string, len(packageIDs))
	args := make([]any, len(packageIDs))
	for i, id := range packageIDs {
		marks[i] = "?"
		args[i] = id
	}
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT package_id, COALESCE(run_id, ''), state, verdict,
		       blocking_count, warning_count, error_count, pass_count, `+
		p.dialect.TimestampText("checked_at")+`
		  FROM package_compliance
		 WHERE package_id IN (`+strings.Join(marks, ",")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read package compliance: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			r       PackageComplianceRow
			checked string
		)
		if err := rows.Scan(&r.PackageID, &r.RunID, &r.State, &r.Verdict,
			&r.Blocking, &r.Warning, &r.Errors, &r.Pass, &checked); err != nil {
			return nil, fmt.Errorf("scan package compliance: %w", err)
		}
		r.CheckedAt = parseComplianceTime(checked)
		out[r.PackageID] = r
	}
	return out, rows.Err()
}

// ReleaseStaleComplianceRuns frees claims held by Coordinators that died.
//
// Without this a release whose Coordinator was killed mid-run is stuck
// "running" forever and can never be checked again - the state nobody can get
// out of without a database console.
func (p *Packages) ReleaseStaleComplianceRuns(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := securityTime(time.Now().UTC().Add(-olderThan))
	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE compliance_runs
		   SET state = 'failed',
		       error = 'the Coordinator running this check stopped responding; the claim was released',
		       finished_at = ?, heartbeat_at = NULL
		 WHERE state = 'running' AND heartbeat_at < ?`), securityTime(time.Now().UTC()), cutoff)
	if err != nil {
		return 0, fmt.Errorf("release stale compliance runs: %w", err)
	}
	n, _ := res.RowsAffected()

	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE package_compliance
		   SET state = 'failed'
		 WHERE state = 'running'
		   AND run_id IN (SELECT id FROM compliance_runs WHERE state = 'failed')`)); err != nil {
		return int(n), fmt.Errorf("release stale compliance summaries: %w", err)
	}
	return int(n), nil
}

// parseComplianceTime reads a timestamp rendered by Dialect.TimestampText.
//
// An unparseable value becomes absent rather than an error: a timestamp this
// cannot read is a missing timestamp, and failing a whole report because one
// column is in an unexpected format is a worse answer than showing the report
// without a date on it.
func parseComplianceTime(s string) *time.Time {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	t, err := parseSecurityTime(s)
	if err != nil {
		return nil
	}
	return &t
}
