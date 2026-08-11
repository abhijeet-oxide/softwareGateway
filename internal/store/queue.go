package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/backoff"
)

// The queue half of persistence: the dequeue, leases, completion, the reaper
// and wave advancement. See docs/design/04-queue-and-scheduling.md.
//
// # Why this file is hand-written SQL rather than generated
//
// docs/design/16 §6 deferred `sqlc` twice and said the decision would be made
// here, against the dequeue — "hand-tuned, correctness-critical, and
// dialect-divergent enough that it will not go through the rewriter at all".
// That turned out to be exactly right, and the answer is still hand-written.
// See the divergence note in docs/design/16 §6 for the reasoning; the short
// version is that the two dialects need two genuinely different statements, so
// there is no single query for a generator to check.
//
// # The one thing to understand before changing anything here
//
// A job is leasable if and only if `state = 'pending'`. Wave gating is
// resolved INTO that column at plan time and at wave-advance time, never
// evaluated at dequeue time (docs/design/04 §3.3). If you add a join to the
// dequeue you have undone the reason the hot index works.

// LeaseRequest is one worker asking for work.
type LeaseRequest struct {
	// Owner identifies the worker. It is the only thing tying a job to the
	// process holding it, and it is opaque here.
	Owner string
	// Limit caps how many jobs come back. The caller has already reduced this
	// to the worker's remaining capacity.
	Limit int
	// Duration is how long the lease is good for. The worker renews by
	// heartbeat; a worker that stops heartbeating loses its jobs to the reaper
	// after this elapses.
	Duration time.Duration
}

// LeasedJob is one unit of work handed to a worker.
//
// Deliberately free of registry hosts, paths and credentials: those are
// hydrated separately by HydrateEndpoints, so the dequeue statement itself
// stays join-free. See the note at the top of this file.
type LeasedJob struct {
	ID         int64
	TransferID string
	// Kind is "blob" or "manifest".
	Kind       string
	Digest     string
	SizeBytes  int64
	MediaType  string
	ArtifactID *int64

	SourceRepoID int64
	TargetRepoID int64

	// Attempt is this job's attempt number, already incremented by the lease.
	Attempt int
	Wave    int

	// TargetTags are the tags to apply once this manifest is committed,
	// resolved at planning time from the artifact's own reference annotation.
	// Empty for a blob, and for any manifest the source did not name.
	TargetTags []string
	// TargetRepository is the destination path, carried for diagnostics so a
	// stuck job says where it was going without a join.
	TargetRepository string
}

// LeaseJobs atomically hands up to Limit pending jobs to one worker.
//
// `attempts` is incremented AT LEASE TIME, not on failure. A worker that dies
// without reporting anything has still consumed an attempt, so a job that
// reliably kills its worker cannot loop forever. Counting only reported
// failures would make a poison job immortal (docs/design/04 §4.1).
//
// Per-repository budget filtering (§4.1's `$1`/`$2` repository arrays) is NOT
// implemented here. Budgets are the M7 backpressure controller's job; until it
// exists there is nothing to divide, and a filter over an empty budget set
// would either exclude everything or be a no-op pretending to be a limit. The
// shape of this function does not change when it lands — an extra clause in
// the candidate CTE.
func (p *Packages) LeaseJobs(ctx context.Context, req LeaseRequest) ([]LeasedJob, error) {
	if req.Owner == "" {
		return nil, errors.New("lease: a worker ID is required")
	}
	if req.Limit <= 0 {
		return nil, nil
	}
	if req.Duration <= 0 {
		req.Duration = 2 * time.Minute
	}

	if p.dialect.Name() == DriverPostgres {
		return p.leasePostgres(ctx, req)
	}
	return p.leaseSQLite(ctx, req)
}

// leaseColumns is the RETURNING/SELECT list both dialects share, so the two
// implementations cannot drift in what they produce.
const leaseColumns = `id, transfer_id, kind, digest, size_bytes, media_type,
	artifact_id, source_repo_id, target_repo_id, attempts, wave, target_tags,
	target_repository`

// leaseCandidatePredicate is the leasability test, shared by both dialects.
//
// The NOT EXISTS is concurrent duplicate suppression (docs/design/04 §5): if
// another worker is already moving this exact blob to this exact repository,
// skip it. The skipped job stays pending and is picked up moments later — by
// which time the first has completed and written a placement, so the second
// hits the placement fast path and moves zero bytes.
func (p *Packages) leaseCandidatePredicate() string {
	return `state = 'pending'
	   AND NOT paused
	   AND next_visible_at <= ` + p.dialect.Now() + `
	   AND NOT EXISTS (
	         SELECT 1 FROM jobs inflight
	          WHERE inflight.state = 'leased'
	            AND inflight.target_repo_id = jobs.target_repo_id
	            AND inflight.digest = jobs.digest
	       )`
}

