package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
// here, against the dequeue - "hand-tuned, correctness-critical, and
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
	// Priority and SiteRank travel with the job so a lease batch can be handed
	// to the worker in the order it was selected in. See sortForDispatch.
	Priority int
	SiteRank int

	// RepairLevel is how much of the fast-path ladder this job may not use,
	// escalated by RepairMissingBlobs when the destination has been caught
	// claiming to hold content it will not serve. Zero is ordinary.
	RepairLevel int

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
// shape of this function does not change when it lands - an extra clause in
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

	var (
		leased []LeasedJob
		err    error
	)
	if p.dialect.Name() == DriverPostgres {
		leased, err = p.leasePostgres(ctx, req)
	} else {
		leased, err = p.leaseSQLite(ctx, req)
	}
	if err != nil {
		return nil, err
	}

	// Handing work out is what makes a transfer RUNNING. Failing to say so was
	// worth a write: this used to happen only when the first job COMPLETED, on
	// the argument that keeping the dequeue free of a write to a second table
	// was worth more. It is not. A transfer with ten blobs in flight and none
	// finished reported `ready`, and everything gated on the state word went
	// with it - the ETA column blanked while the speed column showed a
	// throughput, which is a table disagreeing with itself. Multi-gigabyte
	// blobs make that window long, and a resumed transfer starts inside it.
	//
	// The write is guarded on `state = 'ready'`, so it is an indexed no-op for
	// every lease after the first.
	if err := p.startTransfers(ctx, leased); err != nil {
		return nil, err
	}
	return leased, nil
}

