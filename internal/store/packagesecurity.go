package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// PackageSecurityState is where a release's sync has got to.
//
// Four values, because "has this been synced" has two answers and needs four:
// never, running, done, failed. Three of them look identical to a timestamp,
// and the interface has to offer a different thing for each.
type PackageSecurityState string

const (
	PackageSecurityNever   PackageSecurityState = ""
	PackageSecuritySyncing PackageSecurityState = "syncing"
	PackageSecuritySynced  PackageSecurityState = "synced"
	PackageSecurityFailed  PackageSecurityState = "failed"
)

// PackageSecurityRow is one release's stored security result.
type PackageSecurityRow struct {
	PackageID int64
	State     PackageSecurityState
	Error     string

	Provider   string
	Repository string
	Role       string

	Counts security.Counts
	// DistinctTotal collapses (CVE, package) PAIRS - openssl and libssl3
	// carrying one advisory are two. DistinctCVEs collapses the ADVISORY -
	// they are one.
	//
	// Both, and named for what they count, because the interface printed the
	// first under the label "unique CVEs" and a reader hearing that counts the
	// second. Two right answers to two questions is fine; one answer wearing
	// the other's name is how a page loses a reader's trust in every number on
	// it.
	DistinctTotal  int
	DistinctCVEs   int
	DistinctCounts security.Counts
	Coverage       security.Coverage

	// Sources is one row per scanner that contributed, with how much only that
	// scanner reported. Empty on a single-scanner deployment, where the
	// breakdown is the release's own numbers restated.
	Sources []security.SourceCounts

	ScannedAt   *time.Time
	SyncedAt    *time.Time
	StartedAt   *time.Time
	Fingerprint string

	// ClaimedBy names the process that holds a running claim, and HeartbeatAt
	// is the last time it said so. Together they are what tells a sync that is
	// running from one whose Coordinator went away: `state = syncing` says only
	// that somebody started one.
	ClaimedBy   string
	HeartbeatAt *time.Time

	// Log is the transcript of the run that produced this row.
	Log []security.SyncLogEntry
}

// Synced reports whether this row carries a usable result.
func (r PackageSecurityRow) Synced() bool { return r.State == PackageSecuritySynced }

// Stalled reports a claim whose holder has stopped saying it is alive.
//
// This is the difference between "a sync is running somewhere else" and "a
// Coordinator was killed mid-sync", which a reader was previously shown as the
// first because the row cannot tell them apart on `state` alone. A row from
// before the heartbeat existed has none, and falls back to the long window the
// claim itself uses.
func (r PackageSecurityRow) Stalled(staleAfter time.Duration) bool {
	if r.State != PackageSecuritySyncing {
		return false
	}
	if r.HeartbeatAt != nil {
		return time.Since(*r.HeartbeatAt) > security.HeartbeatTimeout
	}
	if r.StartedAt != nil {
		return time.Since(*r.StartedAt) > staleAfter
	}
	return true
}

// PackageSecurity stores the per-release result of a vulnerability sync.
//
// Separate from Security, which caches per-artifact retrievals, because the two
// have different lifetimes and answer different questions. This one does not
// expire: it is what somebody asked for, and it is what a listing of two
// hundred releases renders without touching a scanner.
type PackageSecurity struct {
	db      *sql.DB
	dialect Dialect
}

// NewPackageSecurity builds the store.
func NewPackageSecurity(s Store) *PackageSecurity {
	return &PackageSecurity{db: s.DB(), dialect: DialectFor(s.Driver())}
}

func (p *PackageSecurity) q(query string) string { return p.dialect.Rewrite(query) }

// ErrSyncInFlight means somebody else is already syncing this release.
//
// Not an error to show as a failure: it is the honest answer to "start a sync"
// when one is running, and the caller reports it as "already running" rather
// than as a refusal.
var ErrSyncInFlight = errors.New("a vulnerability sync is already running for this release")