// leasePostgres is the real thing: one statement, one round trip.
//
// FOR UPDATE SKIP LOCKED is what makes this safe under concurrency. Without
// it, ten workers leasing simultaneously would serialize behind row locks;
// with it, each takes a different set and none waits.
func (p *Packages) leasePostgres(ctx context.Context, req LeaseRequest) ([]LeasedJob, error) {
	query := p.dialect.Rewrite(`
		WITH candidate AS (
		    SELECT id
		      FROM jobs
		     WHERE ` + p.leaseCandidatePredicate() + `
		     ORDER BY priority DESC, next_visible_at, id
		       FOR UPDATE SKIP LOCKED
		     LIMIT ?
		)
		UPDATE jobs j
		   SET state            = 'leased',
		       lease_owner      = ?,
		       lease_expires_at = ` + p.dialect.TimeAhead("?") + `,
		       attempts         = attempts + 1,
		       started_at       = COALESCE(j.started_at, ` + p.dialect.Now() + `),
		       updated_at       = ` + p.dialect.Now() + `
		  FROM candidate c
		 WHERE j.id = c.id
		RETURNING j.` + strings.ReplaceAll(leaseColumns, ", ", ", j."))

	rows, err := p.db.QueryContext(ctx, query, req.Limit, req.Owner, req.Duration.Seconds())
	if err != nil {
		return nil, fmt.Errorf("lease jobs for %s: %w", req.Owner, err)
	}
	defer func() { _ = rows.Close() }()

	return scanLeasedJobs(rows)
}

// leaseSQLite does in a transaction what Postgres does in one statement.
//
// SQLite has no SKIP LOCKED and needs none: it serializes writers against the
// whole database, so two concurrent leases cannot interleave and select the
// same row. The transaction is what makes select-then-update equivalent to the
// Postgres CTE rather than a check-then-act race.
//
// This is the dialect divergence docs/design/16 §6 predicted. The two
// statements are not a rewriter apart; they are different algorithms resting
// on different concurrency guarantees, and pretending otherwise by forcing
// them through one template would hide that.
func (p *Packages) leaseSQLite(ctx context.Context, req LeaseRequest) ([]LeasedJob, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin lease transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids, err := p.leaseCandidates(ctx, tx, req.Limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	placeholders, args := inClause(ids)
	args = append([]any{req.Owner, req.Duration.Seconds()}, args...)

	update := p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = 'leased',
		       lease_owner      = ?,
		       lease_expires_at = ` + p.dialect.TimeAhead("?") + `,
		       attempts         = attempts + 1,
		       started_at       = COALESCE(started_at, ` + p.dialect.Now() + `),
		       updated_at       = ` + p.dialect.Now() + `
		 WHERE id IN (` + placeholders + `)`)

	if _, err := tx.ExecContext(ctx, update, args...); err != nil {
		return nil, fmt.Errorf("lease jobs for %s: %w", req.Owner, err)
	}

	selectPlaceholders, selectArgs := inClause(ids)
	rows, err := tx.QueryContext(ctx, p.dialect.Rewrite(
		`SELECT `+leaseColumns+` FROM jobs WHERE id IN (`+selectPlaceholders+`) ORDER BY id`),
		selectArgs...)
	if err != nil {
		return nil, fmt.Errorf("read leased jobs for %s: %w", req.Owner, err)
	}

	out, err := scanLeasedJobs(rows)
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit lease: %w", err)
	}
	return out, nil
}

func (p *Packages) leaseCandidates(ctx context.Context, tx *sql.Tx, limit int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT id FROM jobs
		 WHERE `+p.leaseCandidatePredicate()+`
		 ORDER BY priority DESC, next_visible_at, id
		 LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("select lease candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan lease candidate: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanLeasedJobs(rows *sql.Rows) ([]LeasedJob, error) {
	var out []LeasedJob
	for rows.Next() {
		var j LeasedJob
		var mediaType sql.NullString
		var tags []byte
		var targetRepository sql.NullString
		if err := rows.Scan(&j.ID, &j.TransferID, &j.Kind, &j.Digest, &j.SizeBytes,
			&mediaType, &j.ArtifactID, &j.SourceRepoID, &j.TargetRepoID,
			&j.Attempt, &j.Wave, &tags, &targetRepository); err != nil {
			return nil, fmt.Errorf("scan leased job: %w", err)
		}
		j.MediaType = mediaType.String
		j.TargetTags = decodeTags(tags)
		j.TargetRepository = targetRepository.String
		out = append(out, j)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

// Endpoint is where a job's bytes come from or go to.
//
// The worker holds no database credentials and no catalog, so everything it
// needs to build a registry client travels with the job. CredentialsRef is a
// NAME, not a secret: the worker resolves it against its own projected secret
// volume, exactly as the Coordinator does. No credential is ever serialized
// into an API response.
type Endpoint struct {
	RepositoryID int64
	Product      string
	// Name is the configured repository name — the `sources[].name` or
	// `targets[].name` the worker resolves credentials by.
	Name         string
	Registry     string
	Repository   string
	RegistryType string
	Role         string
}

// HydrateEndpoints resolves repository row IDs into everything a worker needs
// to talk to them.
//
// Separate from the dequeue on purpose. The dequeue's index is a clean match
// for its ORDER BY only while it stays join-free (docs/design/04 §4.2), so the
// joins live here — one extra round trip per lease BATCH, amortized across up
// to sixteen jobs, rather than a join on the hot path.
func (p *Packages) HydrateEndpoints(ctx context.Context, ids []int64) (map[int64]Endpoint, error) {
	out := map[int64]Endpoint{}
	if len(ids) == 0 {
		return out, nil
	}

	placeholders, args := inClause(dedupeInt64(ids))
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT r.id, p.name, r.name, r.registry_host, r.repository_path,
		       r.registry_type, r.role
		  FROM repositories r
		  JOIN products p ON p.id = r.product_id
		 WHERE r.id IN (`+placeholders+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("resolve repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.RepositoryID, &e.Product, &e.Name, &e.Registry,
			&e.Repository, &e.RegistryType, &e.Role); err != nil {
			return nil, fmt.Errorf("scan repository: %w", err)
		}
		out[e.RepositoryID] = e
	}
	return out, rows.Err()
}

