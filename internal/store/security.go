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

// Security is the platform's store of what a scanner said.
//
// See db/migrations/*/00022_security_findings.sql for the argument behind the
// tiers, and 00033_security_cache.sql for why they are no longer governed by
// expiry. The short version: the lightweight tier is kept because a package
// listing cannot query Xray about 157 artifacts to render a column; the heavy
// tiers are kept because regenerating them costs a scanner round trip per
// image; and NOTHING is deleted on a clock, because a page that silently
// emptied itself overnight is worse than one showing an answer with its age
// written next to it.
//
// Rows carry `evictable_at` - the point after which they MAY be reclaimed - and
// `last_used_at`. Reads serve them whatever their age. Eviction is a size
// decision taken by the sweeper against a byte budget, oldest-unread first, and
// it never runs while the store is inside that budget.
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
// Age is NOT a filter here. A summary row is the durable result of a sync, and
// hiding it because a clock passed turns "synced three days ago" into "never
// synced" - which is the one distinction this whole package exists to keep. The
// row's retrieved_at travels with it and the interface says how old it is.
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
			       scanned_at, retrieved_at, missing
			  FROM security_scans
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (` + strings.Join(placeholders, ",") + `)`)

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

// LoadDetails implements security.Cache: the complete reports.
//
// Payloads are stored compressed (see decodePayload), and the codec travels in
// its own column rather than being sniffed: a JSON body that happens to start
// with the gzip magic is not a thing, but a store that guesses is a store that
// will one day be wrong about somebody's findings.
//
// Reading TOUCHES the rows. That is a write on a read path, which is normally
// the wrong trade - but eviction is least-recently-USED and a cache that cannot
// tell which of its rows anybody looks at evicts the hot ones first. One
// batched UPDATE per chunk, best-effort: losing a touch costs a row its place
// in the queue, never its contents.
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
			SELECT artifact_ref, payload, codec
			  FROM security_details
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (` + strings.Join(placeholders, ",") + `)`)

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("load security details: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var ref, codec string
				var payload []byte
				if err := rows.Scan(&ref, &payload, &codec); err != nil {
					return fmt.Errorf("scan security detail: %w", err)
				}
				decoded, err := decodePayload(payload, codec)
				if err != nil {
					continue
				}
				var report security.Report
				if err := json.Unmarshal(decoded, &report); err != nil {
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
		s.touchDetails(ctx, scope, chunk)
	}
	return out, nil
}

// touchDetails records that these rows were read, for the eviction order.
//
// Best-effort by construction: it is called from a read, and a read that failed
// because a bookkeeping UPDATE failed would be a page that will not render
// because the cache could not remember being useful.
func (s *Security) touchDetails(ctx context.Context, scope security.Scope, refs []security.ArtifactRef) {
	if len(refs) == 0 {
		return
	}
	args := []any{securityTime(time.Now().UTC()), scope.Product, scope.Repository, scope.Provider}
	placeholders := make([]string, 0, len(refs))
	for _, ref := range refs {
		placeholders = append(placeholders, "?")
		args = append(args, ref.Ref())
	}
	_, _ = s.db.ExecContext(ctx, s.q(`
		UPDATE security_details SET last_used_at = ?
		 WHERE product = ? AND repository = ? AND provider = ?
		   AND artifact_ref IN (`+strings.Join(placeholders, ",")+`)`), args...)
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

	// The retentions are how long a row is PINNED, not how long it lives. Past
	// them a row becomes evictable and is still served; it goes only when the
	// store is over its budget and this row is the least recently read. The
	// defaults are generous for the same reason: a cache whose default
	// behaviour is to forget is one every deployment has to configure before it
	// is useful.
	summaryTTL := ttl.Summary
	if summaryTTL <= 0 {
		summaryTTL = 30 * 24 * time.Hour
	}
	detailTTL := ttl.Detail
	if detailTTL <= 0 {
		detailTTL = 7 * 24 * time.Hour
	}

	now := time.Now().UTC()
	for start := 0; start < len(reports); start += saveChunk {
		end := start + saveChunk
		if end > len(reports) {
			end = len(reports)
		}
		if err := s.saveBatch(
			ctx, scope, reports[start:end], detail, now.Add(summaryTTL), now.Add(detailTTL),
		); err != nil {
			return err
		}
	}
	return nil
}

// saveChunk is how many artifacts one transaction writes.
//
// # Why the whole release is no longer one transaction
//
// Two reasons, and the second is the one that bites. A release of 260 images
// holds tens of thousands of findings, so one transaction meant a single
// long-running write against the database's only writer, blocking the sweep and
// every other sync behind it. And it was ALL OR NOTHING: a sync that reached
// 95% and lost its connection wrote nothing at all, so the next attempt started
// from an empty cache.
//
// Each artifact's rows stay atomic - its summary, its findings and its detail
// are written together or not at all - which is the consistency that actually
// matters here. What a partial write leaves behind is some artifacts recorded
// and the rest not, which is exactly what the cache is designed to express.
const saveChunk = 25

