package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Persistence for delegated replication: what we last WROTE to a registry, and
// what it did next.
//
// See db/migrations/*/00016_target_replication.sql for why these are two
// tables. In one sentence: an apply is an assertion we made and a sync is an
// observation we recorded, and a schema that conflated them would make it
// impossible to answer "did Quay do what we asked" a year later.

// Replication is the persistence surface for target replication.
type Replication struct {
	db      *sql.DB
	dialect Dialect
}

// NewReplication builds the store.
func NewReplication(s Store) *Replication {
	return &Replication{db: s.DB(), dialect: DialectFor(s.Driver())}
}

// ProductID resolves a configured product name to its row id.
//
// The replication tables key on the row rather than the name so a deleted
// product takes its history with it by cascade, which is what the retention
// story already assumes everywhere else.
func (r *Replication) ProductID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		r.dialect.Rewrite(`SELECT id FROM products WHERE name = ?`), name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("product %q has no catalog row: reconciliation has not run: %w", name, ErrNoRecord)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve product %q: %w", name, err)
	}
	return id, nil
}

// TargetReplication is one target's applied state and last observation.
type TargetReplication struct {
	ProductID  int64
	Product    string
	TargetName string
	Mode       string

	AppliedConfigHash string
	AppliedSecretHash string
	AppliedAt         sql.NullString
	AppliedBy         string

	LastObservedAt sql.NullString
	DriftDetected  bool
	DriftSummary   string
}

// Applied records a successful write to a registry's own configuration.
//
// Upserted per (product, target) rather than appended: the interesting object
// is the CURRENT applied state, and the history of applies is what the audit
// trail is for. A table that grew a row per apply would need a "latest" query
// on every drift check for no information anyone asked for.
func (r *Replication) Applied(
	ctx context.Context, productID int64, target, mode, configHash, secretHash, actor string, at time.Time,
) error {
	stmt := r.dialect.Rewrite(`
		INSERT INTO target_replication
		       (product_id, target_name, mode, applied_config_hash, applied_secret_hash,
		        applied_at, applied_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ` + r.dialect.Now() + `, ` + r.dialect.Now() + `)
		ON CONFLICT (product_id, target_name) DO UPDATE SET
		    mode                = EXCLUDED.mode,
		    applied_config_hash = EXCLUDED.applied_config_hash,
		    applied_secret_hash = EXCLUDED.applied_secret_hash,
		    applied_at          = EXCLUDED.applied_at,
		    applied_by          = EXCLUDED.applied_by,
		    -- An apply closes drift by definition: we have just made the
		    -- registry say what the configuration says. Leaving the flag set
		    -- would report a fault that the operator had already fixed, which
		    -- is how a drift signal stops being believed.
		    drift_detected      = ` + r.dialect.Bool(false) + `,
		    drift_summary       = '',
		    updated_at          = ` + r.dialect.Now())

	if _, err := r.db.ExecContext(ctx, stmt, productID, target, mode, configHash, secretHash,
		formatTime(at), actor); err != nil {
		return fmt.Errorf("record apply for %s: %w", target, err)
	}
	return nil
}

// Observed records the result of a read.
//
// Called from every reload, from `products check` and from the metrics sweep,
// so it must never write anything but the observation — in particular it must
// not touch applied_config_hash, which is the record of what we sent and the
// only fixed point drift is measured from.
func (r *Replication) Observed(
	ctx context.Context, productID int64, target, mode string, drifted bool, summary string, at time.Time,
) error {
	stmt := r.dialect.Rewrite(`
		INSERT INTO target_replication
		       (product_id, target_name, mode, last_observed_at, drift_detected, drift_summary,
		        created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ` + r.dialect.Now() + `, ` + r.dialect.Now() + `)
		ON CONFLICT (product_id, target_name) DO UPDATE SET
		    mode             = EXCLUDED.mode,
		    last_observed_at = EXCLUDED.last_observed_at,
		    drift_detected   = EXCLUDED.drift_detected,
		    drift_summary    = EXCLUDED.drift_summary,
		    updated_at       = ` + r.dialect.Now())

	if _, err := r.db.ExecContext(ctx, stmt, productID, target, mode,
		formatTime(at), drifted, summary); err != nil {
		return fmt.Errorf("record observation for %s: %w", target, err)
	}
	return nil
}

