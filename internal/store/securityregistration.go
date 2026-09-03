package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Whether a release has been replicated to a scanner that has to be told about
// it, and what that scanner holds for it.
//
// See db/migrations/*/00045_security_registrations.sql for why this is stored
// rather than asked of the scanner on every page render, and why the two being
// able to disagree is the reason the Replicate button exists.

// SecurityRegistrations reads and writes what a release is registered with.
type SecurityRegistrations struct {
	db      *sql.DB
	dialect Dialect
}

// NewSecurityRegistrations builds the store.
func NewSecurityRegistrations(s Store) *SecurityRegistrations {
	return &SecurityRegistrations{db: s.DB(), dialect: DialectFor(s.Driver())}
}

func (r *SecurityRegistrations) q(query string) string { return r.dialect.Rewrite(query) }

// RegistrationRow is one release's registration with one scanner.
type RegistrationRow struct {
	PackageID int64
	Provider  string

	State security.RegistrationState
	Error string

	Expected     int
	Submitted    int
	AlreadyKnown int
	Associated   int
	Analysed     int

	Application   string
	ApplicationID string
	Version       string
	VersionID     string
	URL           string

	StartedAt    *time.Time
	RegisteredAt *time.Time

	Log []security.SyncLogEntry
}

// Done reports whether the scanner holds everything this release wanted.
func (r RegistrationRow) Done() bool { return r.State.Done() }

// Outstanding is how many artifacts the scanner still does not hold.
func (r RegistrationRow) Outstanding() int {
	n := r.Expected - r.Associated
	if n < 0 {
		return 0
	}
	return n
}

// StaleRegistrationAfter is how long a `registering` row is honoured before it
// is treated as abandoned.
//
// Short, because this operation is short: submitting a release's images and
// creating one application version is seconds against a responsive scanner and
// a couple of minutes against a slow one. A window sized for a sync - half an
// hour - would leave a release un-replicable for half an hour after a
// Coordinator restart, over an operation that would have finished in twenty
// seconds.
const StaleRegistrationAfter = 10 * time.Minute

// Claim marks a release as being registered, and refuses if somebody holds it.
//
// # Why the claim is worth having when the work is idempotent anyway
//
// Not for correctness - a double run submits nothing twice and creates no
// second application. For the INTERFACE: two people pressing Replicate on one
// release should see one operation, not two progress states racing each other,
// and the second press should say "that is already running" rather than
// appearing to do nothing.
func (r *SecurityRegistrations) Claim(ctx context.Context, packageID int64, provider string) error {
	now := time.Now().UTC()
	cutoff := securityTime(now.Add(-StaleRegistrationAfter))

	res, err := r.db.ExecContext(ctx, r.q(`
		INSERT INTO security_registrations (package_id, provider, state, started_at)
		VALUES (?, ?, 'registering', ?)
		ON CONFLICT (package_id, provider) DO UPDATE SET
			state = 'registering',
			started_at = excluded.started_at,
			error = NULL
		WHERE security_registrations.state <> 'registering'
		   OR security_registrations.started_at IS NULL
		   OR security_registrations.started_at < ?`),
		packageID, provider, securityTime(now), cutoff)
	if err != nil {
		return fmt.Errorf("claim security registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRegistrationInFlight
	}
	return nil
}

// ErrRegistrationInFlight means somebody else is registering this release.
//
// Not a failure the caller reports as an error page: the thing they wanted is
// happening, which is a different sentence from "that did not work".
var ErrRegistrationInFlight = fmt.Errorf("a replication to this scanner is already running")

// Record stores a finished registration.
func (r *SecurityRegistrations) Record(
	ctx context.Context, packageID int64, reg security.Registration, log []security.SyncLogEntry,
) error {
	now := time.Now().UTC()
	message := reg.Message
	if message == "" && len(reg.Failed) > 0 {
		message = reg.FirstFailure()
	}

	_, err := r.db.ExecContext(ctx, r.q(`
		INSERT INTO security_registrations (
			package_id, provider, state, error,
			expected, submitted, already_known, associated, analysed,
			application, application_id, version, version_id, url,
			started_at, registered_at, log)
		VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, NULL, ?, ?)
		ON CONFLICT (package_id, provider) DO UPDATE SET
			state = excluded.state, error = excluded.error,
			expected = excluded.expected, submitted = excluded.submitted,
			already_known = excluded.already_known, associated = excluded.associated,
			analysed = excluded.analysed,
			application = excluded.application, application_id = excluded.application_id,
			version = excluded.version, version_id = excluded.version_id, url = excluded.url,
			started_at = NULL, registered_at = excluded.registered_at, log = excluded.log`),
		packageID, reg.Provider, string(reg.State), message,
		reg.Expected, reg.Submitted, reg.AlreadyKnown, reg.Associated, reg.Analysed,
		reg.Application, reg.ApplicationID, reg.Version, reg.VersionID, reg.URL,
		securityTime(now), encodeSyncLog(log))
	if err != nil {
		return fmt.Errorf("record security registration: %w", err)
	}
	return nil
}

// Fail records a registration that could not run at all.
//
// The COUNTS ARE NOT CLEARED. A release replicated last week whose Anchore is
// unreachable today still holds what it held; showing the reader nothing, when
// something is known and dated, is the worse of the two answers. Same rule the
// sync applies to a failed run.
func (r *SecurityRegistrations) Fail(
	ctx context.Context, packageID int64, provider, reason string, log []security.SyncLogEntry,
) error {
	_, err := r.db.ExecContext(ctx, r.q(`
		INSERT INTO security_registrations (package_id, provider, state, error, log)
		VALUES (?,?, 'failed', ?, ?)
		ON CONFLICT (package_id, provider) DO UPDATE SET
			state = 'failed', error = excluded.error, started_at = NULL, log = excluded.log`),
		packageID, provider, truncate(reason, 500), encodeSyncLog(log))
	if err != nil {
		return fmt.Errorf("record security registration failure: %w", err)
	}
	return nil
}

// Get reads one release's registration with one scanner.
func (r *SecurityRegistrations) Get(
	ctx context.Context, packageID int64, provider string,
) (RegistrationRow, bool, error) {
	rows, err := r.load(ctx, `WHERE package_id = ? AND provider = ?`, packageID, provider)
	if err != nil || len(rows) == 0 {
		return RegistrationRow{}, false, err
	}
	return rows[0], true, nil
}

// ForPackage reads every scanner's registration for one release.
func (r *SecurityRegistrations) ForPackage(
	ctx context.Context, packageID int64,
) ([]RegistrationRow, error) {
	return r.load(ctx, `WHERE package_id = ? ORDER BY provider`, packageID)
}

// ForPackages reads registrations for several releases at once, for a listing.
func (r *SecurityRegistrations) ForPackages(
	ctx context.Context, ids []int64,
) (map[int64][]RegistrationRow, error) {
	out := map[int64][]RegistrationRow{}
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
		rows, err := r.load(ctx,
			`WHERE package_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY package_id, provider`,
			args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[row.PackageID] = append(out[row.PackageID], row)
		}
	}
	return out, nil
}