// startTransfers moves the transfers behind a lease batch to `running`.
//
// started_at is set at the same moment and only if absent, because it anchors
// elapsed time and throughput: a transfer that waited an hour for a worker did
// not spend an hour transferring, and a RESUMED transfer must keep the start it
// already had rather than restarting the clock.
func (p *Packages) startTransfers(ctx context.Context, leased []LeasedJob) error {
	if len(leased) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(leased))
	ids := make([]any, 0, len(leased))
	var placeholders strings.Builder
	for _, j := range leased {
		if j.TransferID == "" || seen[j.TransferID] {
			continue
		}
		seen[j.TransferID] = true
		if placeholders.Len() > 0 {
			placeholders.WriteByte(',')
		}
		placeholders.WriteByte('?')
		ids = append(ids, j.TransferID)
	}
	if len(ids) == 0 {
		return nil
	}

	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers
		    SET state      = 'running',
		        started_at = COALESCE(started_at, `+p.dialect.Now()+`),
		        updated_at = `+p.dialect.Now()+`
		  WHERE id IN (`+placeholders.String()+`) AND state = 'ready'`), ids...)
	if err != nil {
		return fmt.Errorf("start transfers for a lease batch: %w", err)
	}
	return nil
}

// leaseColumns is the RETURNING/SELECT list both dialects share, so the two
// implementations cannot drift in what they produce.
const leaseColumns = `id, transfer_id, kind, digest, size_bytes, media_type,
	artifact_id, source_repo_id, target_repo_id, attempts, wave, priority,
	site_rank, repair_level, target_tags, target_repository`

// The lease CLEARS the previous attempt's error.
//
// An error describes the attempt that produced it. Leaving it on a job that is
// now running again means a listing shows a live job labelled with a failure
// that has already been superseded - which is how a fixed problem goes on
// looking broken. It is kept while the job waits out its backoff, because
// there the reason it is waiting is exactly what a reader wants.

// # The dequeue order, and why it has three keys rather than one
//
//	priority DESC     what an operator asked for, first
//	kind DESC         manifests before blobs - one round trip, and it unblocks
//	                  the index above it ('manifest' > 'blob', see 00014)
//	site_rank         the copy that keeps a bundle resolvable, before the copy
//	                  published under a component's own name
//	size_bytes DESC   largest first WITHIN a rank
//
// The middle key is the interesting one, and it exists because ordering by size
// alone was tried, measured and found to cost far more than it saved.
//
// A bundle publishes its components TWICE - once inside the bundle, once under
// the component's own name - so one digest becomes two jobs of IDENTICAL size.
// The second is nearly free: the blob is already in a sibling repository of the
// same registry, so it is a cross-repository MOUNT rather than a transfer over
// the WAN. But only if it runs AFTER the first. Sorting by size alone makes the
// two adjacent, so both land in one lease batch and both stream from the
// vendor. Measured on the bundle fixture: 16 vendor GETs for 11 distinct blobs,
// and no mounts at all.
//
// site_rank states what insertion order previously left to chance. Every rank-0
// job is dequeued before any rank-1 job, so by the time the second copy is
// leased the first has either completed - leaving a placement to mount from -
// or is still in flight, in which case the duplicate suppression below defers
// it. Neither path streams the same digest twice.
//
// With that guaranteed, largest-first is safe within a rank, and it is worth
// having: insertion order is arbitrary with respect to size, so a
// multi-gigabyte layer leased last runs alone while every other slot idles.
// See docs/design/04 §13.
const leaseOrder = ` ORDER BY priority DESC, kind DESC, site_rank, size_bytes DESC, id`

// leaseCandidatePredicate is the leasability test, shared by both dialects.
//
// The NOT EXISTS is concurrent duplicate suppression (docs/design/04 §5): if
// another worker is already moving this blob to this REGISTRY, skip it. The
// skipped job stays pending and is picked up moments later - by which time the
// first has completed and written a placement, so the second takes a fast path
// and moves zero bytes.
//
// # Why this is per registry rather than per repository
//
// It was per repository, and that made the check almost useless for a bundle.
// A component published under two names has one digest, two destination
// repositories and therefore two jobs - created consecutively, so they have
// adjacent ids and land in the SAME lease batch. Both streamed the blob from
// the vendor at the same time, over the WAN, for content the registry could
// have relocated internally in one request.
//
// Widening it to the registry is what makes the second job cheap: it runs after
// the first, by which time a placement exists on a sibling repository and the
// worker can MOUNT rather than stream. The order the two run in does not
// matter, only that they do not run at once.
//
// The join is against `repositories`, which the note at the top of this file
// warns about - the dequeue is meant to stay join-free. It is inside a NOT
// EXISTS over `jobs_inflight_blob_idx`, so it runs only for the handful of
// digests actually in flight, not for every candidate row.
//
// # Why site_rank needs a second clause, and ordering was not enough
//
// Ordering the dequeue by site_rank puts every rank-0 job before every rank-1
// job, which was supposed to guarantee that the second copy of a component
// mounts from the first. It guarantees it only when the two land in DIFFERENT
// lease batches. Within one batch neither is leased yet, so the suppression
// above does not fire, both are selected, both are hydrated at the same moment
// - and the mount candidate is resolved from placements that the rank-0 job has
// not written yet. Both stream.
//
// Measured on the bundle fixture with a batch of eight: some components mounted
// and some did not, entirely according to where the batch boundary fell. That is
// not a property anybody can reason about, and no total shows it - a blob
// uploaded twice to two repositories looks exactly like one uploaded once and
// mounted once, in bytes, in state and in the result.
//
// So a rank-1 job is not leasable while its rank-0 sibling is still OUTSTANDING,
// rather than merely while it is leased. The second clause says that, and is
// gated on `site_rank = 0` first so it costs nothing for the majority of jobs,
// which are rank 0 and skip it entirely.
//
// # A PAUSED sibling is not an earlier one
//
// The clause matches across transfers, deliberately: two transfers of the same
// product line share most of their digests, and the mount is worth having
// between them as much as within one. But "wait for the earlier copy" is only
// sound while the earlier copy is going to run. A paused job is not, and the
// wait becomes permanent - a second download sitting in `ready`, never leasing
// a job and therefore never even reaching `running`, because a transfer
// somebody paused holds a rank-0 job for the same digest. It cannot resolve on
// its own: nothing about the paused transfer changes until a person resumes it.
//
// So paused jobs are excluded. The mount they would have offered is lost, which
// costs bandwidth once; waiting for them costs the whole transfer, forever.
func (p *Packages) leaseCandidatePredicate() string {
	return `state = 'pending'
	   AND NOT paused
	   AND next_visible_at <= ` + p.dialect.Now() + `
	   AND NOT EXISTS (
	         SELECT 1 FROM jobs inflight
	           JOIN repositories ir ON ir.id = inflight.target_repo_id
	           JOIN repositories jr ON jr.id = jobs.target_repo_id
	          WHERE inflight.state = 'leased'
	            AND inflight.digest = jobs.digest
	            AND ir.registry_host = jr.registry_host
	            AND ir.product_id = jr.product_id
	       )
	   AND (jobs.site_rank = 0 OR NOT EXISTS (
	         SELECT 1 FROM jobs earlier
	           JOIN repositories er ON er.id = earlier.target_repo_id
	           JOIN repositories jr2 ON jr2.id = jobs.target_repo_id
	          WHERE earlier.digest = jobs.digest
	            AND earlier.site_rank < jobs.site_rank
	            AND earlier.state IN ('pending','blocked','leased')
	            AND NOT earlier.paused
	            AND er.registry_host = jr2.registry_host
	            AND er.product_id = jr2.product_id
	       ))`
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
		     WHERE ` + p.leaseCandidatePredicate() + leaseOrder + `
		       FOR UPDATE SKIP LOCKED
		     LIMIT ?
		)
		UPDATE jobs j
		   SET state            = 'leased',
		       lease_owner      = ?,
		       lease_expires_at = ` + p.dialect.TimeAhead("?") + `,
		       attempts         = attempts + 1,
		       last_error       = NULL,
		       last_error_class = NULL,
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
		       last_error       = NULL,
		       last_error_class = NULL,
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
		 WHERE `+p.leaseCandidatePredicate()+leaseOrder+`
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
			&j.Attempt, &j.Wave, &j.Priority, &j.SiteRank, &j.RepairLevel, &tags,
			&targetRepository); err != nil {
			return nil, fmt.Errorf("scan leased job: %w", err)
		}
		j.MediaType = mediaType.String
		j.TargetTags = decodeTags(tags)
		j.TargetRepository = targetRepository.String
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sortForDispatch(out)
	return out, nil
}

// sortForDispatch puts a lease batch back into the order it was SELECTED in.
//
// Neither dialect returns it that way - Postgres RETURNING is unordered, and
// the SQLite path reads its rows back by id - and the order is not cosmetic: a
// worker dispatches the batch in order against a bounded semaphore, so the last
// job in the slice is the last to start. Selecting largest-first and then
// handing the worker the batch sorted by id throws away the entire point of the
// ordering, which is what the dequeue test caught.
//
// Same keys as the SQL, so there is one order and not two.
func sortForDispatch(jobs []LeasedJob) {
	sort.SliceStable(jobs, func(i, k int) bool {
		a, b := jobs[i], jobs[k]
		switch {
		case a.Priority != b.Priority:
			return a.Priority > b.Priority
		case a.Kind != b.Kind:
			// Same rule as the SQL, and the same dependence on the spelling:
			// 'manifest' > 'blob', so descending puts manifests first.
			return a.Kind > b.Kind
		case a.SiteRank != b.SiteRank:
			return a.SiteRank < b.SiteRank
		case a.SizeBytes != b.SizeBytes:
			return a.SizeBytes > b.SizeBytes
		default:
			return a.ID < b.ID
		}
	})
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
	// Name is the configured repository name - the `sources[].name` or
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
// joins live here - one extra round trip per lease BATCH, amortized across up
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
// The tag is applied LAST, only after the index manifest is committed -
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
) (renewed []int64, cancelled []int64, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	if d <= 0 {
		d = 2 * time.Minute
	}

	// WHICH OF THESE BELONG TO A TRANSFER SOMEBODY STOPPED.
	//
	// This is how a stop reaches a worker. There is no push channel: the
	// heartbeat is the only regular call from a worker holding a long blob, so
	// cancellation rides it, and the worker aborts within one interval.
	//
	// Renewing them instead - which is what happened before this existed - made
	// `stop` mean "stop when the current blob finishes", and a forty-gigabyte
	// blob makes that an hour. The transfer sat in `cancelling` the whole time
	// with bytes still moving into it, which is the one thing the operator had
	// just asked it not to do.
	cancelled, err = p.leasesOfStoppedTransfers(ctx, owner, ids)
	if err != nil {
		return nil, nil, err
	}
	stopped := make(map[int64]bool, len(cancelled))
	for _, id := range cancelled {
		stopped[id] = true
	}
	live := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !stopped[id] {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		return nil, cancelled, nil
	}
	ids = live

	placeholders, idArgs := inClause(ids)
	args := append([]any{d.Seconds(), owner}, idArgs...)

	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET lease_expires_at = `+p.dialect.TimeAhead("?")+`,
		       updated_at       = `+p.dialect.Now()+`
		 WHERE lease_owner = ? AND state = 'leased' AND id IN (`+placeholders+`)`),
		args...); err != nil {
		return nil, nil, fmt.Errorf("renew leases for %s: %w", owner, err)
	}

	// Read back rather than trusting RowsAffected: the worker needs to know
	// WHICH leases it still holds, not how many.
	selectPlaceholders, selectIDs := inClause(ids)
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(
		`SELECT id FROM jobs
		  WHERE lease_owner = ? AND state = 'leased' AND id IN (`+selectPlaceholders+`)`),
		append([]any{owner}, selectIDs...)...)
	if err != nil {
		return nil, nil, fmt.Errorf("read renewed leases for %s: %w", owner, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("scan renewed lease: %w", err)
		}
		renewed = append(renewed, id)
	}
	return renewed, cancelled, rows.Err()
}