// TransferTag returns the tag a transfer's top-level manifest must carry at
// the destination, and the package's root digest.
//
// The tag is applied LAST, only after the index manifest is committed —
// invariant I1. Until that moment the destination holds a set of unreferenced
// blobs, which are harmless, invisible to consumers, and useful to the next
// transfer.
func (p *Packages) TransferTag(ctx context.Context, transferID string) (tag, digest string, err error) {
	row := p.db.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT pk.tag, pk.manifest_digest
		  FROM transfers t
		  JOIN packages pk ON pk.id = t.package_id
		 WHERE t.id = ?`), transferID)

	if err := row.Scan(&tag, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("transfer %s not found", transferID)
		}
		return "", "", fmt.Errorf("read transfer %s tag: %w", transferID, err)
	}
	return tag, digest, nil
}

// ---------------------------------------------------------------------------
// Progress, renewal, completion
// ---------------------------------------------------------------------------

// RenewLeases extends the leases a worker still holds, reporting which were
// actually renewed.
//
// The `lease_owner = ?` and `state = 'leased'` predicates are what make this
// safe: a worker whose lease already expired and was reaped renews nothing,
// learns that from the returned set, and abandons the job rather than
// finishing work another worker has since redone.
func (p *Packages) RenewLeases(
	ctx context.Context, owner string, ids []int64, d time.Duration,
) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if d <= 0 {
		d = 2 * time.Minute
	}

	placeholders, idArgs := inClause(ids)
	args := append([]any{d.Seconds(), owner}, idArgs...)

	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET lease_expires_at = `+p.dialect.TimeAhead("?")+`,
		       updated_at       = `+p.dialect.Now()+`
		 WHERE lease_owner = ? AND state = 'leased' AND id IN (`+placeholders+`)`),
		args...); err != nil {
		return nil, fmt.Errorf("renew leases for %s: %w", owner, err)
	}

	// Read back rather than trusting RowsAffected: the worker needs to know
	// WHICH leases it still holds, not how many.
	selectPlaceholders, selectIDs := inClause(ids)
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(
		`SELECT id FROM jobs
		  WHERE lease_owner = ? AND state = 'leased' AND id IN (`+selectPlaceholders+`)`),
		append([]any{owner}, selectIDs...)...)
	if err != nil {
		return nil, fmt.Errorf("read renewed leases for %s: %w", owner, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan renewed lease: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ReportProgress records how far a blob has got.
//
// LOSSY BY DESIGN (docs/design/09 §7.2). This is a UI signal; dropping one
// costs nothing, so it takes no transaction and no lock. `complete` is the
// call that is not lossy.
func (p *Packages) ReportProgress(ctx context.Context, jobID int64, owner string, bytes int64) error {
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET bytes_transferred = ?, updated_at = `+p.dialect.Now()+`
		 WHERE id = ? AND lease_owner = ? AND state = 'leased'`),
		bytes, jobID, owner); err != nil {
		return fmt.Errorf("report progress for job %d: %w", jobID, err)
	}
	return nil
}

// Completion is a worker's report on one finished job.
type Completion struct {
	JobID int64
	Owner string
	// Outcome is succeeded | skipped | failed | cancelled.
	Outcome          string
	BytesTransferred int64
	// SkipReason is placement_hit | exists_at_target | mounted, and must be set
	// when Outcome is skipped — the CHECK constraint enforces the vocabulary,
	// and the distinction is what makes a dedupe regression visible.
	SkipReason string
	ErrorClass string
	ErrorMsg   string
	// Placed records that the destination now holds this blob — whether the
	// bytes moved, the registry relocated them server-side, or a HEAD found
	// them already there. All three earn a placement row: the blob IS there,
	// and how it got there is carried by SkipReason.
	Placed bool
	// Attempt is which attempt this was, used only to size the retry backoff.
	//
	// Taken from the worker's copy rather than re-read inside the completion
	// transaction: the lease set it and the two always agree, so a second read
	// would buy nothing.
	Attempt int
}

// CompletionResult reports what the completion did beyond the job itself.
type CompletionResult struct {
	// Applied is false when the job was not this worker's to complete —
	// its lease had expired and been reaped, and another worker may already
	// have redone it. Not an error: it is the expected outcome of a slow
	// worker finishing late, and the correct response is to drop the result.
	Applied bool
	// WaveAdvanced reports that this completion drained a wave and opened the
	// next one.
	WaveAdvanced bool
	// TransferState is the transfer's state after this completion.
	TransferState string
	NewWave       int
}

// CompleteJob records a finished job and everything that follows from it, in
// ONE transaction: job state, the blob placement, the wave-drain check and any
// wave advancement, and the transfer's own state.
//
// This atomicity is the reason the queue lives in the same database as the
// state (docs/design/03 §1). With a broker, "job succeeded" and "transfer
// progressed" are two writes to two systems and cannot be made atomic without
// an outbox or a reconciler. Here it is one COMMIT.
func (p *Packages) CompleteJob(ctx context.Context, c Completion) (CompletionResult, error) {
	var res CompletionResult

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the job under the transaction, and confirm this worker still owns
	// it. A worker whose lease expired mid-transfer must not be able to mark
	// succeeded a job the reaper has already returned to the queue.
	var (
		transferID   string
		kind, digest string
		size         int64
		targetRepoID int64
		wave         int
		owner        sql.NullString
		state        string
	)
	err = tx.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT transfer_id, kind, digest, size_bytes, target_repo_id, wave, lease_owner, state
		  FROM jobs WHERE id = ?`), c.JobID).
		Scan(&transferID, &kind, &digest, &size, &targetRepoID, &wave, &owner, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("job %d not found", c.JobID)
	}
	if err != nil {
		return res, fmt.Errorf("read job %d: %w", c.JobID, err)
	}

	if state != "leased" || !owner.Valid || owner.String != c.Owner {
		// Stale. Content addressing is what makes this harmless: a late worker
		// either wrote identical bytes or was rejected by the registry.
		return res, nil
	}
	res.Applied = true

	if err := p.applyJobOutcome(ctx, tx, c); err != nil {
		return res, err
	}

	// A blob that reached the destination — by transfer, by mount, or by being
	// found already there — is a placement. Recording it is what makes the
	// NEXT transfer of a product line nearly free.
	if c.Placed && kind == "blob" {
		source := placementSource(c)
		if err := p.RecordPlacement(ctx, tx, targetRepoID, digest, size, source); err != nil {
			return res, err
		}
	}

	advanced, newWave, transferState, err := p.settleTransfer(ctx, tx, transferID, wave)
	if err != nil {
		return res, err
	}
	res.WaveAdvanced, res.NewWave, res.TransferState = advanced, newWave, transferState

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit completion of job %d: %w", c.JobID, err)
	}
	return res, nil
}