func (r *SecurityRegistrations) load(
	ctx context.Context, where string, args ...any,
) ([]RegistrationRow, error) {
	query := r.q(`
		SELECT package_id, provider, state, COALESCE(error, ''),
		       expected, submitted, already_known, associated, analysed,
		       application, application_id, version, version_id, url,
		       started_at, registered_at, COALESCE(log, '')
		  FROM security_registrations ` + where)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read security registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RegistrationRow
	for rows.Next() {
		var (
			row                     RegistrationRow
			state                   string
			startedAt, registeredAt sql.NullString
			log                     string
		)
		if err := rows.Scan(&row.PackageID, &row.Provider, &state, &row.Error,
			&row.Expected, &row.Submitted, &row.AlreadyKnown, &row.Associated, &row.Analysed,
			&row.Application, &row.ApplicationID, &row.Version, &row.VersionID, &row.URL,
			&startedAt, &registeredAt, &log,
		); err != nil {
			return nil, fmt.Errorf("scan security registration: %w", err)
		}
		row.State = security.RegistrationState(state)
		row.StartedAt = parseNullableSecurityTime(startedAt)
		row.RegisteredAt = parseNullableSecurityTime(registeredAt)
		row.Log = decodeSyncLog(log)
		out = append(out, row)
	}
	return out, rows.Err()
}

// Stalled reports a claim whose holder has stopped: a row marked registering
// with a start time older than anything could plausibly still be running.
func (r RegistrationRow) Stalled(after time.Duration) bool {
	if r.State != security.RegistrationRunning {
		return false
	}
	if r.StartedAt == nil {
		return true
	}
	return time.Since(*r.StartedAt) > after
}

// ReleaseAbandoned frees claims whose Coordinator went away.
//
// Called by the same recovery sweep that releases abandoned syncs. Without it a
// Coordinator killed mid-replication leaves a release showing a spinner and
// refusing the button until somebody notices.
func (r *SecurityRegistrations) ReleaseAbandoned(ctx context.Context) (int64, error) {
	cutoff := securityTime(time.Now().UTC().Add(-StaleRegistrationAfter))
	res, err := r.db.ExecContext(ctx, r.q(`
		UPDATE security_registrations
		   SET state = 'failed',
		       error = 'the replication was interrupted; run it again',
		       started_at = NULL
		 WHERE state = 'registering'
		   AND (started_at IS NULL OR started_at < ?)`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("release abandoned security registrations: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