// Claim marks a release as syncing, and refuses if somebody already has.
//
// # Why the claim is a conditional UPDATE rather than a read-then-write
//
// Two people pressing the button at the same moment, or a page that retries,
// would both read "not syncing" and both start. The conflict target and the
// WHERE clause make the claim atomic: exactly one caller sees a row affected,
// and the other gets ErrSyncInFlight without either of them having queried a
// scanner.
//
// # What makes a claim recoverable
//
// Two things, and they answer different failures. `staleAfter` bounds a sync
// that is genuinely running and has taken absurdly long. The HEARTBEAT bounds
// the failure that actually happens: a Coordinator killed mid-sync leaves a row
// marked syncing with a start time that only gets older, and until this a
// release in that state was refused for half an hour while every reader was
// told the sync was running on another Coordinator. A claim that has stopped
// beating is held by nothing, and it is taken.
//
// Rows written before the heartbeat existed have none, and fall back to the
// start time - the old behaviour, for the old rows, rather than a migration
// that has to guess.
func (p *PackageSecurity) Claim(
	ctx context.Context, packageID int64, owner string, staleAfter time.Duration,
) error {
	now := time.Now().UTC()
	cutoff := securityTime(now.Add(-staleAfter))
	beatCutoff := securityTime(now.Add(-security.HeartbeatTimeout))

	res, err := p.db.ExecContext(ctx, p.q(`
		INSERT INTO package_security (package_id, state, started_at, heartbeat_at, claimed_by, error)
		VALUES (?, 'syncing', ?, ?, ?, '')
		ON CONFLICT (package_id) DO UPDATE SET
			state = 'syncing',
			started_at = excluded.started_at,
			heartbeat_at = excluded.heartbeat_at,
			claimed_by = excluded.claimed_by,
			error = ''
		WHERE package_security.state <> 'syncing'
		   OR package_security.started_at IS NULL
		   OR package_security.started_at < ?
		   OR (package_security.heartbeat_at IS NOT NULL AND package_security.heartbeat_at < ?)`),
		packageID, securityTime(now), securityTime(now), owner, cutoff, beatCutoff)
	if err != nil {
		return fmt.Errorf("claim security sync: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSyncInFlight
	}
	return nil
}

// Heartbeat renews a claim and reports whether it is still held by `owner`.
//
// False is not an error: the claim was stopped by somebody pressing Stop, or
// taken by another process after this one stopped beating. Either way the run
// that asked has been told to stand down, which is what makes a sync
// cancellable from a Coordinator that is not the one running it.
func (p *PackageSecurity) Heartbeat(ctx context.Context, packageID int64, owner string) (bool, error) {
	res, err := p.db.ExecContext(ctx, p.q(`
		UPDATE package_security
		   SET heartbeat_at = ?
		 WHERE package_id = ? AND state = 'syncing' AND claimed_by = ?`),
		securityTime(time.Now().UTC()), packageID, owner)
	if err != nil {
		return false, fmt.Errorf("heartbeat security sync: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Stop releases a running claim, leaving the last good result in place.
//
// # Why the state goes BACK rather than to failed
//
// Because nothing failed. A sync somebody stopped is a sync that did not
// happen, and a release that was synced last week still knows what it knew. So
// a release with a stored result returns to `synced` and one with none returns
// to "never synced" - the two states that were true before the run started.
//
// Reports whether a claim was actually held: a caller pressing Stop on a sync
// that has just finished should be told so rather than shown a success.
func (p *PackageSecurity) Stop(ctx context.Context, packageID int64) (bool, error) {
	res, err := p.db.ExecContext(ctx, p.q(`
		UPDATE package_security
		   SET state = CASE WHEN synced_at IS NULL THEN '' ELSE 'synced' END,
		       started_at = NULL,
		       heartbeat_at = NULL,
		       claimed_by = '',
		       error = ''
		 WHERE state = 'syncing' AND package_id = ?`), packageID)
	if err != nil {
		return false, fmt.Errorf("stop security sync: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Complete records a finished sync.
func (p *PackageSecurity) Complete(ctx context.Context, row PackageSecurityRow) error {
	c := row.Counts
	cov := row.Coverage
	now := time.Now().UTC()

	_, err := p.db.ExecContext(ctx, p.q(`
		INSERT INTO package_security (
			package_id, state, error, provider, repository, role,
			total, fixable, critical, high, medium, low, unknown,
			fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
			distinct_total, distinct_cves, distinct_fixable,
			distinct_critical, distinct_high, distinct_medium, distinct_low, distinct_unknown,
			artifacts, scanned, not_scanned, unsupported, unavailable, disabled,
			scanned_at, synced_at, started_at, fingerprint, log, missing)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,NULL,?,?,?)
		ON CONFLICT (package_id) DO UPDATE SET
			state = excluded.state, error = excluded.error,
			provider = excluded.provider, repository = excluded.repository, role = excluded.role,
			total = excluded.total, fixable = excluded.fixable,
			critical = excluded.critical, high = excluded.high, medium = excluded.medium,
			low = excluded.low, unknown = excluded.unknown,
			fix_critical = excluded.fix_critical, fix_high = excluded.fix_high,
			fix_medium = excluded.fix_medium, fix_low = excluded.fix_low,
			fix_unknown = excluded.fix_unknown,
			distinct_total = excluded.distinct_total, distinct_cves = excluded.distinct_cves,
			distinct_fixable = excluded.distinct_fixable,
			distinct_critical = excluded.distinct_critical, distinct_high = excluded.distinct_high,
			distinct_medium = excluded.distinct_medium, distinct_low = excluded.distinct_low,
			distinct_unknown = excluded.distinct_unknown,
			artifacts = excluded.artifacts, scanned = excluded.scanned,
			not_scanned = excluded.not_scanned, unsupported = excluded.unsupported,
			unavailable = excluded.unavailable, disabled = excluded.disabled,
			scanned_at = excluded.scanned_at, synced_at = excluded.synced_at,
			started_at = NULL, heartbeat_at = NULL, claimed_by = '',
			fingerprint = excluded.fingerprint, log = excluded.log,
			missing = excluded.missing`),
		row.PackageID, string(row.State), row.Error, row.Provider, row.Repository, roleOr(row.Role),
		c.Total, c.Fixable,
		c.BySeverity.Critical, c.BySeverity.High, c.BySeverity.Medium, c.BySeverity.Low, c.BySeverity.Unknown,
		c.FixableBySeverity.Critical, c.FixableBySeverity.High, c.FixableBySeverity.Medium,
		c.FixableBySeverity.Low, c.FixableBySeverity.Unknown,
		row.DistinctTotal, row.DistinctCVEs, row.DistinctCounts.Fixable,
		row.DistinctCounts.BySeverity.Critical, row.DistinctCounts.BySeverity.High,
		row.DistinctCounts.BySeverity.Medium, row.DistinctCounts.BySeverity.Low,
		row.DistinctCounts.BySeverity.Unknown,
		cov.Artifacts, cov.Scanned, cov.NotScanned, cov.Unsupported, cov.Unavailable, cov.Disabled,
		timeOrNil(row.ScannedAt), securityTime(now), row.Fingerprint, encodeSyncLog(row.Log), cov.Missing)
	if err != nil {
		return fmt.Errorf("complete security sync: %w", err)
	}
	return p.replaceSources(ctx, row.PackageID, row.Sources)
}

// replaceSources rewrites a release's per-scanner rows.
//
// Delete-then-insert rather than an upsert, because a scanner that stopped
// contributing has to LOSE its row: a release re-synced after Anchore was
// switched off would otherwise keep answering "Anchore found 402 nobody else
// saw" forever, about a scan that no longer happens.
func (p *PackageSecurity) replaceSources(
	ctx context.Context, packageID int64, sources []security.SourceCounts,
) error {
	if _, err := p.db.ExecContext(ctx, p.q(
		`DELETE FROM package_security_sources WHERE package_id = ?`), packageID); err != nil {
		return fmt.Errorf("clear security sources: %w", err)
	}
	for _, src := range sources {
		c := src.Counts
		if _, err := p.db.ExecContext(ctx, p.q(`
			INSERT INTO package_security_sources (
				package_id, provider, total, fixable,
				critical, high, medium, low, unknown,
				distinct_cves, only_cves, artifacts)
			VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?)`),
			packageID, src.Provider, c.Total, c.Fixable,
			c.BySeverity.Critical, c.BySeverity.High, c.BySeverity.Medium,
			c.BySeverity.Low, c.BySeverity.Unknown,
			src.UniqueCVEs, src.OnlyHere, src.Artifacts); err != nil {
			return fmt.Errorf("save security source %q: %w", src.Provider, err)
		}
	}
	return nil
}

// LoadSources reads a release's per-scanner rows.
func (p *PackageSecurity) LoadSources(
	ctx context.Context, packageID int64,
) ([]security.SourceCounts, error) {
	rows, err := p.db.QueryContext(ctx, p.q(`
		SELECT provider, total, fixable, critical, high, medium, low, unknown,
		       distinct_cves, only_cves, artifacts
		  FROM package_security_sources
		 WHERE package_id = ?
		 ORDER BY provider`), packageID)
	if err != nil {
		return nil, fmt.Errorf("read security sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []security.SourceCounts
	for rows.Next() {
		var src security.SourceCounts
		c := &src.Counts
		if err := rows.Scan(&src.Provider, &c.Total, &c.Fixable,
			&c.BySeverity.Critical, &c.BySeverity.High, &c.BySeverity.Medium,
			&c.BySeverity.Low, &c.BySeverity.Unknown,
			&src.UniqueCVEs, &src.OnlyHere, &src.Artifacts); err != nil {
			return nil, fmt.Errorf("scan security source: %w", err)
		}
		c.NonFixable = c.Total - c.Fixable
		out = append(out, src)
	}
	return out, rows.Err()
}

// Fail records a sync that gave up, keeping whatever the last good result was.
//
// The counts are deliberately NOT cleared. A release that synced cleanly last
// week and whose scanner is unreachable today still knows what it knew - and
// showing the reader nothing, when something is known and dated, is the worse
// of the two answers.
func (p *PackageSecurity) Fail(ctx context.Context, packageID int64, reason string, log []security.SyncLogEntry) error {
	_, err := p.db.ExecContext(ctx, p.q(`
		INSERT INTO package_security (package_id, state, error, started_at, log)
		VALUES (?, 'failed', ?, NULL, ?)
		ON CONFLICT (package_id) DO UPDATE SET
			state = 'failed', error = excluded.error, started_at = NULL,
			heartbeat_at = NULL, claimed_by = '', log = excluded.log`),
		packageID, truncate(reason, 500), encodeSyncLog(log))
	if err != nil {
		return fmt.Errorf("record security sync failure: %w", err)
	}
	return nil
}

// Get reads one release's stored result. A missing row is not an error: it is
// "nobody has synced this", which is a state the interface renders.
func (p *PackageSecurity) Get(ctx context.Context, packageID int64) (PackageSecurityRow, bool, error) {
	rows, err := p.load(ctx, `WHERE package_id = ?`, packageID)
	if err != nil {
		return PackageSecurityRow{}, false, err
	}
	if len(rows) == 0 {
		return PackageSecurityRow{}, false, nil
	}
	return rows[0], true, nil
}

// ForPackages reads many rows at once, for a listing.
//
// One query per page rather than one per row. That difference is the whole
// reason this table exists: the column it renders used to cost a scanner query
// per release and now costs nothing.
func (p *PackageSecurity) ForPackages(ctx context.Context, ids []int64) (map[int64]PackageSecurityRow, error) {
	out := map[int64]PackageSecurityRow{}
	if len(ids) == 0 {
		return out, nil
	}
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
		rows, err := p.load(ctx, `WHERE package_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.PackageID] = r
		}
	}
	return out, nil
}

// ReleaseAbandoned clears claims held by a process that is no longer running,
// so a release stuck mid-sync becomes syncable again.
//
// A claim that has stopped beating is abandoned however recently it started:
// that is the whole point of the heartbeat, and waiting `staleAfter` for a
// process that is provably gone is half an hour of a release saying it is
// syncing when nothing is.
func (p *PackageSecurity) ReleaseAbandoned(ctx context.Context, staleAfter time.Duration) (int64, error) {
	now := time.Now().UTC()
	cutoff := securityTime(now.Add(-staleAfter))
	beatCutoff := securityTime(now.Add(-security.HeartbeatTimeout))
	res, err := p.db.ExecContext(ctx, p.q(`
		UPDATE package_security
		   SET state = 'failed',
		       error = 'the sync was interrupted; run it again',
		       started_at = NULL,
		       heartbeat_at = NULL,
		       claimed_by = ''
		 WHERE state = 'syncing'
		   AND (started_at IS NULL
		        OR started_at < ?
		        OR (heartbeat_at IS NOT NULL AND heartbeat_at < ?))`), cutoff, beatCutoff)
	if err != nil {
		return 0, fmt.Errorf("release abandoned security syncs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (p *PackageSecurity) load(ctx context.Context, where string, args ...any) ([]PackageSecurityRow, error) {
	query := p.q(`
		SELECT package_id, state, COALESCE(error, ''), provider, repository, role,
		       total, fixable, critical, high, medium, low, unknown,
		       fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
		       distinct_total, COALESCE(distinct_cves, 0), COALESCE(distinct_fixable, 0),
		       COALESCE(distinct_critical, 0), COALESCE(distinct_high, 0),
		       COALESCE(distinct_medium, 0), COALESCE(distinct_low, 0), COALESCE(distinct_unknown, 0),
		       artifacts, scanned, not_scanned, unsupported, unavailable, disabled,
		       scanned_at, synced_at, started_at, fingerprint, COALESCE(log, ''), COALESCE(missing, 0),
		       COALESCE(claimed_by, ''), heartbeat_at
		  FROM package_security ` + where)

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read package security: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PackageSecurityRow
	for rows.Next() {
		var (
			r                              PackageSecurityRow
			state                          string
			scannedAt, syncedAt, startedAt sql.NullString
			heartbeatAt                    sql.NullString
			log                            string
		)
		if err := rows.Scan(&r.PackageID, &state, &r.Error, &r.Provider, &r.Repository, &r.Role,
			&r.Counts.Total, &r.Counts.Fixable,
			&r.Counts.BySeverity.Critical, &r.Counts.BySeverity.High, &r.Counts.BySeverity.Medium,
			&r.Counts.BySeverity.Low, &r.Counts.BySeverity.Unknown,
			&r.Counts.FixableBySeverity.Critical, &r.Counts.FixableBySeverity.High,
			&r.Counts.FixableBySeverity.Medium, &r.Counts.FixableBySeverity.Low,
			&r.Counts.FixableBySeverity.Unknown, &r.DistinctTotal, &r.DistinctCVEs, &r.DistinctCounts.Fixable,
			&r.DistinctCounts.BySeverity.Critical, &r.DistinctCounts.BySeverity.High,
			&r.DistinctCounts.BySeverity.Medium, &r.DistinctCounts.BySeverity.Low,
			&r.DistinctCounts.BySeverity.Unknown,
			&r.Coverage.Artifacts, &r.Coverage.Scanned, &r.Coverage.NotScanned,
			&r.Coverage.Unsupported, &r.Coverage.Unavailable, &r.Coverage.Disabled,
			&scannedAt, &syncedAt, &startedAt, &r.Fingerprint, &log, &r.Coverage.Missing,
			&r.ClaimedBy, &heartbeatAt,
		); err != nil {
			return nil, fmt.Errorf("scan package security: %w", err)
		}
		r.State = PackageSecurityState(state)
		r.Counts.NonFixable = r.Counts.Total - r.Counts.Fixable
		r.DistinctCounts.Total = r.DistinctTotal
		r.DistinctCounts.NonFixable = r.DistinctTotal - r.DistinctCounts.Fixable
		r.ScannedAt = parseNullableSecurityTime(scannedAt)
		r.SyncedAt = parseNullableSecurityTime(syncedAt)
		r.StartedAt = parseNullableSecurityTime(startedAt)
		r.HeartbeatAt = parseNullableSecurityTime(heartbeatAt)
		r.Log = decodeSyncLog(log)
		out = append(out, r)
	}
	return out, rows.Err()
}

// encodeSyncLog stores a transcript as JSON, and an empty one as an empty
// string rather than as "null" - a column somebody may read by eye.
func encodeSyncLog(entries []security.SyncLogEntry) string {
	if len(entries) == 0 {
		return ""
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(raw)
}

// decodeSyncLog is deliberately forgiving: a log we cannot read is a missing
// log, never a failed page.
func decodeSyncLog(raw string) []security.SyncLogEntry {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []security.SyncLogEntry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func parseNullableSecurityTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := parseSecurityTime(v.String)
	if err != nil {
		return nil
	}
	return &t
}

// Record adapts a finished sync into this table's row.
//
// The adapter lives here rather than in internal/security because the
// dependency points this way: the store knows about the security model, and the
// security model must not know about SQL.
func (p *PackageSecurity) Record(ctx context.Context, res security.PackageResult) error {
	row := PackageSecurityRow{
		PackageID:      res.PackageID,
		State:          PackageSecuritySynced,
		Provider:       res.Provider,
		Repository:     res.Repository,
		Role:           res.Role,
		Counts:         res.Posture.Counts,
		DistinctTotal:  res.Posture.UniqueCounts.Total,
		DistinctCVEs:   res.Posture.UniqueCVEs,
		DistinctCounts: res.Posture.UniqueCounts,
		Coverage:       res.Posture.Coverage,
		ScannedAt:      res.Posture.ScannedAt,
		Fingerprint:    res.Fingerprint,
		Sources:        res.Posture.BySource,
		Log:            res.Log,
	}
	return p.Complete(ctx, row)
}

var _ security.Recorder = (*PackageSecurity)(nil)