// placementSource maps a completion onto the blob_placements vocabulary.
//
// The distinction is kept because an observed placement is the weakest
// evidence and the first thing to doubt when something is missing.
func placementSource(c Completion) string {
	switch c.SkipReason {
	case "mounted":
		return "mounted"
	case "exists_at_target", "placement_hit":
		return "observed"
	default:
		return "transferred"
	}
}

// applyJobOutcome writes the job's terminal state, or schedules its retry.
//
// A failure is only terminal once attempts are exhausted. Until then the job
// returns to `pending` behind a backoff, and — crucially — keeps
// bytes_transferred, so a retry resumes the accounting rather than restarting
// it (docs/design/04 §11).
func (p *Packages) applyJobOutcome(ctx context.Context, tx *sql.Tx, c Completion) error {
	if c.Outcome != "failed" {
		skip := nullIfEmpty(c.SkipReason)
		_, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			UPDATE jobs
			   SET state             = ?,
			       skip_reason       = ?,
			       bytes_transferred = ?,
			       lease_owner       = NULL,
			       lease_expires_at  = NULL,
			       completed_at      = `+p.dialect.Now()+`,
			       updated_at        = `+p.dialect.Now()+`
			 WHERE id = ?`),
			c.Outcome, skip, c.BytesTransferred, c.JobID)
		if err != nil {
			return fmt.Errorf("complete job %d: %w", c.JobID, err)
		}
		return nil
	}

	// Failed. Exhausted or retryable is decided in SQL against the row's own
	// attempts and max_attempts, so the decision cannot drift from the value
	// the lease incremented.
	_, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		       last_error        = ?,
		       last_error_class  = ?,
		       bytes_transferred = ?,
		       lease_owner       = NULL,
		       lease_expires_at  = NULL,
		       next_visible_at   = `+p.dialect.TimeAhead("?")+`,
		       completed_at      = CASE WHEN attempts >= max_attempts
		                                THEN `+p.dialect.Now()+` ELSE NULL END,
		       updated_at        = `+p.dialect.Now()+`
		 WHERE id = ?`),
		nullIfEmpty(c.ErrorMsg), nullIfEmpty(c.ErrorClass), c.BytesTransferred,
		retryDelay(c.Attempt).Seconds(), c.JobID)
	if err != nil {
		return fmt.Errorf("fail job %d: %w", c.JobID, err)
	}
	return nil
}

// retryDelay is the wait before a failed job becomes visible again.
//
// Full jitter, from the shared policy, because the common failure here is
// CORRELATED: a registry returning 503 fails all forty in-flight jobs at once.
// Without jitter they retry together and re-hammer an already-struggling
// registry in synchronized waves (docs/design/04 §11).
func retryDelay(attempt int) time.Duration { return backoff.Policy{}.Delay(attempt) }