// Get returns one target's replication record. A target that has never been
// applied or observed has no row, which is reported as ErrNoRows rather than
// as a zero value — "never applied" and "applied with empty hashes" are
// different states and only one of them is a mistake.
func (r *Replication) Get(ctx context.Context, productID int64, target string) (TargetReplication, error) {
	query := r.dialect.Rewrite(`
		SELECT product_id, target_name, mode, applied_config_hash, applied_secret_hash,
		       applied_at, applied_by, last_observed_at, drift_detected, drift_summary
		  FROM target_replication
		 WHERE product_id = ? AND target_name = ?`)

	var out TargetReplication
	err := r.db.QueryRowContext(ctx, query, productID, target).Scan(
		&out.ProductID, &out.TargetName, &out.Mode,
		&out.AppliedConfigHash, &out.AppliedSecretHash, &out.AppliedAt, &out.AppliedBy,
		&out.LastObservedAt, &out.DriftDetected, &out.DriftSummary)
	if errors.Is(err, sql.ErrNoRows) {
		// Distinguished from a zero value on purpose: "never applied" and
		// "applied with empty hashes" are different states, and only one of
		// them means somebody should run apply.
		return TargetReplication{}, ErrNoRecord
	}
	if err != nil {
		return TargetReplication{}, err
	}
	return out, nil
}