// leasesOfStoppedTransfers picks out the jobs this worker holds whose transfer
// is no longer going anywhere.
//
// `cancelling` AND `cancelled`, because the two are one situation seen at
// different moments: the second is what the first becomes when the last lease
// reports, and a worker that missed a heartbeat can be holding a job of either.
func (p *Packages) leasesOfStoppedTransfers(
	ctx context.Context, owner string, ids []int64,
) ([]int64, error) {
	placeholders, args := inClause(ids)
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT j.id
		  FROM jobs j
		  JOIN transfers t ON t.id = j.transfer_id
		 WHERE j.lease_owner = ?
		   AND j.state = 'leased'
		   AND t.state IN ('cancelling','cancelled')
		   AND j.id IN (`+placeholders+`)`),
		append([]any{owner}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("find stopped work held by %s: %w", owner, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stopped work: %w", err)
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
	// when Outcome is skipped - the CHECK constraint enforces the vocabulary,
	// and the distinction is what makes a dedupe regression visible.
	SkipReason string
	ErrorClass string
	ErrorMsg   string
	// Placed records that the destination now holds this blob - whether the
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
	// MaxAttempts caps the retries for THIS failure's error class, from
	// docs/design/11 §2.3 - a digest mismatch is worth two attempts where a
	// transient 5xx is worth eight.
	//
	// Zero means "whatever the row says", which is the column default. The cap
	// only ever lowers it: a class-specific budget is a reason to stop sooner,
	// never a licence to exceed what the row allows.
	MaxAttempts int
}

// CompletionResult reports what the completion did beyond the job itself.
type CompletionResult struct {
	// Applied is false when the job was not this worker's to complete -
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
	// Promoted is how many jobs this completion made runnable by satisfying
	// their last dependency. Reported because a transfer that never promotes
	// anything has a dependency graph that is wrong, and nothing else would
	// show it.
	Promoted int
	// Repaired is what a rejected manifest push cost the placement cache: the
	// records withdrawn and the blob jobs sent back for a forced upload.
	Repaired RepairResult
}

// ClassBlobUnknown is the error class the engine reports when a destination
// rejects a manifest for content it says it does not have.
//
// Declared here as well as in the engine because this is the side that ACTS on
// it, and a string compared in two packages against two separate literals is a
// string that eventually differs in one of them.
const ClassBlobUnknown = "blob_unknown"

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
		rowMax       int
	)
	err = tx.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT transfer_id, kind, digest, size_bytes, target_repo_id, wave, lease_owner,
		       state, max_attempts
		  FROM jobs WHERE id = ?`), c.JobID).
		Scan(&transferID, &kind, &digest, &size, &targetRepoID, &wave, &owner, &state, &rowMax)
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

	// The effective cap is the LOWER of the row's budget and the class's.
	// Computed here rather than in SQL because the two dialects spell the
	// minimum of two scalars differently, and one Go comparison is clearer
	// than a dialect method for it.
	effectiveMax := rowMax
	if c.MaxAttempts > 0 && c.MaxAttempts < effectiveMax {
		effectiveMax = c.MaxAttempts
	}

	if err := p.applyJobOutcome(ctx, tx, c, effectiveMax); err != nil {
		return res, err
	}

	// A blob that reached the destination - by transfer, by mount, or by being
	// found already there - is a placement. Recording it is what makes the
	// NEXT transfer of a product line nearly free.
	if c.Placed && kind == "blob" {
		source := placementSource(c)
		if err := p.RecordPlacement(ctx, tx, targetRepoID, digest, size, source); err != nil {
			return res, err
		}
	}

	// The destination has told us a blob we recorded as present is not. That is
	// not a failure to retry - retrying asks the same destination the same
	// question and gets the same wrong answer - it is a cache to repair. See
	// RepairMissingBlobs, which is the backstop docs/design/11 §2.5 promised.
	if c.Outcome == "failed" && c.ErrorClass == ClassBlobUnknown && kind == "manifest" {
		repair, err := p.RepairMissingBlobs(ctx, tx, c.JobID)
		if err != nil {
			return res, err
		}
		res.Repaired = repair
	}

	// Per-artifact readiness. Anything that was waiting on THIS job and is now
	// fully satisfied becomes runnable in the same transaction that made it
	// true - so a manifest whose last blob just landed is leasable before the
	// worker reporting that blob has finished its round trip, rather than at
	// the end of the wave.
	if c.Outcome == "succeeded" || c.Outcome == "skipped" {
		promoted, err := p.PromoteDependents(ctx, tx, c.JobID)
		if err != nil {
			return res, err
		}
		res.Promoted = promoted
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
// returns to `pending` behind a backoff, and - crucially - keeps
// bytes_transferred, so a retry resumes the accounting rather than restarting
// it (docs/design/04 §11).
func (p *Packages) applyJobOutcome(
	ctx context.Context, tx *sql.Tx, c Completion, maxAttempts int,
) error {
	if c.Outcome != "failed" {
		skip := nullIfEmpty(c.SkipReason)
		// A job that succeeded carries no error, whatever earlier attempts
		// said. Leaving the last failure on a succeeded row makes a listing
		// read as though it had failed.
		_, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			UPDATE jobs
			   SET state             = ?,
			       skip_reason       = ?,
			       bytes_transferred = ?,
			       last_error        = NULL,
			       last_error_class  = NULL,
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
		   SET state = CASE WHEN attempts >= ? THEN 'failed' ELSE 'pending' END,
		       last_error        = ?,
		       last_error_class  = ?,
		       bytes_transferred = ?,
		       lease_owner       = NULL,
		       lease_expires_at  = NULL,
		       next_visible_at   = `+p.dialect.TimeAhead("?")+`,
		       completed_at      = CASE WHEN attempts >= ?
		                                THEN `+p.dialect.Now()+` ELSE NULL END,
		       updated_at        = `+p.dialect.Now()+`
		 WHERE id = ?`),
		maxAttempts, nullIfEmpty(c.ErrorMsg), nullIfEmpty(c.ErrorClass), c.BytesTransferred,
		retryDelay(c.Attempt).Seconds(), maxAttempts, c.JobID)
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

	// A transfer somebody has stopped is not a transfer to settle, advance or
	// declare successful - it is one waiting for its last lease to report.
	//
	// This has to be checked BEFORE anything below it. `stop` cancels every job
	// not yet started, so the waves genuinely drain, and without this guard the
	// walk would run to the end and mark a cancelled transfer `succeeded` on
	// the strength of the work it deliberately did not do.
	if state == "cancelling" {
		state, err = p.closeCancellation(ctx, tx, transferID)
		return false, currentWave, state, err
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
		// A job that failed permanently takes its dependents with it. Without
		// this, per-artifact readiness would leave the transfer holding a few
		// blocked manifests that can never run - which reads as "still
		// working" to the stall check below, forever. See FailUnreachableJobs.
		if _, err := p.FailUnreachableJobs(ctx, tx, transferID); err != nil {
			return false, currentWave, state, err
		}

		// A wave that will not drain because a job has EXHAUSTED its attempts
		// is not a wave still working - it is a transfer that has stopped, and
		// until this check existed it went on reporting `running` with nothing
		// in flight for as long as anybody left it there. See
		// SettleStalledTransfers, which is the same question asked
		// periodically for the failures that arrive without a completion.
		state, err = p.settleIfStalled(ctx, tx, transferID, state)
		return false, currentWave, state, err
	}

	// Advance THROUGH empty waves rather than one step per completion. A wave
	// with no jobs of its own has nothing to complete and so nothing to drive
	// the next advance - stopping at one would stall the transfer on any gap
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

	// Past the top wave. Everything is accounted for - but "accounted for" is
	// not "succeeded", and the difference has to be checked HERE rather than
	// inferred from the walk above.
	//
	// The walk cannot answer it. `waveDrained` is asked about ONE wave, the one
	// the completing job belongs to, and `waveOccupied` counts only work that
	// can still move; a wave whose remaining jobs have exhausted their attempts
	// is empty by both measures. Per-artifact readiness makes that ordinary
	// rather than exotic: a manifest runs as soon as its own content lands, so
	// by the time the wave it nominally belongs to formally drains, the waves
	// above it are frequently already terminal - failures included. The loop
	// then walks past them into this branch and the transfer was declared
	// `succeeded` with failed jobs sitting under it.
	//
	// Which is not merely a wrong word on a listing. `succeeded` is settled, and
	// RetryTransfer refuses a settled transfer, so the three failures could not
	// be retried and could not be explained: 100% done, FAILED 3, and
	// "there is nothing to retry".
	state, _, err = p.finishTransfer(ctx, tx, transferID)
	return false, currentWave, state, err
}

// closeCancellation moves a stopping transfer to `cancelled` once the last
// lease has reported.
//
// `cancelling` is a real state rather than a flag because a leased job belongs
// to a worker and stops at that worker's next checkpoint, not the instant
// somebody types the command. Naming the window makes it observable; closing it
// here is what stops the window being permanent.
func (p *Packages) closeCancellation(
	ctx context.Context, tx *sql.Tx, transferID string,
) (state string, err error) {
	var inFlight int
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'leased'`),
		transferID).Scan(&inFlight); err != nil {
		return "cancelling", fmt.Errorf("count leased jobs of transfer %s: %w", transferID, err)
	}
	if inFlight > 0 {
		return "cancelling", nil
	}

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET state = 'cancelled', completed_at = `+p.dialect.Now()+
			`, updated_at = `+p.dialect.Now()+` WHERE id = ?`), transferID); err != nil {
		return "cancelling", fmt.Errorf("cancel transfer %s: %w", transferID, err)
	}
	return "cancelled", nil
}

// finishTransfer settles a transfer whose jobs have all stopped, reporting
// which way it went.
//
// One count decides it, and it is a count over the WHOLE transfer rather than
// any wave: a transfer is successful when nothing failed, and any other rule is
// a rule about where the failure happened, which no consumer of this cares
// about.
func (p *Packages) finishTransfer(
	ctx context.Context, tx *sql.Tx, transferID string,
) (state string, failed int, err error) {
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'failed'`),
		transferID).Scan(&failed); err != nil {
		return "", 0, fmt.Errorf("count failed jobs of transfer %s: %w", transferID, err)
	}

	if failed == 0 {
		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
			`UPDATE transfers SET state = 'succeeded', failure_reason = NULL,
			        completed_at = `+p.dialect.Now()+`, updated_at = `+p.dialect.Now()+`
			  WHERE id = ?`), transferID); err != nil {
			return "", 0, fmt.Errorf("finish transfer %s: %w", transferID, err)
		}
		return "succeeded", 0, nil
	}

	reason, err := p.failureReason(ctx, tx, transferID, failed)
	if err != nil {
		return "", failed, err
	}
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET state = 'failed', failure_reason = ?,
		        completed_at = `+p.dialect.Now()+`, updated_at = `+p.dialect.Now()+`
		  WHERE id = ?`), reason, transferID); err != nil {
		return "", failed, fmt.Errorf("fail transfer %s: %w", transferID, err)
	}
	return "failed", failed, nil
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
// "scaled down" - the only signal is a timestamp that stopped advancing.
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

	// The transfer's state comes back with the job, because what an expired
	// lease MEANS depends on it. On a live transfer the work is still wanted
	// and goes back to the queue; on one somebody stopped it is not, and
	// requeueing it undoes the stop by timeout - the job runs again, the
	// transfer never empties, and `cancelling` becomes permanent.
	rows, err := tx.QueryContext(ctx, p.dialect.Rewrite(
		`SELECT j.id, j.transfer_id, j.attempts, j.max_attempts, t.state
		   FROM jobs j
		   JOIN transfers t ON t.id = j.transfer_id
		  WHERE j.state = 'leased' AND j.lease_expires_at < `+p.dialect.Now()))
	if err != nil {
		return nil, fmt.Errorf("find expired leases: %w", err)
	}

	type expired struct {
		id                    int64
		transferID            string
		attempts, maxAttempts int
		transferState         string
	}
	var found []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.transferID, &e.attempts, &e.maxAttempts,
			&e.transferState); err != nil {
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
		switch {
		case e.transferState == "cancelling" || e.transferState == "cancelled":
			state = "cancelled"
		case e.attempts >= e.maxAttempts:
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
			       completed_at      = CASE WHEN ? IN ('failed','cancelled') THEN `+p.dialect.Now()+` ELSE NULL END,
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
// exists without saying which - and on the retry path, which is the normal
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
	PackageName string
	// Strategy is HOW this transfer was performed: `copy`, `mirror` or
	// `proxy`. Recorded on the row rather than derived from configuration, so
	// a year-old record still says how the content got there even after the
	// target has been reconfigured - and so that a settled transfer with no
	// jobs and no bytes is distinguishable from one that failed to plan.
	Strategy string
	Tag      string
	// DisplayTag is Tag with the vendor's structural noise removed - `25.7.2131`
	// for NEAR's `orb_25.7.2131`. Empty where no shortening applies, which is
	// every source that declares no `vendor`. Cosmetic: Tag is the identity.
	DisplayTag string
	// ContentBytes is the size of the RELEASE: every distinct digest in it,
	// counted once.
	//
	// Deliberately not the same quantity as PlannedBytes, and the difference is
	// not a discrepancy. A component of a bundle is published under its own
	// name as well as inside the bundle, and a registry stores blobs PER
	// REPOSITORY - so one blob landing in two repositories is two placements
	// and two jobs, and its bytes are counted twice in the work. A base layer
	// shared by fifty components is counted fifty times.
	//
	// Both numbers are true about different things: this is what the release
	// weighs, PlannedBytes is what the transfer has to do. Reporting only the
	// second made a 29.8 GiB orb read as a 63.7 GiB one.
	//
	// Zero where the package's size was never established - a package listed
	// but not fully walked has no honest total.
	ContentBytes int64
	Source       string
	Target       string
	// SourceName and TargetName are the CONFIGURED names - the `sources[].name`
	// and `targets[].name` an operator types into --from and --to. Source and
	// Target above are the resolved host and path, which is what a person needs
	// when they are looking at one transfer and far too wide when they are
	// scanning a page of them.
	SourceName string
	TargetName string
	State      string
	Priority   int

	CurrentWave int
	MaxWave     int

	PlannedJobs  int
	PlannedBytes int64
	// DedupeSkippedBytes was never queued: planning already knew the
	// destination held it. SkippedBytes was queued and then not sent, because
	// the worker found it there or the registry relocated it internally.
	//
	// Both are savings and they are counted separately because they are earned
	// at different times, but a caller asking "what did this transfer save" wants
	// the sum - and reporting only the first is how a transfer that skipped
	// 32 GiB came to report `SAVED 0 B`. On a clean database nothing is
	// deduplicated at planning time by definition: every saving is discovered by
	// the worker, so the whole of it landed in the line nobody was reading.
	DedupeSkippedBytes int64
	SkippedBytes       int64

	// Rolled up from jobs.
	JobsDone   int
	JobsFailed int
	// JobsBlocked are gated behind a later wave and cannot be leased yet. The
	// number that explains an idle-looking fleet: a transfer with five hundred
	// outstanding jobs and one running is usually four hundred and ninety-nine
	// manifests waiting for the last blob, not a worker that is stuck.
	JobsBlocked int
	// JobsRepaired have been sent back because the destination denied holding
	// content it had reported. This is the ONLY thing that makes a done count
	// go DOWN, and a reader watching one drop with no explanation on the page
	// concludes the tool is broken - which is exactly what happened.
	JobsRepaired int
	// OutstandingBytes is what is actually LEFT to move: the size of every job
	// still to run, less what each has already sent.
	//
	// Not planned minus transferred. That difference includes every byte that
	// will never move - content the destination already had, blobs relocated
	// internally, work deduplicated away - so on a transfer that skipped a
	// hundred megabytes it reports a hundred megabytes of phantom work, and any
	// estimate built on it is wrong by exactly that much.
	OutstandingBytes int64
	// QuietestInFlight is when the least recently active in-flight job last
	// moved: its last progress report, or the moment it was leased.
	//
	// "1 job in flight" says nothing about whether that job is transferring or
	// hung, and those need opposite responses. A worker holding a job that has
	// been silent for hours is the one shape the lease machinery cannot see -
	// the lease is renewed by the worker being alive, not by the job moving.
	QuietestInFlight string
	JobsOutstanding  int
	BytesTransferred int64
	// JobsInFlight is how many are leased RIGHT NOW, and Workers is how many
	// distinct workers hold them. Without these, concurrency is invisible: a
	// page of jobs ordered by size shows whichever happen to be at the top,
	// and an operator cannot tell sixteen-way parallelism from one-at-a-time.
	JobsInFlight int
	Workers      int
	// JobsWaiting is how many are in retry backoff rather than runnable. It is
	// the difference between "the queue is saturated" and "most of this is
	// sitting out a backoff", which look identical from a progress count.
	JobsWaiting int

	FailureReason string
	CreatedAt     string
	// StartedAt is when the first job was leased, not when the transfer was
	// asked for. Elapsed and throughput are both meaningless measured from the
	// request: a transfer that waited an hour for a worker did not spend an
	// hour transferring.
	StartedAt   string
	CompletedAt string
}

// SavedBytes is everything this transfer did not have to move.
func (t TransferSummary) SavedBytes() int64 {
	return t.DedupeSkippedBytes + t.SkippedBytes
}

// transferSelect is the shared projection, so list and get cannot disagree
// about what a transfer looks like.
func (p *Packages) transferSelect() string {
	return `
	SELECT t.id, t.request_id, t.package_id, pr.name, src.repository_path, pk.tag,
	       COALESCE(pk.display_tag, ''), COALESCE(pk.total_bytes, 0),
	       src.registry_host || '/' || src.repository_path,
	       dst.registry_host || '/' || dst.repository_path,
	       src.name, dst.name,
	       t.state, t.priority, t.current_wave, t.max_wave,
	       t.planned_job_count, t.planned_bytes, t.dedupe_skipped_bytes,
	       COALESCE((SELECT SUM(j.size_bytes) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'skipped'), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state IN ('succeeded','skipped')), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'failed'), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state IN ('pending','blocked','leased')), 0),
	       COALESCE((SELECT SUM(j.bytes_transferred) FROM jobs j WHERE j.transfer_id = t.id), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'leased'), 0),
	       COALESCE((SELECT count(DISTINCT j.lease_owner) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'leased' AND j.lease_owner IS NOT NULL), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'pending' AND j.next_visible_at > ` + p.dialect.Now() + `), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.state = 'blocked'), 0),
	       COALESCE((SELECT count(*) FROM jobs j WHERE j.transfer_id = t.id
	                  AND j.repair_level > 0), 0),
	       COALESCE((SELECT SUM(j.size_bytes - j.bytes_transferred) FROM jobs j
	                  WHERE j.transfer_id = t.id
	                    AND j.state IN ('pending','blocked','leased')), 0),
	       COALESCE((SELECT MIN(j.updated_at) FROM jobs j
	                  WHERE j.transfer_id = t.id AND j.state = 'leased'), ''),
	       COALESCE(t.failure_reason, ''), t.created_at, COALESCE(t.started_at, ''),
	       COALESCE(t.completed_at, ''), t.strategy
	  FROM transfers t
	  JOIN packages pk ON pk.id = t.package_id
	  JOIN products pr ON pr.id = pk.product_id
	  JOIN repositories src ON src.id = t.source_repo_id
	  JOIN repositories dst ON dst.id = t.target_repo_id`
}

func scanTransfer(row interface{ Scan(...any) error }) (TransferSummary, error) {
	var t TransferSummary
	err := row.Scan(&t.ID, &t.RequestID, &t.PackageID, &t.ProductName, &t.PackageName, &t.Tag,
		&t.DisplayTag, &t.ContentBytes,
		&t.Source, &t.Target, &t.SourceName, &t.TargetName,
		&t.State, &t.Priority, &t.CurrentWave, &t.MaxWave,
		&t.PlannedJobs, &t.PlannedBytes, &t.DedupeSkippedBytes, &t.SkippedBytes,
		&t.JobsDone, &t.JobsFailed, &t.JobsOutstanding, &t.BytesTransferred,
		&t.JobsInFlight, &t.Workers, &t.JobsWaiting, &t.JobsBlocked, &t.JobsRepaired, &t.OutstandingBytes, &t.QuietestInFlight,
		&t.FailureReason, &t.CreatedAt, &t.StartedAt, &t.CompletedAt, &t.Strategy)
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
	query := p.transferSelect()
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

	row := p.db.QueryRowContext(ctx, p.dialect.Rewrite(p.transferSelect()+" WHERE t.id = ?"), id)

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
// that then refused the very string `list` printed was a trap - the output
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

	// Where this job reads from and writes to, as repository PATHS.
	SourceRepository string
	TargetRepository string
	// TargetTags are the names this manifest will answer to once pushed.
	TargetTags []string

	// The artifact this job belongs to - what makes a digest legible.
	//
	// A blob on its own says nothing: `sha256:8a34…` is not something anybody
	// can act on or recognise. The manifest that references it is, and the
	// vendor already named that manifest in an annotation, so the layer of a
	// Helm chart can say it is a layer of that chart.
	//
	// For a manifest job this is the artifact itself. For a blob it is a
	// manifest that references it - one of possibly several, because a base
	// layer shared by five images belongs to all of them.
	ParentDigest    string
	ParentMediaType string
	// ParentRef is the vendor's own name for the parent, from
	// org.opencontainers.image.ref.name - `orbs/CFX-5000-k8s/nginx:1.2.3`.
	// Empty when the vendor named nothing, which is normal for the platform
	// manifests under an index.
	ParentRef string
	// ParentShared reports that the parent is one of several referencing this
	// blob, so a reader knows the attribution is an example rather than the
	// whole truth.
	ParentShared bool
}

// jobActivityOrder sorts what is HAPPENING to the top.
//
// A transfer has thousands of jobs and a listing shows a page of them, so the
// order decides what an operator ever sees. Ordering by wave put whatever
// happened to be planned first at the top - usually blocked manifests - and
// buried the handful actually moving.
//
// Running first, then what is runnable, then what is waiting, then the
// outcomes. Within each, largest first: the big blobs are what a stalled
// transfer is usually stalled on, and what the remaining time depends on.
const jobActivityOrder = `CASE j.state
	WHEN 'leased'    THEN 0
	WHEN 'pending'   THEN 1
	WHEN 'failed'    THEN 2
	WHEN 'blocked'   THEN 3
	WHEN 'succeeded' THEN 4
	WHEN 'skipped'   THEN 5
	ELSE 6 END`

// ListJobs returns a transfer's jobs - layer-level progress.
func (p *Packages) ListJobs(
	ctx context.Context, ref string, state string, limit int,
) ([]JobSummary, error) {
	if limit <= 0 {
		limit = 500
	}

	transferID, err := p.ResolveTransferID(ctx, ref)
	if err != nil {
		return nil, err
	}

	where := " WHERE j.transfer_id = ?"
	args := []any{transferID}
	if state != "" {
		where += " AND j.state = ?"
		args = append(args, state)
	}
	args = append(args, limit)

	// The parent artifact is resolved IN THE QUERY rather than per row, because
	// a listing of five hundred jobs would otherwise be five hundred round
	// trips to turn digests into names.
	//
	// For a manifest job the artifact is on the job. For a blob it is found
	// through artifact_blobs, scoped to this transfer's package so a digest
	// shared with an unrelated package cannot attribute it to the wrong thing.
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT j.id, j.kind, j.digest, j.size_bytes, j.state, COALESCE(j.skip_reason,''),
		       j.wave, j.attempts, j.max_attempts, j.bytes_transferred,
		       COALESCE(j.lease_owner,''), COALESCE(j.last_error,''),
		       COALESCE(j.last_error_class,''),
		       COALESCE(src.repository_path,''),
		       COALESCE(j.target_repository, dst.repository_path, ''),
		       j.target_tags,
		       COALESCE(pa.digest,''), COALESCE(pa.media_type,''), pa.annotations,
		       COALESCE((SELECT count(*) FROM artifact_blobs ab2
		                  JOIN package_artifacts a2 ON a2.id = ab2.artifact_id
		                 WHERE ab2.digest = j.digest AND a2.package_id = t.package_id), 0)
		  FROM jobs j
		  JOIN transfers t ON t.id = j.transfer_id
		  LEFT JOIN repositories src ON src.id = j.source_repo_id
		  LEFT JOIN repositories dst ON dst.id = j.target_repo_id
		  LEFT JOIN package_artifacts pa ON pa.id = COALESCE(
		        j.artifact_id,
		        (SELECT ab.artifact_id
		           FROM artifact_blobs ab
		           JOIN package_artifacts a ON a.id = ab.artifact_id
		          WHERE ab.digest = j.digest AND a.package_id = t.package_id
		          ORDER BY a.depth, a.id
		          LIMIT 1))`+where+`
		 ORDER BY `+jobActivityOrder+`, j.size_bytes DESC, j.id
		 LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs of transfer %s: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []JobSummary
	for rows.Next() {
		var (
			j           JobSummary
			tags        []byte
			annotations []byte
			referencing int
		)
		if err := rows.Scan(&j.ID, &j.Kind, &j.Digest, &j.SizeBytes, &j.State,
			&j.SkipReason, &j.Wave, &j.Attempts, &j.MaxAttempts, &j.BytesTransferred,
			&j.LeaseOwner, &j.LastError, &j.LastErrorClass,
			&j.SourceRepository, &j.TargetRepository, &tags,
			&j.ParentDigest, &j.ParentMediaType, &annotations, &referencing); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		j.TargetTags = decodeTags(tags)
		j.ParentRef = refNameFrom(annotations)
		j.ParentShared = j.Kind == "blob" && referencing > 1
		out = append(out, j)
	}
	return out, rows.Err()
}

// refNameFrom pulls the vendor's own name out of an artifact's annotations.
//
// The same reserved key the planner derives destinations from, read here for
// display: it is what turns `sha256:8a34…` into "a layer of
// orbs/CFX-5000-k8s/nginx:1.2.3".
func refNameFrom(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var annotations map[string]string
	if err := json.Unmarshal(raw, &annotations); err != nil {
		return ""
	}
	return annotations["org.opencontainers.image.ref.name"]
}

// tagsJSON encodes a job's destination tags.
//
// NULL rather than `[]` for the empty case, so "this artifact carries no tag"
// and "somebody wrote an empty list" stay distinguishable in a raw query - and
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
// re-derived from configuration at expansion time - which silently promoted
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

// WaveSummary is one wave's population, by state.
//
// # Why a wave needs its own row
//
// A transfer's totals answer "how much is left" and cannot answer "why is
// nothing happening", because the outstanding count mixes three populations
// that behave completely differently: jobs that can run, jobs waiting out a
// backoff, and jobs GATED behind a wave that has not drained. Five hundred
// outstanding with one in flight reads as a broken fleet and is usually four
// hundred and ninety-nine manifests correctly refusing to be pushed before
// their blobs land (invariant I1).
//
// Per wave, that becomes obvious at a glance: wave 0 nearly done, waves 1-3
// full and blocked.
type WaveSummary struct {
	Wave int
	// Kind is "blob", "manifest", or "mixed" where a wave holds both. In
	// practice blobs are wave 0 and manifests are the rest, and seeing that
	// stated is half of understanding the ordering.
	Kind string

	Total   int
	Done    int
	Running int
	// Pending is leasable now; Waiting is pending behind a retry backoff. The
	// two are the same state column and completely different situations.
	Pending int
	Waiting int
	Blocked int
	Failed  int

	PlannedBytes     int64
	TransferredBytes int64
}

// WaveProgress reports every wave of one transfer, lowest first.
//
// One grouped query over the jobs of one transfer, on the (transfer_id, state)
// index. Served per transfer rather than folded into the listing: forty
// transfers × four waves is a table nobody reads, and the question this answers
// is always asked about one transfer.
func (p *Packages) WaveProgress(ctx context.Context, transferID string) ([]WaveSummary, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT wave,
		       count(*),
		       SUM(CASE WHEN state IN ('succeeded','skipped') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'leased' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'pending' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'pending'
		                 AND next_visible_at > `+p.dialect.Now()+` THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'blocked' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END),
		       COALESCE(SUM(size_bytes), 0),
		       COALESCE(SUM(bytes_transferred), 0),
		       MIN(kind), MAX(kind)
		  FROM jobs
		 WHERE transfer_id = ?
		 GROUP BY wave
		 ORDER BY wave`), transferID)
	if err != nil {
		return nil, fmt.Errorf("wave progress for transfer %s: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []WaveSummary
	for rows.Next() {
		var (
			w                WaveSummary
			minKind, maxKind string
		)
		if err := rows.Scan(&w.Wave, &w.Total, &w.Done, &w.Running, &w.Pending,
			&w.Waiting, &w.Blocked, &w.Failed, &w.PlannedBytes, &w.TransferredBytes,
			&minKind, &maxKind); err != nil {
			return nil, fmt.Errorf("scan wave summary: %w", err)
		}
		// Pending counts everything in that state; Waiting is the subset behind
		// a backoff. Subtracting leaves what a worker could take right now,
		// which is the number that explains an idle fleet.
		w.Pending -= w.Waiting
		w.Kind = minKind
		if minKind != maxKind {
			w.Kind = "mixed"
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