// findingsPerStatement bounds one multi-row insert.
//
// Two hundred rows is 2,200 placeholders - comfortably inside every driver's
// parameter limit - and it turns a release's ~26,000 individual inserts into
// about 130 statements. That arithmetic is the whole change: the write was
// round-trip bound, not disk bound, and against Postgres it was the slowest
// part of a sync that had already finished talking to the scanner.
const findingsPerStatement = 200

// saveBatch writes one chunk of artifacts in a single transaction.
func (s *Security) saveBatch(
	ctx context.Context, scope security.Scope, reports []security.Report,
	detail bool, summaryExpires, detailExpires time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin security cache write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The artifacts whose findings this chunk rewrites, in the order their rows
	// were written, so the ids read back below can be matched to them.
	var stored []security.Report

	for _, r := range reports {
		// A disabled report is a fact about configuration, and caching it would
		// outlive the configuration change that fixes it.
		if r.Status == security.StatusDisabled {
			continue
		}
		// An artifact the scanner would not ANSWER for is a fact about this
		// attempt, and these rows are shared by every release holding the same
		// artifact. Writing it over a stored result destroyed the answer other
		// releases were reading: a busy Xray returned 209 unavailable during
		// one release's sync, and a second release's page - whose own summary
		// still said 241 vulnerabilities - went to an empty table, because its
		// artifacts' counts and findings had just been replaced with nothing.
		//
		// So a failure fills a gap and never overwrites: an artifact with no
		// stored result gets a row saying the scanner would not answer, and one
		// with a result keeps it until the summary TTL expires it.
		if r.Status == security.StatusUnavailable {
			if err := s.saveUnavailable(ctx, tx, scope, r, summaryExpires); err != nil {
				return err
			}
			continue
		}
		if err := s.saveScan(ctx, tx, scope, r, summaryExpires); err != nil {
			return err
		}
		stored = append(stored, r)
	}

	if !detail || len(stored) == 0 {
		return tx.Commit()
	}

	// ONE read-back for the chunk, not one per artifact. The upsert cannot
	// portably return the id - SQLite and Postgres disagree about RETURNING on
	// a conflict - so the ids are read afterwards, and reading twenty-five at
	// once costs one round trip where reading them one at a time cost
	// twenty-five.
	ids, err := s.scanIDs(ctx, tx, scope, stored)
	if err != nil {
		return err
	}

	if err := s.replaceFindings(ctx, tx, scope, ids, stored); err != nil {
		return err
	}
	for _, r := range stored {
		if err := s.saveDetail(ctx, tx, scope, r, detailExpires); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// scanIDs reads back the row ids for a chunk of artifacts, keyed by ref.
func (s *Security) scanIDs(
	ctx context.Context, tx *sql.Tx, scope security.Scope, reports []security.Report,
) (map[string]int64, error) {
	placeholders := make([]string, 0, len(reports))
	args := []any{scope.Product, scope.Repository}
	for _, r := range reports {
		placeholders = append(placeholders, "?")
		args = append(args, r.Artifact.Ref())
	}

	rows, err := tx.QueryContext(ctx, s.q(`
		SELECT provider, artifact_ref, id FROM security_scans
		 WHERE product = ? AND repository = ?
		   AND artifact_ref IN (`+strings.Join(placeholders, ",")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read back security scans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Keyed by PROVIDER and ref, which is what the unique index is keyed by.
	// One repository answered by two scanners holds two rows for one artifact,
	// and matching on the ref alone would attach one scanner's findings to the
	// other's row.
	out := make(map[string]int64, len(reports))
	for rows.Next() {
		var (
			provider, ref string
			id            int64
		)
		if err := rows.Scan(&provider, &ref, &id); err != nil {
			return nil, fmt.Errorf("scan security scan id: %w", err)
		}
		out[provider+"|"+ref] = id
	}
	return out, rows.Err()
}

// scanKey identifies one artifact's stored scan within a scope.
func scanKey(r security.Report, scope security.Scope) string {
	return providerOf(r, scope) + "|" + r.Artifact.Ref()
}

// providerOf is the scanner that answered, falling back to the scope's.
func providerOf(r security.Report, scope security.Scope) string {
	if r.Provider != "" {
		return r.Provider
	}
	return scope.Provider
}

// replaceFindings rewrites the index rows for a whole chunk of artifacts.
//
// Delete-then-insert rather than a merge, for the reason saveFindings gave: a
// re-scan that RESOLVED a finding must remove its row, and a merge that only
// upserts would leave the resolved finding in the index forever - a search for
// it would keep naming an image that no longer has it, which is worse than not
// having a search.
//
// What changed is the arithmetic, not the semantics. One DELETE for the chunk
// and one INSERT per two hundred findings, where this was a DELETE per artifact
// and an INSERT per finding: on a release with 26,000 findings that is about
// 130 statements instead of 26,000, and the write stops being the slowest part
// of a sync that has already finished talking to the scanner.
func (s *Security) replaceFindings(
	ctx context.Context, tx *sql.Tx, scope security.Scope,
	ids map[string]int64, reports []security.Report,
) error {
	type row struct {
		scanID  int64
		finding security.Finding
	}

	scanIDs := make([]any, 0, len(reports))
	pending := make([]row, 0, len(reports)*8)
	for _, r := range reports {
		id, ok := ids[scanKey(r, scope)]
		if !ok {
			// The row was written a moment ago in this transaction, so this
			// cannot happen - and if it ever does, dropping the findings
			// silently would be a release whose count and whose rows disagree.
			return fmt.Errorf("security cache: no stored scan for %s", r.Artifact.Ref())
		}
		scanIDs = append(scanIDs, id)
		for _, f := range r.Findings {
			pending = append(pending, row{scanID: id, finding: f})
		}
	}
	if len(scanIDs) == 0 {
		return nil
	}

	for start := 0; start < len(scanIDs); start += sqlChunk {
		end := start + sqlChunk
		if end > len(scanIDs) {
			end = len(scanIDs)
		}
		chunk := scanIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		if _, err := tx.ExecContext(ctx, s.q(
			`DELETE FROM security_findings WHERE scan_id IN (`+placeholders+`)`), chunk...,
		); err != nil {
			return fmt.Errorf("clear security findings: %w", err)
		}
	}

	const columns = 11
	values := "(" + strings.TrimSuffix(strings.Repeat("?,", columns), ",") + ")"

	for start := 0; start < len(pending); start += findingsPerStatement {
		end := start + findingsPerStatement
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]

		args := make([]any, 0, len(batch)*columns)
		for _, p := range batch {
			f := p.finding
			fixedIn := ""
			if len(f.FixedIn) > 0 {
				fixedIn = f.FixedIn[0]
			}
			severity := f.Severity
			if !severity.Valid() {
				severity = security.SeverityUnknown
			}
			args = append(args,
				p.scanID, f.CVE, f.ID, string(severity), f.Fixable,
				f.Component.ID, f.Component.Name, f.Component.Version, f.Component.Type,
				fixedIn, truncate(f.Summary, 500))
		}

		if _, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO security_findings (
				scan_id, cve, issue_id, severity, fixable,
				component_id, component_name, component_version, component_type,
				fixed_in, summary)
			VALUES `+strings.TrimSuffix(strings.Repeat(values+",", len(batch)), ",")+`
			ON CONFLICT (scan_id, cve, issue_id, component_id, component_version) DO NOTHING`), args...,
		); err != nil {
			return fmt.Errorf("save security findings: %w", err)
		}
	}
	return nil
}

// saveUnavailable records a scanner failure WITHOUT disturbing a stored result.
//
// DO NOTHING rather than DO UPDATE: see the argument in Save. The row is worth
// writing when there is nothing there - a page that says "the scanner would not
// answer for this" beats one that says the artifact has never been synced - and
// is never worth writing over an answer somebody already has.
func (s *Security) saveUnavailable(
	ctx context.Context, tx *sql.Tx, scope security.Scope, r security.Report, expires time.Time,
) error {
	provider := r.Provider
	if provider == "" {
		provider = scope.Provider
	}
	_, err := tx.ExecContext(ctx, s.q(`
		INSERT INTO security_scans (
			product, repository, role, provider,
			artifact_ref, artifact_key, artifact_tag, artifact_kind, artifact_repo,
			status, message, scanned_at, retrieved_at, fingerprint, evictable_at, last_used_at)
		VALUES (?,?,?,?, ?,?,?,?,?, ?,?, ?,?,?,?,?)
		ON CONFLICT (product, repository, provider, artifact_ref) DO NOTHING`),
		scope.Product, scope.Repository, roleOr(scope.Role), provider,
		r.Artifact.Ref(), r.Artifact.ArtifactKey(), r.Artifact.Tag, r.Artifact.Kind, r.Artifact.Repository,
		string(r.Status), r.Message,
		timeOrNil(r.ScannedAt), securityTime(r.RetrievedAt), fingerprintOf(r),
		securityTime(expires), securityTime(r.RetrievedAt))
	if err != nil {
		return fmt.Errorf("save unavailable security scan: %w", err)
	}
	return nil
}

// saveScan upserts one artifact's summary row.
//
// The id it was given is NOT read back here. It used to be, with a SELECT per
// artifact - and the ids are needed for a whole chunk at once, so they are read
// in one query afterwards. See scanIDs.
func (s *Security) saveScan(ctx context.Context, tx *sql.Tx, scope security.Scope, r security.Report, expires time.Time) error {
	c := r.Counts
	provider := providerOf(r, scope)

	// Upsert with the same key the unique index uses.
	_, err := tx.ExecContext(ctx, s.q(`
		INSERT INTO security_scans (
			product, repository, role, provider,
			artifact_ref, artifact_key, artifact_tag, artifact_kind, artifact_repo,
			status, message,
			total, fixable, critical, high, medium, low, unknown,
			fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
			scanned_at, retrieved_at, fingerprint, evictable_at, missing, last_used_at)
		VALUES (?,?,?,?, ?,?,?,?,?, ?,?, ?,?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?)
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
			evictable_at = excluded.evictable_at,
			missing = excluded.missing,
			last_used_at = excluded.last_used_at`),
		scope.Product, scope.Repository, roleOr(scope.Role), provider,
		r.Artifact.Ref(), r.Artifact.ArtifactKey(), r.Artifact.Tag, r.Artifact.Kind, r.Artifact.Repository,
		string(r.Status), r.Message,
		c.Total, c.Fixable,
		c.BySeverity.Critical, c.BySeverity.High, c.BySeverity.Medium, c.BySeverity.Low, c.BySeverity.Unknown,
		c.FixableBySeverity.Critical, c.FixableBySeverity.High, c.FixableBySeverity.Medium,
		c.FixableBySeverity.Low, c.FixableBySeverity.Unknown,
		timeOrNil(r.ScannedAt), securityTime(r.RetrievedAt), fingerprintOf(r),
		securityTime(expires), r.Missing, securityTime(r.RetrievedAt))
	if err != nil {
		return fmt.Errorf("save security scan: %w", err)
	}
	return nil
}

// saveDetail stores the complete normalized report, compressed.
//
// # Why compression is on by default rather than a knob
//
// Because the payload is JSON full of repeated advisory prose, it stores at
// roughly a tenth of its size, and the cost is microseconds on a path that has
// just spent seconds waiting for a scanner. A deployment that is short of CPU
// and long on disk is not a deployment this platform has met; one that is short
// of disk is every deployment eventually.
//
// The uncompressed size is recorded alongside, because "the cache is 4 GB"
// and "the cache holds 40 GB of scanner output" are both worth being able to
// answer, and only one of them can be measured after the fact.
func (s *Security) saveDetail(ctx context.Context, tx *sql.Tx, scope security.Scope, r security.Report, expires time.Time) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode security detail: %w", err)
	}
	payload, codec := encodePayload(raw)
	provider := r.Provider
	if provider == "" {
		provider = scope.Provider
	}
	_, err = tx.ExecContext(ctx, s.q(`
		INSERT INTO security_details (product, repository, provider, artifact_ref,
		                              payload, codec, bytes, source_bytes,
		                              fingerprint, retrieved_at, evictable_at, last_used_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (product, repository, provider, artifact_ref) DO UPDATE SET
			payload = excluded.payload,
			codec = excluded.codec,
			bytes = excluded.bytes,
			source_bytes = excluded.source_bytes,
			fingerprint = excluded.fingerprint,
			retrieved_at = excluded.retrieved_at,
			evictable_at = excluded.evictable_at,
			last_used_at = excluded.last_used_at`),
		scope.Product, scope.Repository, provider, r.Artifact.Ref(),
		payload, codec, len(payload), len(raw),
		fingerprintOf(r), securityTime(r.RetrievedAt), securityTime(expires),
		securityTime(r.RetrievedAt))
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

		// The documents go with them. A refresh that left last week's raw
		// scanner payload beside this minute's findings would hand somebody an
		// export whose two halves disagree - and the raw half is the one they
		// forward to a customer.
		for _, table := range []string{"security_scans", "security_details", "security_documents"} {
			query := s.q(`DELETE FROM ` + table +
				` WHERE product = ? AND repository = ? AND provider = ?` + in)
			if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("invalidate %s: %w", table, err)
			}
		}
	}
	return nil
}

// Sweep reclaims space, and only when space needs reclaiming.
//
// # What changed, and why the old shape was wrong
//
// This used to be "delete everything past its expiry", run every fifteen
// minutes. That is a correct cache eviction and the wrong policy for a security
// index: it deleted findings nobody had asked it to forget, on a clock, while
// the summary counts those findings backed lived on in `package_security`
// forever - so a release ended up with a count and no rows, and the page behind
// the count went blank. Nothing about that was recoverable without re-running a
// twenty-minute sync against somebody else's scanner.
//
// So the order of questions is now: is anything UNREFERENCED (always go), and
// then is the store OVER ITS BUDGET (evict the least recently read until it is
// not). Inside the budget, nothing is deleted no matter how old it is. That is
// what an operator means by "do not delete until required".
//
// # Why the heavy tiers are evicted first
//
// A document is megabytes and one request rebuilds it. A detail payload is
// kilobytes and one request rebuilds it. A summary row plus its findings is the
// durable result of a whole sync and rebuilding it is minutes of a scanner. So
// the budget is spent in that order, and the index tier is reached only when
// the two heavy tiers have already been emptied - which on any sane budget
// never happens.
func (s *Security) Sweep(ctx context.Context, budget CacheBudget) (SecuritySweepResult, error) {
	var out SecuritySweepResult

	orphans, err := s.dropOrphans(ctx)
	if err != nil {
		return out, err
	}
	out.Orphans = orphans

	usage, err := s.Usage(ctx)
	if err != nil {
		return out, err
	}
	out.Before = usage

	// A budget of zero is "keep everything", not "keep nothing". The failure
	// mode of the other reading is a deployment that upgrades into an empty
	// cache because it never set a number it did not know existed.
	if budget.Bytes <= 0 || usage.Bytes() <= budget.Bytes {
		out.After = usage
		return out, nil
	}

	over := usage.Bytes() - budget.Bytes
	for _, tier := range []struct {
		table  string
		bytes  *int64
		rows   *int64
		evict  *int64
		single bool
	}{
		{table: "security_documents", bytes: &usage.DocumentBytes, rows: &usage.Documents, evict: &out.Documents},
		{table: "security_details", bytes: &usage.DetailBytes, rows: &usage.Details, evict: &out.Details},
	} {
		if over <= 0 {
			break
		}
		freed, rows, err := s.evictLRU(ctx, tier.table, over)
		if err != nil {
			return out, err
		}
		*tier.evict = rows
		*tier.bytes -= freed
		*tier.rows -= rows
		over -= freed
	}

	out.After = usage
	return out, nil
}

// CacheBudget is how much room the security store is allowed to occupy.
//
// One number, over the tiers that are regenerable. The index tier is not in it:
// it is the durable result of a sync and is measured in bytes per artifact, so
// a budget that could evict it would be a budget somebody set by mistake.
type CacheBudget struct {
	// Bytes is the ceiling for the stored (compressed) payload tiers. Zero
	// means no ceiling, which is the default and is deliberate: forgetting is
	// the surprising behaviour and should have to be asked for.
	Bytes int64
}

// CacheUsage is what the store currently occupies.
type CacheUsage struct {
	Scans       int64
	Findings    int64
	Details     int64
	Documents   int64
	DetailBytes int64
	// DocumentBytes is what is STORED. SourceBytes is what it decodes to, which
	// is the number that says whether compression is earning its keep.
	DocumentBytes int64
	SourceBytes   int64
}

// Bytes is the total the budget is measured against.
func (u CacheUsage) Bytes() int64 { return u.DetailBytes + u.DocumentBytes }

// SecuritySweepResult is what one sweep did.
type SecuritySweepResult struct {
	Orphans   int64
	Details   int64
	Documents int64
	Before    CacheUsage
	After     CacheUsage
}

// Freed reports whether the sweep removed anything worth logging.
func (r SecuritySweepResult) Freed() bool { return r.Orphans+r.Details+r.Documents > 0 }

// Usage measures the store.
//
// One query per tier rather than one big union, because the two heavy tables
// answer with a SUM over a column they index and the union would plan as three
// sequential scans to save two round trips.
func (s *Security) Usage(ctx context.Context) (CacheUsage, error) {
	var u CacheUsage

	if err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*) FROM security_scans`)).Scan(&u.Scans); err != nil {
		return u, fmt.Errorf("measure security scans: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*) FROM security_findings`)).Scan(&u.Findings); err != nil {
		return u, fmt.Errorf("measure security findings: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*), COALESCE(SUM(bytes),0), COALESCE(SUM(source_bytes),0)
		   FROM security_details`)).Scan(&u.Details, &u.DetailBytes, &u.SourceBytes); err != nil {
		return u, fmt.Errorf("measure security details: %w", err)
	}
	var docSource int64
	if err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*), COALESCE(SUM(bytes),0), COALESCE(SUM(source_bytes),0)
		   FROM security_documents`)).Scan(&u.Documents, &u.DocumentBytes, &docSource); err != nil {
		return u, fmt.Errorf("measure security documents: %w", err)
	}
	u.SourceBytes += docSource
	return u, nil
}

// dropOrphans removes rows nothing can reach.
//
// The one deletion that needs no budget and no age: a detail or document row
// whose summary row is gone is unreachable by every read path in this file, and
// keeping it costs disk to store an answer nobody can ask for.
func (s *Security) dropOrphans(ctx context.Context) (int64, error) {
	var total int64
	for _, table := range []string{"security_details", "security_documents"} {
		res, err := s.db.ExecContext(ctx, s.q(`
			DELETE FROM `+table+` WHERE NOT EXISTS (
				SELECT 1 FROM security_scans sc
				 WHERE sc.product = `+table+`.product
				   AND sc.repository = `+table+`.repository
				   AND sc.provider = `+table+`.provider
				   AND sc.artifact_ref = `+table+`.artifact_ref)`))
		if err != nil {
			return total, fmt.Errorf("drop orphaned %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// evictLRU removes the least recently read EVICTABLE rows of one tier until it
// has freed `want` bytes, and stops there.
//
// Deleted in bounded passes rather than one statement, because "delete the
// oldest N until the sum reaches X" is a window function on Postgres and a
// different one on SQLite, and a portable loop over a small ordered page is
// both dialects' plan without either's syntax.
//
// A row inside its retention is never taken, however old it is by read time.
// That is what the retention IS now: not a life, a pin.
func (s *Security) evictLRU(ctx context.Context, table string, want int64) (freed, rows int64, err error) {
	const page = 200
	for freed < want {
		type victim struct {
			args  []any
			bytes int64
		}

		// last_used_at is NULL for a row written before it was ever read, and
		// COALESCE puts those first: never-read is the most evictable state
		// there is.
		// A document row is keyed by KIND as well, and deleting all four kinds
		// to reclaim one would throw away an SBOM to make room for a
		// vulnerability payload nobody asked to lose.
		keys, where := evictionKey(table)

		found, err := s.db.QueryContext(ctx, s.q(`
			SELECT `+strings.Join(keys, ", ")+`, bytes
			  FROM `+table+`
			 WHERE evictable_at <= `+s.dialect.Now()+`
			 ORDER BY COALESCE(last_used_at, `+lastResortColumn(table)+`) ASC
			 LIMIT ?`), page)
		if err != nil {
			return freed, rows, fmt.Errorf("select %s to evict: %w", table, err)
		}
		var victims []victim
		err = func() error {
			defer func() { _ = found.Close() }()
			for found.Next() {
				values := make([]string, len(keys))
				dest := make([]any, 0, len(keys)+1)
				for i := range values {
					dest = append(dest, &values[i])
				}
				var size int64
				dest = append(dest, &size)
				if err := found.Scan(dest...); err != nil {
					return err
				}
				args := make([]any, 0, len(values))
				for _, v := range values {
					args = append(args, v)
				}
				victims = append(victims, victim{args: args, bytes: size})
			}
			return found.Err()
		}()
		if err != nil {
			return freed, rows, fmt.Errorf("scan %s to evict: %w", table, err)
		}
		if len(victims) == 0 {
			return freed, rows, nil
		}

		before := freed
		for _, v := range victims {
			res, err := s.db.ExecContext(ctx, s.q(
				`DELETE FROM `+table+` WHERE `+where), v.args...)
			if err != nil {
				return freed, rows, fmt.Errorf("evict from %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			rows += n
			freed += v.bytes
			if freed >= want {
				break
			}
		}
		// A whole page that freed nothing measurable.
		//
		// The migration measures the rows written before there was a column to
		// measure them, so this should not happen - and if it ever does, the
		// alternative is deleting the entire tier to reclaim zero bytes, which
		// is the failure this guard exists to make impossible rather than
		// merely unlikely.
		if freed == before {
			return freed, rows, nil
		}
	}
	return freed, rows, nil
}

// lastResortColumn is the timestamp to fall back on for a row nobody has read.
func lastResortColumn(table string) string {
	if table == "security_documents" {
		return "fetched_at"
	}
	return "retrieved_at"
}

// evictionKey is a tier's primary key, as columns to read and a WHERE to delete
// exactly one row by.
func evictionKey(table string) (columns []string, where string) {
	columns = []string{"product", "repository", "provider", "artifact_ref"}
	if table == "security_documents" {
		columns = append(columns, "kind")
	}
	clauses := make([]string, 0, len(columns))
	for _, c := range columns {
		clauses = append(clauses, c+" = ?")
	}
	return columns, strings.Join(clauses, " AND ")
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
		       sc.provider, sc.repository, ` + s.dialect.TimestampText("sc.scanned_at") + `
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
			missing               bool
		)
		if err := rows.Scan(&ref, &status, &message, &provider,
			&c.Total, &c.Fixable,
			&c.BySeverity.Critical, &c.BySeverity.High, &c.BySeverity.Medium,
			&c.BySeverity.Low, &c.BySeverity.Unknown,
			&c.FixableBySeverity.Critical, &c.FixableBySeverity.High, &c.FixableBySeverity.Medium,
			&c.FixableBySeverity.Low, &c.FixableBySeverity.Unknown,
			&scannedAt, &retrievedAt, &missing); err != nil {
			return fmt.Errorf("scan security summary: %w", err)
		}
		c.NonFixable = c.Total - c.Fixable

		r := security.Report{
			Status:   security.Status(status),
			Provider: provider,
			Message:  message.String,
			Counts:   c,
			Missing:  missing,
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

// RepositoryIdentity is a configured repository as the catalog recorded it.
type RepositoryIdentity struct {
	Name     string
	Role     string
	Registry string
	Path     string
	Type     string
}

// RepositoryByID resolves the CONFIGURED identity of a repository row.
//
// The security path needs it and nothing else does, which is why it lives
// beside the security store rather than on Packages: a release knows which
// repository row it was discovered in, and a scanner is configured against a
// repository NAME. Without this, the two are joined by guessing - matching a
// registry host and path against the product document - and a product with two
// sources on one host would resolve to whichever the guess happened to find.
func (s *Security) RepositoryByID(ctx context.Context, id int64) (RepositoryIdentity, error) {
	var out RepositoryIdentity
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT name, role, registry_host, repository_path, registry_type
		   FROM repositories WHERE id = ?`), id).
		Scan(&out.Name, &out.Role, &out.Registry, &out.Path, &out.Type)
	if err != nil {
		return RepositoryIdentity{}, fmt.Errorf("resolve repository %d: %w", id, err)
	}
	return out, nil
}

// ReportsFor reads the STORED per-artifact reports for a set of artifacts.
//
// # Why this reads the index and not the detail blob
//
// The index - scans plus findings - is the durable half: statuses, counts, and
// the identifiers that make a finding findable. It is what a sync writes and
// what every listing, comparison and search reads, and it outlives the heavy
// half by design.
//
// The detail blob carries the prose: descriptions, references, CVSS vectors. It
// expires much sooner, because that is the part which would otherwise turn this
// platform into a second copy of a vulnerability database that re-grades itself
// continuously. When it is still present it is merged in; when it is not, the
// findings are still complete enough to list, filter, compare and export -
// they simply lack the paragraph.
//
// A page that showed nothing once the prose expired would be a page that
// silently emptied itself overnight.
//
// # Why the caller chooses
//
// Merging the prose is not free, and it is not free in proportion to how much
// of it the caller wants: it decompresses and parses EVERY stored payload for
// the release, because that is where the paragraphs are. For a release of
// eighty-four thousand findings that is tens of megabytes of JSON per side,
// spent to attach descriptions that a comparison then classifies without ever
// reading. security.IndexOnly skips the whole pass.
//
// The asymmetry to know about: malware, the policy verdict and the list of held
// documents live only in the detail tier, so IndexOnly does not carry them
// either. That is right for a comparison and wrong for a page that draws them,
// which is why this is the caller's decision and not a heuristic here.
func (s *Security) ReportsFor(
	ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
	detail security.Detail,
) ([]security.Report, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	byRef := map[string]*security.Report{}
	scanIDs := map[int64]string{}

	for _, chunk := range chunkRefs(refs, sqlChunk) {
		args := []any{scope.Product, scope.Repository, scope.Provider}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref.Ref())
		}

		query := s.q(`
			SELECT id, artifact_ref, artifact_key, COALESCE(artifact_tag, ''),
			       COALESCE(artifact_kind, ''), COALESCE(artifact_repo, ''),
			       status, COALESCE(message, ''), provider,
			       total, fixable, critical, high, medium, low, unknown,
			       fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
			       scanned_at, retrieved_at, missing
			  FROM security_scans
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (` + strings.Join(placeholders, ",") + `)`)

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("read stored security reports: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var (
					id                                                   int64
					ref, key, tag, kind, repo, status, message, provider string
					c                                                    security.Counts
					scannedAt                                            sql.NullString
					retrievedAt                                          string
					missing                                              bool
				)
				if err := rows.Scan(&id, &ref, &key, &tag, &kind, &repo, &status, &message, &provider,
					&c.Total, &c.Fixable,
					&c.BySeverity.Critical, &c.BySeverity.High, &c.BySeverity.Medium,
					&c.BySeverity.Low, &c.BySeverity.Unknown,
					&c.FixableBySeverity.Critical, &c.FixableBySeverity.High,
					&c.FixableBySeverity.Medium, &c.FixableBySeverity.Low,
					&c.FixableBySeverity.Unknown,
					&scannedAt, &retrievedAt, &missing); err != nil {
					return fmt.Errorf("scan stored security report: %w", err)
				}
				c.NonFixable = c.Total - c.Fixable

				r := &security.Report{
					Artifact: security.ArtifactRef{
						Name: key, Tag: tag, Digest: ref, Kind: kind, Repository: repo,
					},
					Status:   security.Status(status),
					Provider: provider,
					Message:  message,
					Counts:   c,
					Missing:  missing,
					// Stored, so nothing here cost a scanner request.
					FromCache: true,
				}
				if t := parseNullableSecurityTime(scannedAt); t != nil {
					r.ScannedAt = t
				}
				if t, err := parseSecurityTime(retrievedAt); err == nil {
					r.RetrievedAt = t
				}
				byRef[ref] = r
				scanIDs[id] = ref
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}

	if err := s.attachFindings(ctx, scanIDs, byRef); err != nil {
		return nil, err
	}
	if detail == security.WithProse {
		// Prose is an enrichment. Losing it costs the paragraph, not the
		// findings, so a failure here is deliberately swallowed rather than
		// returned.
		_ = s.enrichFromDetails(ctx, scope, refs, byRef)
	}

	// Preserve the caller's order, and give an artifact with no stored row a
	// report that says so rather than omitting it - a release whose artifacts
	// silently vanished from the list would read as a smaller release.
	out := make([]security.Report, 0, len(refs))
	for _, ref := range refs {
		if r, ok := byRef[ref.Ref()]; ok {
			// The caller's ref carries the richer identity (media type,
			// platform, size) that the index does not store.
			name := r.Artifact.Name
			r.Artifact = ref
			if ref.Name == "" {
				r.Artifact.Name = name
			}
			out = append(out, *r)
			continue
		}
		out = append(out, security.Report{
			Artifact: ref,
			Status:   security.StatusNotScanned,
			Provider: scope.Provider,
			Message:  "This artifact has not been included in a vulnerability sync yet.",
		})
	}
	return out, nil
}

// attachFindings loads the index rows for a set of scans.
func (s *Security) attachFindings(
	ctx context.Context, scanIDs map[int64]string, byRef map[string]*security.Report,
) error {
	if len(scanIDs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(scanIDs))
	for id := range scanIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for start := 0; start < len(ids); start += sqlChunk {
		end := start + sqlChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}

		rows, err := s.db.QueryContext(ctx, s.q(`
			SELECT scan_id, cve, issue_id, severity, fixable,
			       component_id, component_name, component_version, component_type,
			       fixed_in, summary
			  FROM security_findings
			 WHERE scan_id IN (`+strings.Join(placeholders, ",")+`)`), args...)
		if err != nil {
			return fmt.Errorf("read stored findings: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var (
					scanID   int64
					f        security.Finding
					severity string
					fixedIn  string
				)
				if err := rows.Scan(&scanID, &f.CVE, &f.ID, &severity, &f.Fixable,
					&f.Component.ID, &f.Component.Name, &f.Component.Version, &f.Component.Type,
					&fixedIn, &f.Summary); err != nil {
					return fmt.Errorf("scan stored finding: %w", err)
				}
				f.Severity = security.Severity(severity)
				if fixedIn != "" {
					f.FixedIn = []string{fixedIn}
				}
				ref, ok := scanIDs[scanID]
				if !ok {
					continue
				}
				report, ok := byRef[ref]
				if !ok {
					continue
				}
				f.Provider = report.Provider
				report.Findings = append(report.Findings, f)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
	}

	for _, r := range byRef {
		security.SortFindings(r.Findings)
	}
	return nil
}

// enrichFromDetails merges the prose from the short-lived detail tier, where it
// is still present.
func (s *Security) enrichFromDetails(
	ctx context.Context, scope security.Scope,
	refs []security.ArtifactRef, byRef map[string]*security.Report,
) error {
	details, err := s.LoadDetails(ctx, scope, refs)
	if err != nil {
		return err
	}
	for ref, full := range details {
		report, ok := byRef[ref]
		if !ok {
			continue
		}
		prose := make(map[string]security.Finding, len(full.Findings))
		for _, f := range full.Findings {
			prose[f.Key()] = f
		}
		for i, f := range report.Findings {
			rich, ok := prose[f.Key()]
			if !ok {
				continue
			}
			report.Findings[i].Description = rich.Description
			report.Findings[i].References = rich.References
			report.Findings[i].CVSSScore = rich.CVSSScore
			report.Findings[i].CVSSVector = rich.CVSSVector
			report.Findings[i].Published = rich.Published
			report.Findings[i].Policy = rich.Policy
			report.Findings[i].Sources = rich.Sources
			if len(rich.FixedIn) > len(report.Findings[i].FixedIn) {
				report.Findings[i].FixedIn = rich.FixedIn
			}
		}

		// Malware, the policy verdict and the list of held bodies live ONLY in
		// this tier, and that is a deliberate asymmetry rather than an
		// oversight. The index exists to make ninety thousand findings
		// searchable; a release has a handful of malware hits and a few dozen
		// violations, so a table and three indexes to store them would be
		// carrying the cost of the index tier for none of its benefit.
		//
		// The trade is that they go when the detail tier is evicted - which,
		// with eviction now driven by a byte budget rather than a clock, is
		// something an operator chose rather than something that happens
		// overnight.
		report.Malware = full.Malware
		report.Violations = full.Violations
		report.Documents = full.Documents
	}
	return nil
}