// List returns every recorded target, optionally for one product.
func (r *Replication) List(ctx context.Context, productName string) ([]TargetReplication, error) {
	query := `
		SELECT p.name, tr.product_id, tr.target_name, tr.mode,
		       tr.applied_config_hash, tr.applied_secret_hash, tr.applied_at, tr.applied_by,
		       tr.last_observed_at, tr.drift_detected, tr.drift_summary
		  FROM target_replication tr
		  JOIN products p ON p.id = tr.product_id`
	args := []any{}
	if productName != "" {
		query += " WHERE p.name = ?"
		args = append(args, productName)
	}
	query += " ORDER BY p.name, tr.target_name"

	rows, err := r.db.QueryContext(ctx, r.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list target replication: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TargetReplication
	for rows.Next() {
		var t TargetReplication
		if err := rows.Scan(&t.Product, &t.ProductID, &t.TargetName, &t.Mode,
			&t.AppliedConfigHash, &t.AppliedSecretHash, &t.AppliedAt, &t.AppliedBy,
			&t.LastObservedAt, &t.DriftDetected, &t.DriftSummary); err != nil {
			return nil, fmt.Errorf("scan target replication: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Sync statuses. OUR classification, which is deliberately not Quay's.
const (
	SyncRequested = "requested"
	SyncRunning   = "running"
	SyncSucceeded = "succeeded"
	// SyncDiverged means the sync completed and the destination holds a
	// DIFFERENT digest than the one we expected. Normal for a mirror whose
	// upstream tag moved, and a fact worth recording rather than a failure.
	SyncDiverged = "diverged"
	SyncFailed   = "failed"
	// SyncUnknown means Quay reported a status this build does not recognise.
	SyncUnknown = "unknown"
)

// MirrorSync is one observed sync run.
type MirrorSync struct {
	ID         int64
	Product    string
	TargetName string
	TransferID sql.NullInt64

	RequestedAt sql.NullString
	StartedAt   sql.NullString
	FinishedAt  sql.NullString

	Status     string
	QuayStatus string

	ObservedDigest string
	ExpectedDigest string
	Message        string
}

// RecordSyncRequested opens a sync record and returns its id.
func (r *Replication) RecordSyncRequested(
	ctx context.Context, productID int64, target string, transferID sql.NullInt64,
	expectedDigest string, at time.Time,
) (int64, error) {
	stmt := r.dialect.Rewrite(`
		INSERT INTO mirror_syncs
		       (product_id, target_name, transfer_id, requested_at, status, expected_digest,
		        created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ` + r.dialect.Now() + `, ` + r.dialect.Now() + `)
		RETURNING id`)

	var id int64
	if err := r.db.QueryRowContext(ctx, stmt,
		productID, target, nullableInt(transferID), formatTime(at), SyncRequested, expectedDigest,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("record sync request for %s: %w", target, err)
	}
	return id, nil
}

// UpdateSync records what we later observed about a sync.
func (r *Replication) UpdateSync(ctx context.Context, id int64, s MirrorSync) error {
	stmt := r.dialect.Rewrite(`
		UPDATE mirror_syncs
		   SET status          = ?,
		       quay_status     = ?,
		       started_at      = COALESCE(?, started_at),
		       finished_at     = COALESCE(?, finished_at),
		       observed_digest = ?,
		       message         = ?,
		       updated_at      = ` + r.dialect.Now() + `
		 WHERE id = ?`)

	if _, err := r.db.ExecContext(ctx, stmt, s.Status, s.QuayStatus,
		nullableStr(s.StartedAt), nullableStr(s.FinishedAt),
		s.ObservedDigest, s.Message, id); err != nil {
		return fmt.Errorf("update sync %d: %w", id, err)
	}
	return nil
}

// ListSyncs returns a target's observed sync history, newest first.
func (r *Replication) ListSyncs(
	ctx context.Context, productName, target string, limit int,
) ([]MirrorSync, error) {
	if limit <= 0 {
		limit = 50
	}
	query := r.dialect.Rewrite(`
		SELECT ms.id, p.name, ms.target_name, ms.transfer_id,
		       ms.requested_at, ms.started_at, ms.finished_at,
		       ms.status, ms.quay_status, ms.observed_digest, ms.expected_digest, ms.message
		  FROM mirror_syncs ms
		  JOIN products p ON p.id = ms.product_id
		 WHERE p.name = ? AND ms.target_name = ?
		 ORDER BY ms.requested_at DESC, ms.id DESC
		 LIMIT ?`)

	rows, err := r.db.QueryContext(ctx, query, productName, target, limit)
	if err != nil {
		return nil, fmt.Errorf("list syncs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MirrorSync
	for rows.Next() {
		var s MirrorSync
		if err := rows.Scan(&s.ID, &s.Product, &s.TargetName, &s.TransferID,
			&s.RequestedAt, &s.StartedAt, &s.FinishedAt,
			&s.Status, &s.QuayStatus, &s.ObservedDigest, &s.ExpectedDigest, &s.Message); err != nil {
			return nil, fmt.Errorf("scan sync: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ErrNoRecord is returned when a target has never been applied or observed.
var ErrNoRecord = errors.New("no replication record")

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func nullableInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullableStr(s sql.NullString) any {
	if !s.Valid || s.String == "" {
		return nil
	}
	return s.String
}

// ---------------------------------------------------------------------------
// Delegated transfers
//
// A transfer whose destination registry fetches for itself creates no jobs, so
// none of the queue's machinery applies to it. These three calls are the whole
// of its lifecycle: it enters `syncing`, it is polled, and it settles.
// ---------------------------------------------------------------------------

// MarkSyncing records that a transfer has been handed to the registry.
//
// `strategy` is stored on the row so that a year-old record still says HOW the
// content got there. Without it a settled transfer with no jobs and no bytes is
// indistinguishable from one that failed to plan.
func (r *Replication) MarkSyncing(ctx context.Context, transferID, strategy string) error {
	stmt := r.dialect.Rewrite(`
		UPDATE transfers
		   SET state = 'syncing', strategy = ?, started_at = COALESCE(started_at, ` + r.dialect.Now() + `),
		       updated_at = ` + r.dialect.Now() + `
		 WHERE id = ? AND state IN ('pending','planning','failed')`)
	if _, err := r.db.ExecContext(ctx, stmt, strategy, transferID); err != nil {
		return fmt.Errorf("mark transfer %s syncing: %w", transferID, err)
	}
	return nil
}

// SettleDelegated closes a delegated transfer.
//
// state is `succeeded`, `diverged` or `failed`. `diverged` is deliberately not
// a failure: the sync completed and the destination holds a different digest
// than the one we asked for, which is a mirror faithfully following an upstream
// tag that moved.
func (r *Replication) SettleDelegated(ctx context.Context, transferID, state, reason string) error {
	switch state {
	case "succeeded", "diverged", "failed":
	default:
		return fmt.Errorf("settle transfer %s: %q is not a delegated outcome", transferID, state)
	}
	stmt := r.dialect.Rewrite(`
		UPDATE transfers
		   SET state = ?, failure_reason = ?, completed_at = ` + r.dialect.Now() + `,
		       updated_at = ` + r.dialect.Now() + `
		 WHERE id = ? AND state = 'syncing'`)
	if _, err := r.db.ExecContext(ctx, stmt, state, nullIfBlank(reason), transferID); err != nil {
		return fmt.Errorf("settle transfer %s as %s: %w", transferID, state, err)
	}
	return nil
}

// OpenSync is a delegated transfer waiting on the registry.
type OpenSync struct {
	SyncID      int64
	TransferID  string
	ProductID   int64
	Product     string
	TargetName  string
	RequestedAt sql.NullString

	// ExpectedDigest is what the destination should end up holding. Empty when
	// the sync was not requested for a specific package.
	ExpectedDigest string
	// Tag and Repository are what to look up at the destination.
	Tag        string
	Repository string
}

// OpenSyncs lists delegated transfers that have not settled.
//
// Driven from `transfers` rather than from `mirror_syncs`, because the transfer
// is the thing that must not be left hanging: a sync row we failed to write
// would otherwise make a transfer invisible to the watcher forever.
func (r *Replication) OpenSyncs(ctx context.Context, limit int) ([]OpenSync, error) {
	if limit <= 0 {
		limit = 50
	}
	query := r.dialect.Rewrite(`
		SELECT COALESCE(ms.id, 0), t.id, pr.id, pr.name, tgt.name,
		       ms.requested_at, COALESCE(ms.expected_digest, ''), p.tag, tgt.repository_path
		  FROM transfers t
		  JOIN transfer_requests req ON req.id = t.request_id
		  JOIN products pr           ON pr.id = req.product_id
		  JOIN packages p            ON p.id = t.package_id
		  JOIN repositories tgt      ON tgt.id = t.target_repo_id
		  LEFT JOIN mirror_syncs ms  ON ms.transfer_id = t.id
		 WHERE t.state = 'syncing'
		 ORDER BY t.updated_at
		 LIMIT ?`)

	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("list open syncs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []OpenSync
	for rows.Next() {
		var s OpenSync
		if err := rows.Scan(&s.SyncID, &s.TransferID, &s.ProductID, &s.Product, &s.TargetName,
			&s.RequestedAt, &s.ExpectedDigest, &s.Tag, &s.Repository); err != nil {
			return nil, fmt.Errorf("scan open sync: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LinkSync attaches a sync record to the transfer that requested it.
func (r *Replication) LinkSync(ctx context.Context, syncID int64, transferID string) error {
	stmt := r.dialect.Rewrite(`UPDATE mirror_syncs SET transfer_id = ?, updated_at = ` +
		r.dialect.Now() + ` WHERE id = ?`)
	if _, err := r.db.ExecContext(ctx, stmt, transferID, syncID); err != nil {
		return fmt.Errorf("link sync %d to transfer %s: %w", syncID, transferID, err)
	}
	return nil
}

func nullIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SetExpectedDigest records what the destination SHOULD end up holding.
//
// Written when the sync is requested, never later: a digest looked up after the
// fact would be the destination compared with itself, and `diverged` would
// become undetectable.
func (r *Replication) SetExpectedDigest(ctx context.Context, syncID int64, digest string) error {
	stmt := r.dialect.Rewrite(`UPDATE mirror_syncs SET expected_digest = ?, updated_at = ` +
		r.dialect.Now() + ` WHERE id = ?`)
	if _, err := r.db.ExecContext(ctx, stmt, digest, syncID); err != nil {
		return fmt.Errorf("record expected digest for sync %d: %w", syncID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Download-rule chains
//
// A step waits for its predecessor to reach `succeeded` — and because a
// transfer only reaches `succeeded` after its own verification, that IS the
// verification gate. There is no separate gate mechanism and there does not
// need to be one.
// ---------------------------------------------------------------------------

// AdvanceSteps releases steps whose predecessor has succeeded and skips those
// whose predecessor cannot succeed.
//
// Two statements rather than one, because the two outcomes are not symmetric:
// releasing puts work into the queue, and skipping settles a transfer that will
// never run. Reported separately so a caller can say which happened.
//
// A sweep rather than a hook on completion, for the reason every other
// recovery path in this system is a sweep: the interesting failure is the one
// where nothing calls the hook — the Coordinator restarts between a step
// succeeding and its successor being released, and a chain that only advanced
// on an event would hang there forever.
func (r *Replication) AdvanceSteps(ctx context.Context) (released, skipped int, err error) {
	release := r.dialect.Rewrite(`
		UPDATE transfers
		   SET state = 'planning', updated_at = ` + r.dialect.Now() + `
		 WHERE state = 'waiting'
		   AND depends_on_transfer_id IN (SELECT id FROM transfers WHERE state = 'succeeded')`)
	res, err := r.db.ExecContext(ctx, release)
	if err != nil {
		return 0, 0, fmt.Errorf("release waiting steps: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		released = int(n)
	}

	// `diverged` counts as unsuccessful for a chain, and that is a judgement
	// worth stating: the destination holds content we did not ask for, so a
	// step that would copy FROM it would propagate the wrong digest onward.
	// The divergence is recorded and notifiable on its own step; what it must
	// not do is quietly become the input to the next one.
	skip := r.dialect.Rewrite(`
		UPDATE transfers
		   SET state = 'skipped',
		       failure_reason = 'the step this one depends on did not succeed',
		       completed_at = ` + r.dialect.Now() + `, updated_at = ` + r.dialect.Now() + `
		 WHERE state = 'waiting'
		   AND depends_on_transfer_id IN (
		         SELECT id FROM transfers
		          WHERE state IN ('failed','cancelled','skipped','diverged'))`)
	res, err = r.db.ExecContext(ctx, skip)
	if err != nil {
		return released, 0, fmt.Errorf("skip orphaned steps: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		skipped = int(n)
	}
	return released, skipped, nil
}