// settleTransfer runs the wave-drain check and advances or finishes the
// transfer.
//
// Returns whether a wave advanced, the transfer's current wave, and its state.
func (p *Packages) settleTransfer(
	ctx context.Context, tx *sql.Tx, transferID string, wave int,
) (advanced bool, newWave int, state string, err error) {
	var currentWave, maxWave int
	err = tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT current_wave, max_wave, state FROM transfers WHERE id = ?`), transferID).
		Scan(&currentWave, &maxWave, &state)
	if err != nil {
		return false, 0, "", fmt.Errorf("read transfer %s: %w", transferID, err)
	}

	// A transfer with work in flight is running, whatever it was before. Doing
	// this here rather than at lease time keeps the dequeue free of a write to
	// a second table.
	if state == "ready" {
		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
			`UPDATE transfers SET state = 'running', started_at = COALESCE(started_at, `+
				p.dialect.Now()+`), updated_at = `+p.dialect.Now()+` WHERE id = ?`),
			transferID); err != nil {
			return false, currentWave, state, fmt.Errorf("start transfer %s: %w", transferID, err)
		}
		state = "running"
	}

	drained, err := p.waveDrained(ctx, tx, transferID, wave)
	if err != nil {
		return false, currentWave, state, err
	}
	if !drained || wave != currentWave {
		return false, currentWave, state, nil
	}

	// Advance THROUGH empty waves rather than one step per completion. A wave
	// with no jobs of its own has nothing to complete and so nothing to drive
	// the next advance — stopping at one would stall the transfer on any gap
	// in the wave numbering.
	for next := currentWave + 1; next <= maxWave; next++ {
		if err := p.advanceWave(ctx, tx, transferID, next); err != nil {
			return false, currentWave, state, err
		}
		occupied, err := p.waveOccupied(ctx, tx, transferID, next)
		if err != nil {
			return false, next, state, err
		}
		if occupied {
			return true, next, state, nil
		}
	}

	// Drained at the top wave. Every job is accounted for and the tag is
	// pushed, so the transfer is done.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET state = 'succeeded', completed_at = `+p.dialect.Now()+
			`, updated_at = `+p.dialect.Now()+` WHERE id = ?`), transferID); err != nil {
		return false, currentWave, state, fmt.Errorf("finish transfer %s: %w", transferID, err)
	}
	return false, currentWave, "succeeded", nil
}

// waveDrained reports whether every job in a wave has reached a terminal
// SUCCESSFUL state.
//
// A `failed` job (terminal, attempts exhausted) never satisfies this, so the
// transfer correctly stalls rather than pushing a manifest whose blobs are
// missing (docs/design/04 §3.4).
func (p *Packages) waveDrained(
	ctx context.Context, tx *sql.Tx, transferID string, wave int,
) (bool, error) {
	var outstanding int
	err := tx.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT count(*) FROM jobs
		 WHERE transfer_id = ? AND wave = ?
		   AND state NOT IN ('succeeded','skipped')`), transferID, wave).Scan(&outstanding)
	if err != nil {
		return false, fmt.Errorf("wave drain check for transfer %s: %w", transferID, err)
	}
	return outstanding == 0, nil
}

// waveOccupied reports whether a wave has any job still to run.
func (p *Packages) waveOccupied(
	ctx context.Context, tx *sql.Tx, transferID string, wave int,
) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT count(*) FROM jobs
		 WHERE transfer_id = ? AND wave = ?
		   AND state IN ('pending','blocked','leased')`), transferID, wave).Scan(&n); err != nil {
		return false, fmt.Errorf("check wave %d of transfer %s: %w", wave, transferID, err)
	}
	return n > 0, nil
}

