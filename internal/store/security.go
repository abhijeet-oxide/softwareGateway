package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Security is the platform's cache of what a scanner said.
//
// See db/migrations/*/00022_security_findings.sql for the argument behind the
// two tiers. The short version: the lightweight tier is kept because a package
// listing cannot query Xray about 157 artifacts to render a column, and the
// complete tier expires quickly because Xray is the source of truth for
// detailed findings and this platform is deliberately not a second one.
//
// Every method takes a scope and every statement filters on it. That is an
// authorization boundary, not a filing convention: findings were retrieved
// under one repository's Xray permissions, and serving them to a request
// scoped elsewhere would disclose a security posture the asker was never
// entitled to.
type Security struct {
	db      *sql.DB
	dialect Dialect
}

// NewSecurity builds the security store.
func NewSecurity(s Store) *Security {
	return &Security{db: s.DB(), dialect: DialectFor(s.Driver())}
}

func (s *Security) q(query string) string { return s.dialect.Rewrite(query) }

// LoadSummaries implements security.Cache.
//
// Expired rows are not returned, and that is a filter rather than a delete: a
// sweeper removes them (see Sweep), and a read that deleted would turn a page
// render into a write transaction.
func (s *Security) LoadSummaries(ctx context.Context, scope security.Scope, refs []security.ArtifactRef) (map[string]security.Report, error) {
	if len(refs) == 0 {
		return map[string]security.Report{}, nil
	}

	out := make(map[string]security.Report, len(refs))
	for _, chunk := range chunkRefs(refs, sqlChunk) {
		args := []any{scope.Product, scope.Repository, scope.Provider}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref.Ref())
		}

		query := s.q(`
			SELECT artifact_ref, status, message, provider,
			       total, fixable, critical, high, medium, low, unknown,
			       fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
			       scanned_at, retrieved_at
			  FROM security_scans
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (` + strings.Join(placeholders, ",") + `)
			   AND expires_at > ` + s.dialect.Now())

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("load security summaries: %w", err)
		}
		if err := scanSummaryRows(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LoadDetails implements security.Cache: the complete reports, while they last.
func (s *Security) LoadDetails(ctx context.Context, scope security.Scope, refs []security.ArtifactRef) (map[string]security.Report, error) {
	if len(refs) == 0 {
		return map[string]security.Report{}, nil
	}

	out := make(map[string]security.Report, len(refs))
	for _, chunk := range chunkRefs(refs, sqlChunk) {
		args := []any{scope.Product, scope.Repository, scope.Provider}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref.Ref())
		}

		query := s.q(`
			SELECT artifact_ref, payload
			  FROM security_details
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (` + strings.Join(placeholders, ",") + `)
			   AND expires_at > ` + s.dialect.Now())

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("load security details: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var ref string
				var payload []byte
				if err := rows.Scan(&ref, &payload); err != nil {
					return fmt.Errorf("scan security detail: %w", err)
				}
				var report security.Report
				if err := json.Unmarshal(payload, &report); err != nil {
					// A row we cannot read is a cache miss, not a failure. The
					// provider is right there and the answer is recoverable.
					continue
				}
				out[ref] = report
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Save implements security.Cache.
//
// # Why a counts-only retrieval must not touch the detail tier
//
// A package listing asks for counts. If that write cleared the detail rows, the
// next person to open a release's findings would re-query Xray for all 157
// artifacts - and the listing that caused it renders a column. The `detail`
// flag is what keeps a cheap read from invalidating an expensive one.
func (s *Security) Save(ctx context.Context, scope security.Scope, reports []security.Report, detail bool, ttl security.CacheTTL) error {
	if len(reports) == 0 {
		return nil
	}

	summaryTTL := ttl.Summary
	if summaryTTL <= 0 {
		summaryTTL = 6 * time.Hour
	}
	detailTTL := ttl.Detail
	if detailTTL <= 0 {
		detailTTL = 15 * time.Minute
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin security cache write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for _, r := range reports {
		// A disabled report is a fact about configuration, and caching it would
		// outlive the configuration change that fixes it.
		if r.Status == security.StatusDisabled {
			continue
		}
		scanID, err := s.saveScan(ctx, tx, scope, r, now.Add(summaryTTL))
		if err != nil {
			return err
		}
		if detail {
			if err := s.saveFindings(ctx, tx, scanID, r.Findings); err != nil {
				return err
			}
			if err := s.saveDetail(ctx, tx, scope, r, now.Add(detailTTL)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// saveScan upserts one artifact's summary row and returns its id.
func (s *Security) saveScan(ctx context.Context, tx *sql.Tx, scope security.Scope, r security.Report, expires time.Time) (int64, error) {
	c := r.Counts
	provider := r.Provider
	if provider == "" {
		provider = scope.Provider
	}

	// Upsert with the same key the unique index uses. The re-read afterwards is
	// what makes this portable: SQLite and Postgres disagree about RETURNING on
	// an upsert, and a second SELECT is cheap against a unique index.
	_, err := tx.ExecContext(ctx, s.q(`
		INSERT INTO security_scans (
			product, repository, role, provider,
			artifact_ref, artifact_key, artifact_tag, artifact_kind, artifact_repo,
			status, message,
			total, fixable, critical, high, medium, low, unknown,
			fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
			scanned_at, retrieved_at, fingerprint, expires_at)
		VALUES (?,?,?,?, ?,?,?,?,?, ?,?, ?,?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?)
		ON CONFLICT (product, repository, provider, artifact_ref) DO UPDATE SET
			role = excluded.role,
			artifact_key = excluded.artifact_key,
			artifact_tag = excluded.artifact_tag,
			artifact_kind = excluded.artifact_kind,
			artifact_repo = excluded.artifact_repo,
			status = excluded.status,
			message = excluded.message,
			total = excluded.total, fixable = excluded.fixable,
			critical = excluded.critical, high = excluded.high,
			medium = excluded.medium, low = excluded.low, unknown = excluded.unknown,
			fix_critical = excluded.fix_critical, fix_high = excluded.fix_high,
			fix_medium = excluded.fix_medium, fix_low = excluded.fix_low,
			fix_unknown = excluded.fix_unknown,
			scanned_at = excluded.scanned_at,
			retrieved_at = excluded.retrieved_at,
			fingerprint = excluded.fingerprint,
			expires_at = excluded.expires_at`),
		scope.Product, scope.Repository, roleOr(scope.Role), provider,
		r.Artifact.Ref(), r.Artifact.ArtifactKey(), r.Artifact.Tag, r.Artifact.Kind, r.Artifact.Repository,
		string(r.Status), r.Message,
		c.Total, c.Fixable,
		c.BySeverity.Critical, c.BySeverity.High, c.BySeverity.Medium, c.BySeverity.Low, c.BySeverity.Unknown,
		c.FixableBySeverity.Critical, c.FixableBySeverity.High, c.FixableBySeverity.Medium,
		c.FixableBySeverity.Low, c.FixableBySeverity.Unknown,
		timeOrNil(r.ScannedAt), securityTime(r.RetrievedAt), fingerprintOf(r), securityTime(expires))
	if err != nil {
		return 0, fmt.Errorf("save security scan: %w", err)
	}

	var id int64
	err = tx.QueryRowContext(ctx, s.q(`
		SELECT id FROM security_scans
		 WHERE product = ? AND repository = ? AND provider = ? AND artifact_ref = ?`),
		scope.Product, scope.Repository, provider, r.Artifact.Ref()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("read back security scan: %w", err)
	}
	return id, nil
}

// saveFindings replaces the index rows for one artifact.
//
// Delete-then-insert rather than a merge: a re-scan that RESOLVED a finding
// must remove its row, and a merge that only upserts would leave the resolved
// finding in the index forever - a search for it would keep naming an image
// that no longer has it, which is worse than not having a search.
func (s *Security) saveFindings(ctx context.Context, tx *sql.Tx, scanID int64, findings []security.Finding) error {
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM security_findings WHERE scan_id = ?`), scanID); err != nil {
		return fmt.Errorf("clear security findings: %w", err)
	}
	if len(findings) == 0 {
		return nil
	}

	stmt, err := tx.PrepareContext(ctx, s.q(`
		INSERT INTO security_findings (
			scan_id, cve, issue_id, severity, fixable,
			component_id, component_name, component_version, component_type,
			fixed_in, summary)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (scan_id, cve, issue_id, component_id) DO NOTHING`))
	if err != nil {
		return fmt.Errorf("prepare security finding insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, f := range findings {
		fixedIn := ""
		if len(f.FixedIn) > 0 {
			fixedIn = f.FixedIn[0]
		}
		severity := f.Severity
		if !severity.Valid() {
			severity = security.SeverityUnknown
		}
		if _, err := stmt.ExecContext(ctx,
			scanID, f.CVE, f.ID, string(severity), f.Fixable,
			f.Component.ID, f.Component.Name, f.Component.Version, f.Component.Type,
			fixedIn, truncate(f.Summary, 500),
		); err != nil {
			return fmt.Errorf("save security finding: %w", err)
		}
	}
	return nil
}

// saveDetail stores the complete normalized report.
func (s *Security) saveDetail(ctx context.Context, tx *sql.Tx, scope security.Scope, r security.Report, expires time.Time) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode security detail: %w", err)
	}
	provider := r.Provider
	if provider == "" {
		provider = scope.Provider
	}
	_, err = tx.ExecContext(ctx, s.q(`
		INSERT INTO security_details (product, repository, provider, artifact_ref,
		                              payload, fingerprint, retrieved_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (product, repository, provider, artifact_ref) DO UPDATE SET
			payload = excluded.payload,
			fingerprint = excluded.fingerprint,
			retrieved_at = excluded.retrieved_at,
			expires_at = excluded.expires_at`),
		scope.Product, scope.Repository, provider, r.Artifact.Ref(),
		payload, fingerprintOf(r), securityTime(r.RetrievedAt), securityTime(expires))
	if err != nil {
		return fmt.Errorf("save security detail: %w", err)
	}
	return nil
}

// Invalidate implements security.Cache: drop both tiers for these artifacts.
//
// What "refresh" means. The detail rows go with the summary rows because a
// refresh that left stale details behind would show a user a refreshed count
// over an unrefreshed list, which is worse than either.
func (s *Security) Invalidate(ctx context.Context, scope security.Scope, refs []security.ArtifactRef) error {
	if len(refs) == 0 {
		return nil
	}
	for _, chunk := range chunkRefs(refs, sqlChunk) {
		args := []any{scope.Product, scope.Repository, scope.Provider}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref.Ref())
		}
		in := " AND artifact_ref IN (" + strings.Join(placeholders, ",") + ")"

		for _, table := range []string{"security_scans", "security_details"} {
			query := s.q(`DELETE FROM ` + table +
				` WHERE product = ? AND repository = ? AND provider = ?` + in)
			if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("invalidate %s: %w", table, err)
			}
		}
	}
	return nil
}

// Sweep removes expired rows.
//
// Both tiers, and the finding index goes with the scans it belongs to via the
// cascade. Called from the maintenance loop rather than from a read, because a
// page render must not be a write transaction.
func (s *Security) Sweep(ctx context.Context) (scans, details int64, err error) {
	res, err := s.db.ExecContext(ctx, s.q(
		`DELETE FROM security_scans WHERE expires_at <= `+s.dialect.Now()))
	if err != nil {
		return 0, 0, fmt.Errorf("sweep security scans: %w", err)
	}
	scans, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx, s.q(
		`DELETE FROM security_details WHERE expires_at <= `+s.dialect.Now()))
	if err != nil {
		return scans, 0, fmt.Errorf("sweep security details: %w", err)
	}
	details, _ = res.RowsAffected()
	return scans, details, nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// SearchKind is what a search is over.
type SearchKind string

const (
	SearchCVE       SearchKind = "cve"
	SearchComponent SearchKind = "package"
	SearchImage     SearchKind = "image"
)

// SearchFilter is one search.
type SearchFilter struct {
	// Product scopes the search. NEVER empty in a served request: a search
	// without it would cross the authorization boundary every other statement
	// in this file is careful to respect.
	Product string
	Kind    SearchKind
	Query   string
	// Exact requires a whole-value match. Otherwise the query is matched as a
	// prefix and as a contained substring, which is what makes "openssl" find
	// "libssl-dev" nowhere and "openssl1.1" here.
	Exact bool
	Limit int
}

// SearchHit is one row of a search result: a finding, on an artifact, with
// everything needed to navigate outward from it.
type SearchHit struct {
	CVE              string
	IssueID          string
	Severity         string
	Fixable          bool
	Summary          string
	ComponentID      string
	ComponentName    string
	ComponentVersion string
	ComponentType    string
	FixedIn          string

	ArtifactRef  string
	ArtifactKey  string
	ArtifactTag  string
	ArtifactKind string
	ArtifactRepo string

	Provider   string
	Repository string
	ScannedAt  string
}

// Search answers the three questions the interface asks: which images have this
// CVE, what is wrong with this package, and what is wrong with this image.
//
// It reads the INDEX tier, never the provider. That is the whole reason the
// index exists: a search that queried Xray would take a scanner round trip per
// artifact per keystroke, and a search that read the short-lived detail cache
// would answer differently depending on what somebody happened to have opened
// recently.
func (s *Security) Search(ctx context.Context, f SearchFilter) ([]SearchHit, error) {
	if strings.TrimSpace(f.Query) == "" {
		return nil, nil
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	var where string
	var args []any
	args = append(args, f.Product)

	q := strings.TrimSpace(f.Query)
	switch f.Kind {
	case SearchCVE:
		if f.Exact {
			where = "AND UPPER(v.cve) = ?"
			args = append(args, strings.ToUpper(q))
		} else {
			where = "AND UPPER(v.cve) LIKE ?"
			args = append(args, "%"+strings.ToUpper(q)+"%")
		}
	case SearchComponent:
		if f.Exact {
			where = "AND LOWER(v.component_name) = ?"
			args = append(args, strings.ToLower(q))
		} else {
			where = "AND LOWER(v.component_name) LIKE ?"
			args = append(args, "%"+strings.ToLower(q)+"%")
		}
	case SearchImage:
		if f.Exact {
			where = "AND (LOWER(sc.artifact_key) = ? OR LOWER(sc.artifact_ref) = ? OR LOWER(sc.artifact_repo) = ?)"
			args = append(args, strings.ToLower(q), strings.ToLower(q), strings.ToLower(q))
		} else {
			like := "%" + strings.ToLower(q) + "%"
			where = "AND (LOWER(sc.artifact_key) LIKE ? OR LOWER(sc.artifact_ref) LIKE ? OR " +
				"LOWER(sc.artifact_repo) LIKE ? OR LOWER(sc.artifact_tag) LIKE ?)"
			args = append(args, like, like, like, like)
		}
	default:
		return nil, fmt.Errorf("unknown search kind %q", f.Kind)
	}

	args = append(args, limit)
	query := s.q(`
		SELECT v.cve, v.issue_id, v.severity, v.fixable, v.summary,
		       v.component_id, v.component_name, v.component_version, v.component_type, v.fixed_in,
		       sc.artifact_ref, sc.artifact_key, sc.artifact_tag, sc.artifact_kind, sc.artifact_repo,
		       sc.provider, sc.repository, COALESCE(sc.scanned_at, '')
		  FROM security_findings v
		  JOIN security_scans sc ON sc.id = v.scan_id
		 WHERE sc.product = ? ` + where + `
		 ORDER BY
		   CASE v.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1
		                   WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END,
		   v.cve, sc.artifact_key
		 LIMIT ?`)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("security search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.CVE, &h.IssueID, &h.Severity, &h.Fixable, &h.Summary,
			&h.ComponentID, &h.ComponentName, &h.ComponentVersion, &h.ComponentType, &h.FixedIn,
			&h.ArtifactRef, &h.ArtifactKey, nullString(&h.ArtifactTag), nullString(&h.ArtifactKind),
			nullString(&h.ArtifactRepo), &h.Provider, &h.Repository, &h.ScannedAt,
		); err != nil {
			return nil, fmt.Errorf("scan security search row: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReleasesFor lists the releases that contain a given artifact, so a search
// result can navigate from an image to the releases shipping it.
//
// Joins the existing package tree rather than keeping a second copy of it:
// which artifacts a release contains is the core's knowledge, recorded by
// discovery, and duplicating it here would be a second thing to keep in step.
func (s *Security) ReleasesFor(ctx context.Context, productName string, artifactRefs []string) (map[string][]ReleaseRef, error) {
	if len(artifactRefs) == 0 {
		return map[string][]ReleaseRef{}, nil
	}

	out := map[string][]ReleaseRef{}
	for _, chunk := range chunkStrings(artifactRefs, sqlChunk) {
		args := []any{productName}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref)
		}

		query := s.q(`
			SELECT pa.digest, p.id, p.tag, p.manifest_digest, COALESCE(p.display_tag, '')
			  FROM package_artifacts pa
			  JOIN packages p ON p.id = pa.package_id
			  JOIN products pr ON pr.id = p.product_id
			 WHERE pr.name = ? AND pr.active = ` + s.dialect.Bool(true) + `
			   AND pa.digest IN (` + strings.Join(placeholders, ",") + `)
			 ORDER BY p.discovered_at DESC`)

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("releases for artifacts: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var digest, displayTag string
				var r ReleaseRef
				if err := rows.Scan(&digest, &r.PackageID, &r.Tag, &r.Digest, &displayTag); err != nil {
					return fmt.Errorf("scan release row: %w", err)
				}
				r.DisplayTag = displayTag
				out[digest] = append(out[digest], r)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReleaseRef names one release a search result belongs to.
type ReleaseRef struct {
	PackageID  int64
	Tag        string
	DisplayTag string
	Digest     string
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// sqlChunk bounds how many placeholders one IN clause carries. Postgres refuses
// past 65535 parameters and SQLite has a lower default; a release of a few
// hundred artifacts fits in one chunk and a catalogue-wide query still works.
const sqlChunk = 400

func chunkRefs(in []security.ArtifactRef, size int) [][]security.ArtifactRef {
	var out [][]security.ArtifactRef
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

func chunkStrings(in []string, size int) [][]string {
	var out [][]string
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

func scanSummaryRows(rows *sql.Rows, into map[string]security.Report) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ref, status, provider string
			message               sql.NullString
			c                     security.Counts
			scannedAt             sql.NullString
			retrievedAt           string
		)
		if err := rows.Scan(&ref, &status, &message, &provider,
			&c.Total, &c.Fixable,
			&c.BySeverity.Critical, &c.BySeverity.High, &c.BySeverity.Medium,
			&c.BySeverity.Low, &c.BySeverity.Unknown,
			&c.FixableBySeverity.Critical, &c.FixableBySeverity.High, &c.FixableBySeverity.Medium,
			&c.FixableBySeverity.Low, &c.FixableBySeverity.Unknown,
			&scannedAt, &retrievedAt); err != nil {
			return fmt.Errorf("scan security summary: %w", err)
		}
		c.NonFixable = c.Total - c.Fixable

		r := security.Report{
			Status:   security.Status(status),
			Provider: provider,
			Message:  message.String,
			Counts:   c,
		}
		if scannedAt.Valid && scannedAt.String != "" {
			if t, err := parseSecurityTime(scannedAt.String); err == nil {
				u := t.UTC()
				r.ScannedAt = &u
			}
		}
		if t, err := parseSecurityTime(retrievedAt); err == nil {
			r.RetrievedAt = t.UTC()
		}
		into[ref] = r
	}
	return rows.Err()
}

// securityTime renders a timestamp both dialects compare correctly.
//
// A FIXED-WIDTH UTC form, always, and never RFC3339 with an offset. SQLite
// stores timestamps as TEXT and compares them lexically, so a value carrying
// "+02:00" sorts wrong against the expiry filter - which would silently either
// never expire a row or expire every row, and neither failure announces itself.
//
// Distinct from the package's formatTime, which answers "" for a zero time.
// That is right for a nullable column and wrong for expires_at, which is NOT
// NULL and would fail the insert with a message about a constraint rather than
// about the zero value that caused it.
func securityTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return securityTime(*t)
}

func fingerprintOf(r security.Report) string {
	return security.FingerprintReports([]security.Report{r})
}

// parseSecurityTime reads back what securityTime wrote, tolerating the RFC3339
// forms Postgres returns for the same column.
func parseSecurityTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

func roleOr(role string) string {
	if role == "" {
		return "source"
	}
	return role
}

// nullString adapts a NULL-able TEXT column onto a plain string field.
func nullString(dst *string) sql.Scanner { return &nullStringScanner{dst} }

type nullStringScanner struct{ dst *string }

func (n *nullStringScanner) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*n.dst = ""
	case string:
		*n.dst = t
	case []byte:
		*n.dst = string(t)
	default:
		*n.dst = fmt.Sprint(t)
	}
	return nil
}

// SortHits orders search results worst first, then by identifier, so two
// searches over the same data produce the same page.
func SortHits(hits []SearchHit) {
	rank := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "unknown": 4}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		if a.CVE != b.CVE {
			return a.CVE < b.CVE
		}
		return a.ArtifactKey < b.ArtifactKey
	})
}

var _ security.Cache = (*Security)(nil)
