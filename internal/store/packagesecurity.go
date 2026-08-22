package store

import (
	"context"
	"database/sql"
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

	Counts        security.Counts
	DistinctTotal int
	Coverage      security.Coverage

	ScannedAt   *time.Time
	SyncedAt    *time.Time
	StartedAt   *time.Time
	Fingerprint string
}

// Synced reports whether this row carries a usable result.
func (r PackageSecurityRow) Synced() bool { return r.State == PackageSecuritySynced }

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
// `staleAfter` is what makes the claim recoverable. A process that died
// mid-sync leaves a row marked syncing forever, and a release that can never be
// synced again is a worse outcome than a rare double sync - which costs a
// duplicate scanner query and converges on the same rows.
func (p *PackageSecurity) Claim(ctx context.Context, packageID int64, staleAfter time.Duration) error {
	now := time.Now().UTC()
	cutoff := securityTime(now.Add(-staleAfter))

	res, err := p.db.ExecContext(ctx, p.q(`
		INSERT INTO package_security (package_id, state, started_at, error)
		VALUES (?, 'syncing', ?, '')
		ON CONFLICT (package_id) DO UPDATE SET
			state = 'syncing',
			started_at = excluded.started_at,
			error = ''
		WHERE package_security.state <> 'syncing'
		   OR package_security.started_at IS NULL
		   OR package_security.started_at < ?`),
		packageID, securityTime(now), cutoff)
	if err != nil {
		return fmt.Errorf("claim security sync: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSyncInFlight
	}
	return nil
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
			fix_critical, fix_high, fix_medium, fix_low, fix_unknown, distinct_total,
			artifacts, scanned, not_scanned, unsupported, unavailable, disabled,
			scanned_at, synced_at, started_at, fingerprint)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,NULL,?)
		ON CONFLICT (package_id) DO UPDATE SET
			state = excluded.state, error = excluded.error,
			provider = excluded.provider, repository = excluded.repository, role = excluded.role,
			total = excluded.total, fixable = excluded.fixable,
			critical = excluded.critical, high = excluded.high, medium = excluded.medium,
			low = excluded.low, unknown = excluded.unknown,
			fix_critical = excluded.fix_critical, fix_high = excluded.fix_high,
			fix_medium = excluded.fix_medium, fix_low = excluded.fix_low,
			fix_unknown = excluded.fix_unknown, distinct_total = excluded.distinct_total,
			artifacts = excluded.artifacts, scanned = excluded.scanned,
			not_scanned = excluded.not_scanned, unsupported = excluded.unsupported,
			unavailable = excluded.unavailable, disabled = excluded.disabled,
			scanned_at = excluded.scanned_at, synced_at = excluded.synced_at,
			started_at = NULL, fingerprint = excluded.fingerprint`),
		row.PackageID, string(row.State), row.Error, row.Provider, row.Repository, roleOr(row.Role),
		c.Total, c.Fixable,
		c.BySeverity.Critical, c.BySeverity.High, c.BySeverity.Medium, c.BySeverity.Low, c.BySeverity.Unknown,
		c.FixableBySeverity.Critical, c.FixableBySeverity.High, c.FixableBySeverity.Medium,
		c.FixableBySeverity.Low, c.FixableBySeverity.Unknown, row.DistinctTotal,
		cov.Artifacts, cov.Scanned, cov.NotScanned, cov.Unsupported, cov.Unavailable, cov.Disabled,
		timeOrNil(row.ScannedAt), securityTime(now), row.Fingerprint)
	if err != nil {
		return fmt.Errorf("complete security sync: %w", err)
	}
	return nil
}

// Fail records a sync that gave up, keeping whatever the last good result was.
//
// The counts are deliberately NOT cleared. A release that synced cleanly last
// week and whose scanner is unreachable today still knows what it knew - and
// showing the reader nothing, when something is known and dated, is the worse
// of the two answers.
func (p *PackageSecurity) Fail(ctx context.Context, packageID int64, reason string) error {
	_, err := p.db.ExecContext(ctx, p.q(`
		INSERT INTO package_security (package_id, state, error, started_at)
		VALUES (?, 'failed', ?, NULL)
		ON CONFLICT (package_id) DO UPDATE SET
			state = 'failed', error = excluded.error, started_at = NULL`),
		packageID, truncate(reason, 500))
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
func (p *PackageSecurity) ReleaseAbandoned(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := securityTime(time.Now().UTC().Add(-staleAfter))
	res, err := p.db.ExecContext(ctx, p.q(`
		UPDATE package_security
		   SET state = 'failed',
		       error = 'the sync was interrupted; run it again',
		       started_at = NULL
		 WHERE state = 'syncing' AND (started_at IS NULL OR started_at < ?)`), cutoff)
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
		       fix_critical, fix_high, fix_medium, fix_low, fix_unknown, distinct_total,
		       artifacts, scanned, not_scanned, unsupported, unavailable, disabled,
		       scanned_at, synced_at, started_at, fingerprint
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
		)
		if err := rows.Scan(&r.PackageID, &state, &r.Error, &r.Provider, &r.Repository, &r.Role,
			&r.Counts.Total, &r.Counts.Fixable,
			&r.Counts.BySeverity.Critical, &r.Counts.BySeverity.High, &r.Counts.BySeverity.Medium,
			&r.Counts.BySeverity.Low, &r.Counts.BySeverity.Unknown,
			&r.Counts.FixableBySeverity.Critical, &r.Counts.FixableBySeverity.High,
			&r.Counts.FixableBySeverity.Medium, &r.Counts.FixableBySeverity.Low,
			&r.Counts.FixableBySeverity.Unknown, &r.DistinctTotal,
			&r.Coverage.Artifacts, &r.Coverage.Scanned, &r.Coverage.NotScanned,
			&r.Coverage.Unsupported, &r.Coverage.Unavailable, &r.Coverage.Disabled,
			&scannedAt, &syncedAt, &startedAt, &r.Fingerprint,
		); err != nil {
			return nil, fmt.Errorf("scan package security: %w", err)
		}
		r.State = PackageSecurityState(state)
		r.Counts.NonFixable = r.Counts.Total - r.Counts.Fixable
		r.ScannedAt = parseNullableSecurityTime(scannedAt)
		r.SyncedAt = parseNullableSecurityTime(syncedAt)
		r.StartedAt = parseNullableSecurityTime(startedAt)
		out = append(out, r)
	}
	return out, rows.Err()
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
		PackageID:     res.PackageID,
		State:         PackageSecuritySynced,
		Provider:      res.Provider,
		Repository:    res.Repository,
		Role:          res.Role,
		Counts:        res.Posture.Counts,
		DistinctTotal: res.Posture.UniqueCounts.Total,
		Coverage:      res.Posture.Coverage,
		ScannedAt:     res.Posture.ScannedAt,
		Fingerprint:   res.Fingerprint,
	}
	return p.Complete(ctx, row)
}

var _ security.Recorder = (*PackageSecurity)(nil)