// advanceWave promotes the next wave's jobs from `blocked` to `pending`.
//
// One bulk UPDATE, no per-job dependency resolution. This is what lets the
// dequeue path stay join-free: wave gating has already been resolved into the
// state column by the time a worker looks (docs/design/04 §3.3).
func (p *Packages) advanceWave(ctx context.Context, tx *sql.Tx, transferID string, next int) error {
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET current_wave = ?, updated_at = `+p.dialect.Now()+
			` WHERE id = ?`), next, transferID); err != nil {
		return fmt.Errorf("advance transfer %s to wave %d: %w", transferID, next, err)
	}

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs SET state = 'pending', updated_at = `+p.dialect.Now()+`
		 WHERE transfer_id = ? AND wave = ? AND state = 'blocked'`),
		transferID, next); err != nil {
		return fmt.Errorf("open wave %d of transfer %s: %w", next, transferID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The reaper
// ---------------------------------------------------------------------------

// ReapedJob is one job whose lease expired.
type ReapedJob struct {
	ID         int64
	TransferID string
	// State is what the job became: `pending` if it will be retried, `failed`
	// if its attempts were exhausted.
	State string
}

// ReapExpiredLeases returns jobs held by workers that stopped heartbeating.
//
// THIS IS THE ENTIRE WORKER CRASH-RECOVERY STORY. A worker that is SIGKILLed,
// whose node is preempted, or which is partitioned performs no cleanup, sends
// no message and runs no shutdown hook. Its work returns to the queue within
// one lease period. There is no handshake to get wrong, no tombstone to leak,
// and no difference in handling between "crashed", "network-partitioned" and
// "scaled down" — the only signal is a timestamp that stopped advancing.
//
// The one correctness requirement is that a stale worker's in-flight upload
// must not corrupt anything after another worker retakes the job. It cannot:
// OCI blob uploads are digest-verified by the registry on completion, so a
// stale worker finishing late either writes identical bytes or is rejected.
func (p *Packages) ReapExpiredLeases(ctx context.Context) ([]ReapedJob, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, p.dialect.Rewrite(
		`SELECT id, transfer_id, attempts, max_attempts FROM jobs
		  WHERE state = 'leased' AND lease_expires_at < `+p.dialect.Now()))
	if err != nil {
		return nil, fmt.Errorf("find expired leases: %w", err)
	}

	type expired struct {
		id                    int64
		transferID            string
		attempts, maxAttempts int
	}
	var found []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.transferID, &e.attempts, &e.maxAttempts); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan expired lease: %w", err)
		}
		found = append(found, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}

	out := make([]ReapedJob, 0, len(found))
	for _, e := range found {
		state := "pending"
		if e.attempts >= e.maxAttempts {
			state = "failed"
		}

		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			UPDATE jobs
			   SET state             = ?,
			       lease_owner       = NULL,
			       lease_expires_at  = NULL,
			       next_visible_at   = `+p.dialect.TimeAhead("?")+`,
			       last_error        = COALESCE(last_error, 'lease expired'),
			       last_error_class  = 'lease_expired',
			       completed_at      = CASE WHEN ? = 'failed' THEN `+p.dialect.Now()+` ELSE NULL END,
			       updated_at        = `+p.dialect.Now()+`
			 WHERE id = ? AND state = 'leased'`),
			state, retryDelay(e.attempts).Seconds(), state, e.id); err != nil {
			return nil, fmt.Errorf("reap job %d: %w", e.id, err)
		}
		out = append(out, ReapedJob{ID: e.id, TransferID: e.transferID, State: state})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reap: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Request expansion
// ---------------------------------------------------------------------------

// PendingRequest is a transfer request that has not yet become jobs.
type PendingRequest struct {
	ID           string
	ProductID    int64
	ProductName  string
	PackageID    int64
	Operation    string
	SourceRepoID int64
	Priority     int
}

// PendingRequests returns requests waiting to be expanded into transfers.
//
// Scheduled requests are deliberately excluded: the queue contains only
// EXECUTABLE work, and a download scheduled for next Tuesday is an
// appointment, not work (docs/design/04 §10). It becomes `pending` when the
// scheduler finds it due.
func (p *Packages) PendingRequests(ctx context.Context, limit int) ([]PendingRequest, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT r.id, r.product_id, pr.name, r.package_id, r.operation,
		       r.source_repo_id, r.priority
		  FROM transfer_requests r
		  JOIN products pr ON pr.id = r.product_id
		 WHERE r.state = 'pending'
		 ORDER BY r.priority DESC, r.created_at, r.id
		 LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending transfer requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PendingRequest
	for rows.Next() {
		var r PendingRequest
		if err := rows.Scan(&r.ID, &r.ProductID, &r.ProductName, &r.PackageID,
			&r.Operation, &r.SourceRepoID, &r.Priority); err != nil {
			return nil, fmt.Errorf("scan pending transfer request: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRequestState moves a request through its lifecycle.
//
// `failure` is recorded on the transfers the request produced rather than on
// the request itself: the request row has no failure column, because a request
// that expanded into four transfers can have three succeed and one fail, and
// a single reason on the request could only be wrong.
func (p *Packages) SetRequestState(ctx context.Context, requestID, state string) error {
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfer_requests SET state = ?, updated_at = `+p.dialect.Now()+
			` WHERE id = ?`), state, requestID); err != nil {
		return fmt.Errorf("set request %s to %s: %w", requestID, state, err)
	}
	return nil
}

// FailTransfer marks a transfer failed with a stated reason.
func (p *Packages) FailTransfer(ctx context.Context, transferID, reason string) error {
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET state = 'failed', failure_reason = ?,
		        completed_at = `+p.dialect.Now()+`, updated_at = `+p.dialect.Now()+`
		  WHERE id = ? AND state NOT IN ('succeeded','cancelled')`),
		reason, transferID); err != nil {
		return fmt.Errorf("fail transfer %s: %w", transferID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// inClause builds a placeholder list and its arguments.
func inClause(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "NULL", nil
	}
	var b strings.Builder
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	return b.String(), args
}

func dedupeInt64(in []int64) []int64 {
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// TransferIDFor returns the transfer already opened for a request and target.
//
// Needed because CreateTransfer's ON CONFLICT DO NOTHING reports that a row
// exists without saying which — and on the retry path, which is the normal
// path after a Coordinator restart mid-expansion, the caller needs the ID to
// carry on planning rather than a fresh UUID that would violate the
// constraint.
func (p *Packages) TransferIDFor(ctx context.Context, requestID string, targetRepoID int64) (string, error) {
	var id string
	err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT id FROM transfers WHERE request_id = ? AND target_repo_id = ?`),
		requestID, targetRepoID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no transfer for request %s to repository %d", requestID, targetRepoID)
	}
	if err != nil {
		return "", fmt.Errorf("find transfer for request %s: %w", requestID, err)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Reading transfers
// ---------------------------------------------------------------------------

// TransferSummary is one transfer with its progress rolled up.
//
// PROGRESS IS ALWAYS A ROLLUP, never a maintained counter (invariant I6). A
// counter incremented alongside the jobs would be a second source of truth for
// the same fact, and the two would drift the first time a completion was
// applied twice or a job was reaped mid-report. Deriving it costs one indexed
// aggregate and cannot be wrong.
type TransferSummary struct {
	ID          string
	RequestID   string
	PackageID   int64
	ProductName string
	Tag         string
	Source      string
	Target      string
	State       string
	Priority    int

	CurrentWave int
	MaxWave     int

	PlannedJobs        int
	PlannedBytes       int64
	DedupeSkippedBytes int64

	// Rolled up from jobs.
	JobsDone         int
	JobsFailed       int
	JobsOutstanding  int
	BytesTransferred int64

	FailureReason string
	CreatedAt     string
	CompletedAt   string
}

// transferSelect is the shared projection, so list and get cannot disagree
// about what a transfer looks like.
const transferSelect = `
	SELECT t.id, t.request_id, t.package_id, pr.name, pk.tag,
	       src.registry_host || '/' || src.repository_path,
	       dst.registry_host || '/' || dst.repository_path,
	       t.state, t.priority, t.current_wave, t.max_wave,
	       t.planned_job_count, t.planned_bytes, t.dedupe_skipped_bytes,
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state IN ('succeeded','skipped')), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'failed'), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state IN ('pending','blocked','leased')), 0),
	       COALESCE((SELECT SUM(j.bytes_transferred) FROM jobs j WHERE j.transfer_id = t.id), 0),
	       COALESCE(t.failure_reason, ''), t.created_at, COALESCE(t.completed_at, '')
	  FROM transfers t
	  JOIN packages pk ON pk.id = t.package_id
	  JOIN products pr ON pr.id = pk.product_id
	  JOIN repositories src ON src.id = t.source_repo_id
	  JOIN repositories dst ON dst.id = t.target_repo_id`

func scanTransfer(row interface{ Scan(...any) error }) (TransferSummary, error) {
	var t TransferSummary
	err := row.Scan(&t.ID, &t.RequestID, &t.PackageID, &t.ProductName, &t.Tag,
		&t.Source, &t.Target, &t.State, &t.Priority, &t.CurrentWave, &t.MaxWave,
		&t.PlannedJobs, &t.PlannedBytes, &t.DedupeSkippedBytes,
		&t.JobsDone, &t.JobsFailed, &t.JobsOutstanding, &t.BytesTransferred,
		&t.FailureReason, &t.CreatedAt, &t.CompletedAt)
	return t, err
}

// ListTransfersFilter narrows a transfer listing.
type ListTransfersFilter struct {
	ProductName string
	State       string
	PackageID   int64
	Limit       int
	Offset      int
}

// ListTransfers returns transfers, newest first.
func (p *Packages) ListTransfers(ctx context.Context, f ListTransfersFilter) ([]TransferSummary, error) {
	query := transferSelect
	var args []any

	where := " WHERE 1=1"
	if f.ProductName != "" {
		where += " AND pr.name = ?"
		args = append(args, f.ProductName)
	}
	if f.State != "" {
		where += " AND t.state = ?"
		args = append(args, f.State)
	}
	if f.PackageID > 0 {
		where += " AND t.package_id = ?"
		args = append(args, f.PackageID)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	query += where + " ORDER BY t.created_at DESC, t.id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, f.Offset)

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TransferSummary
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan transfer: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTransfer returns one transfer.
func (p *Packages) GetTransfer(ctx context.Context, ref string) (TransferSummary, error) {
	id, err := p.ResolveTransferID(ctx, ref)
	if err != nil {
		return TransferSummary{}, err
	}

	row := p.db.QueryRowContext(ctx, p.dialect.Rewrite(transferSelect+" WHERE t.id = ?"), id)

	t, scanErr := scanTransfer(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return TransferSummary{}, fmt.Errorf("transfer %s was not found", ref)
	}
	if scanErr != nil {
		return TransferSummary{}, fmt.Errorf("read transfer %s: %w", ref, scanErr)
	}
	return t, nil
}

// ResolveTransferID accepts a full transfer ID or an unambiguous PREFIX.
//
// Transfers are identified by UUID, and every listing shortens them to the
// first segment because a column of full UUIDs is unreadable. A `describe`
// that then refused the very string `list` printed was a trap — the output
// said `transferctl transfers describe 4d882940` and that command answered
// NOT_FOUND.
//
// Prefixes resolve the way a short commit hash does: exact match first, then a
// unique prefix, and an ambiguous one is an error naming the candidates rather
// than a guess.
func (p *Packages) ResolveTransferID(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("a transfer ID is required")
	}

	var exact string
	err := p.db.QueryRowContext(ctx,
		p.dialect.Rewrite(`SELECT id FROM transfers WHERE id = ?`), ref).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve transfer %s: %w", ref, err)
	}

	// LIKE with an escaped pattern: a UUID contains none of the wildcards, but
	// the input is user-supplied and building a pattern from it unescaped is
	// how a listing turns into a scan.
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(
		`SELECT id FROM transfers WHERE id LIKE ? ESCAPE '\' ORDER BY created_at DESC LIMIT 10`),
		escapeLike(ref)+"%")
	if err != nil {
		return "", fmt.Errorf("resolve transfer %s: %w", ref, err)
	}
	defer func() { _ = rows.Close() }()

	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("scan transfer ID: %w", err)
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("transfer %s was not found", ref)
	default:
		return "", fmt.Errorf("%s matches %d transfers: %s\nuse more of the ID",
			ref, len(matches), strings.Join(matches, ", "))
	}
}

// escapeLike neutralises the wildcards in a user-supplied LIKE pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}

// JobSummary is one job as an operator sees it.
type JobSummary struct {
	ID               int64
	Kind             string
	Digest           string
	SizeBytes        int64
	State            string
	SkipReason       string
	Wave             int
	Attempts         int
	MaxAttempts      int
	BytesTransferred int64
	LeaseOwner       string
	LastError        string
	LastErrorClass   string
}

// ListJobs returns a transfer's jobs — layer-level progress.
//
// Ordered by wave then size, largest first within a wave: the big blobs are
// what a person watching a stalled transfer wants at the top, and they are
// also what the remaining time actually depends on.
func (p *Packages) ListJobs(ctx context.Context, ref string, limit int) ([]JobSummary, error) {
	if limit <= 0 {
		limit = 500
	}

	transferID, err := p.ResolveTransferID(ctx, ref)
	if err != nil {
		return nil, err
	}

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT id, kind, digest, size_bytes, state, COALESCE(skip_reason,''),
		       wave, attempts, max_attempts, bytes_transferred,
		       COALESCE(lease_owner,''), COALESCE(last_error,''), COALESCE(last_error_class,'')
		  FROM jobs
		 WHERE transfer_id = ?
		 ORDER BY wave, size_bytes DESC, id
		 LIMIT ?`), transferID, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs of transfer %s: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []JobSummary
	for rows.Next() {
		var j JobSummary
		if err := rows.Scan(&j.ID, &j.Kind, &j.Digest, &j.SizeBytes, &j.State,
			&j.SkipReason, &j.Wave, &j.Attempts, &j.MaxAttempts, &j.BytesTransferred,
			&j.LeaseOwner, &j.LastError, &j.LastErrorClass); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// tagsJSON encodes a job's destination tags.
//
// NULL rather than `[]` for the empty case, so "this artifact carries no tag"
// and "somebody wrote an empty list" stay distinguishable in a raw query — and
// so the common case, a blob, costs two bytes rather than a JSON document.
func tagsJSON(tags []string) any {
	if len(tags) == 0 {
		return nil
	}
	raw, err := json.Marshal(tags)
	if err != nil {
		return nil
	}
	return string(raw)
}

// decodeTags reads them back, tolerating absence.
func decodeTags(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// PendingTransfer is a transfer waiting to be planned.
//
// Transfers are created when a request is MADE, one per destination, and
// planned later. That split is what lets a request record its own intent: the
// destinations a rule named, or a person named, are rows rather than something
// re-derived from configuration at expansion time — which silently promoted
// "replicate to lab" into "replicate to every enabled target", and left
// promotion inexpressible because its destination is not "all targets" at all.
type PendingTransfer struct {
	ID           string
	RequestID    string
	ProductID    int64
	ProductName  string
	PackageID    int64
	Operation    string
	SourceRepoID int64
	TargetRepoID int64
	Priority     int
}

// PendingTransfers returns transfers that have not been planned.
//
// `planning` is included alongside `pending`: a Coordinator that died mid-plan
// left one there, and planning is idempotent by unique constraint, so the
// re-plan costs nothing for the jobs that already exist.
func (p *Packages) PendingTransfers(ctx context.Context, limit int) ([]PendingTransfer, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT t.id, t.request_id, pr.id, pr.name, t.package_id, r.operation,
		       t.source_repo_id, t.target_repo_id, t.priority
		  FROM transfers t
		  JOIN transfer_requests r ON r.id = t.request_id
		  JOIN products pr ON pr.id = r.product_id
		 WHERE t.state IN ('pending','planning')
		 ORDER BY t.priority DESC, t.created_at, t.id
		 LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PendingTransfer
	for rows.Next() {
		var t PendingTransfer
		if err := rows.Scan(&t.ID, &t.RequestID, &t.ProductID, &t.ProductName,
			&t.PackageID, &t.Operation, &t.SourceRepoID, &t.TargetRepoID,
			&t.Priority); err != nil {
			return nil, fmt.Errorf("scan pending transfer: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SettleRequest marks a request expanded once every transfer it opened has
// been planned.
func (p *Packages) SettleRequest(ctx context.Context, requestID string) error {
	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE transfer_requests
		   SET state = 'expanded', updated_at = `+p.dialect.Now()+`
		 WHERE id = ? AND state = 'pending'
		   AND NOT EXISTS (
		         SELECT 1 FROM transfers t
		          WHERE t.request_id = transfer_requests.id
		            AND t.state IN ('pending','planning'))`),
		requestID); err != nil {
		return fmt.Errorf("settle request %s: %w", requestID, err)
	}
	return nil
}
