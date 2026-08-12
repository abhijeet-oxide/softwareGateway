// Package queue is the Coordinator's side of the work queue.
//
// It owns leasing, progress, completion and reaping — everything that decides
// WHICH work a worker gets and what happens when it comes back. The bytes are
// the worker's business; none pass through here.
//
// See docs/design/04-queue-and-scheduling.md.
//
// The package sits above persistence and below the API: it takes and returns
// domain values, and knows nothing about HTTP. That is what lets the same
// logic serve the worker plane, a test, and any later transport without being
// reimplemented (docs/design/15 §2).
package queue

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// Defaults from docs/design/04 §4.3 and §7.1.
const (
	// DefaultLeaseDuration is how long a lease is good for without renewal.
	// Two minutes: long enough that a brief Coordinator hiccup does not strand
	// in-flight work, short enough that a dead worker's jobs return promptly.
	DefaultLeaseDuration = 2 * time.Minute
	// DefaultReapInterval is how often the leader looks for expired leases.
	DefaultReapInterval = 30 * time.Second
	// DefaultIdlePoll is what an empty queue tells a worker to wait.
	//
	// Server-directed rather than worker-chosen, so forty idle workers do not
	// become a poll storm and so the Coordinator can change the answer without
	// redeploying the fleet.
	DefaultIdlePoll = 5 * time.Second
	// DefaultBusyPoll is what a worker is told when it got work — come back
	// promptly, there may be more.
	DefaultBusyPoll = 1 * time.Second
)

// Queue serves the worker plane.
type Queue struct {
	packages *store.Packages
	log      *slog.Logger

	leaseDuration time.Duration
}

// New builds a queue service.
func New(packages *store.Packages, leaseDuration time.Duration, log *slog.Logger) *Queue {
	if log == nil {
		log = slog.Default()
	}
	if leaseDuration <= 0 {
		leaseDuration = DefaultLeaseDuration
	}
	return &Queue{packages: packages, log: log, leaseDuration: leaseDuration}
}

// LeaseDuration is how long the leases this queue hands out are good for.
func (q *Queue) LeaseDuration() time.Duration { return q.leaseDuration }

// Assignment is one job as it goes to a worker, with everything needed to
// execute it and nothing else.
type Assignment struct {
	store.LeasedJob

	Source store.Endpoint
	Target store.Endpoint

	// KnownPlacement is the placement fast path, resolved for this batch.
	KnownPlacement bool

	// MountFrom is a repository on the same target registry already holding
	// this digest, resolved for this batch so the worker can mount instead of
	// streaming. Empty when nothing nearby has it yet.
	MountFrom string

	// Tags are what this manifest must be called at the destination,
	// resolved at planning time from the source's own annotations.
	//
	// This used to be a single tag computed HERE, by comparing the job's
	// digest against the package's root — which could only ever name the one
	// artifact a person had asked for, and left every component of a bundle
	// digest-addressed at the destination. The planner knows better and knows
	// it earlier.
	Tags []string
}

// LeaseResult is one lease call's answer.
type LeaseResult struct {
	Jobs []Assignment
	// LeaseDuration is how long the worker may hold these before renewing.
	LeaseDuration time.Duration
	// NextPollAfter is server-directed backoff. An idle worker is told when to
	// come back; long-polling was considered and server-directed intervals are
	// simpler and give the Coordinator direct control (docs/design/09 §7.1).
	NextPollAfter time.Duration
}

// Lease hands a worker up to `capacity` jobs.
func (q *Queue) Lease(ctx context.Context, workerID string, capacity int) (LeaseResult, error) {
	res := LeaseResult{LeaseDuration: q.leaseDuration, NextPollAfter: DefaultIdlePoll}

	jobs, err := q.packages.LeaseJobs(ctx, store.LeaseRequest{
		Owner:    workerID,
		Limit:    capacity,
		Duration: q.leaseDuration,
	})
	if err != nil {
		return res, err
	}
	if len(jobs) == 0 {
		return res, nil
	}
	res.NextPollAfter = DefaultBusyPoll

	assignments, err := q.hydrate(ctx, jobs)
	if err != nil {
		// The jobs are leased but unusable. Hand them straight back rather
		// than letting them sit until the lease expires: a configuration
		// problem should surface as an immediate retry, not as a two-minute
		// stall repeated forever.
		q.releaseAll(ctx, workerID, jobs, err)
		return res, err
	}
	res.Jobs = assignments
	return res, nil
}

// hydrate turns leased rows into executable assignments.
//
// Three lookups for the whole BATCH rather than per job: endpoints, placements
// and the transfer tag. A worker holding sixteen jobs therefore costs three
// queries, not forty-eight (docs/design/05 §4.1).
func (q *Queue) hydrate(ctx context.Context, jobs []store.LeasedJob) ([]Assignment, error) {
	repoIDs := make([]int64, 0, len(jobs)*2)
	digests := make([]string, 0, len(jobs))
	for _, j := range jobs {
		repoIDs = append(repoIDs, j.SourceRepoID, j.TargetRepoID)
		digests = append(digests, j.Digest)
	}

	endpoints, err := q.packages.HydrateEndpoints(ctx, repoIDs)
	if err != nil {
		return nil, err
	}

	// Placements are looked up per target repository, because a digest present
	// in one destination says nothing about another.
	placed := map[int64]map[string]bool{}
	// And where the destination itself does NOT have it, whether a sibling
	// repository on the same registry does — the difference between mounting a
	// blob and fetching it across the WAN a second time.
	mountable := map[int64]map[string]string{}
	for _, j := range jobs {
		if _, done := placed[j.TargetRepoID]; done {
			continue
		}
		hits, err := q.packages.PlacedDigests(ctx, j.TargetRepoID, digests)
		if err != nil {
			return nil, err
		}
		placed[j.TargetRepoID] = hits

		near, err := q.packages.MountableFrom(ctx, j.TargetRepoID, digests)
		if err != nil {
			return nil, err
		}
		mountable[j.TargetRepoID] = near
	}

	out := make([]Assignment, 0, len(jobs))
	for _, j := range jobs {
		src, ok := endpoints[j.SourceRepoID]
		if !ok {
			return nil, fmt.Errorf("job %d: source repository %d is not in the catalog",
				j.ID, j.SourceRepoID)
		}
		dst, ok := endpoints[j.TargetRepoID]
		if !ok {
			return nil, fmt.Errorf("job %d: target repository %d is not in the catalog",
				j.ID, j.TargetRepoID)
		}

		known := j.Kind == "blob" && placed[j.TargetRepoID][j.Digest]
		mountFrom := ""
		if j.Kind == "blob" && !known {
			mountFrom = mountable[j.TargetRepoID][j.Digest]
		}

		out = append(out, Assignment{
			LeasedJob:      j,
			Source:         src,
			Target:         dst,
			KnownPlacement: known,
			MountFrom:      mountFrom,
			Tags:           j.TargetTags,
		})
	}
	return out, nil
}

// releaseAll returns a batch to the queue after a hydration failure.
//
// Best effort by design: if this fails too, the reaper collects the jobs one
// lease period later. Nothing is lost either way — that is the property leases
// exist to provide.
func (q *Queue) releaseAll(ctx context.Context, workerID string, jobs []store.LeasedJob, cause error) {
	for _, j := range jobs {
		_, err := q.packages.CompleteJob(ctx, store.Completion{
			JobID:      j.ID,
			Owner:      workerID,
			Outcome:    "failed",
			Attempt:    j.Attempt,
			ErrorClass: transfer.ClassConfiguration,
			ErrorMsg:   cause.Error(),
		})
		if err != nil {
			q.log.WarnContext(ctx, "could not return an unusable job to the queue",
				"job", j.ID, "error", err)
		}
	}
}

// Progress records how far a blob has got. Lossy by design.
func (q *Queue) Progress(ctx context.Context, jobID int64, workerID string, bytes int64) error {
	return q.packages.ReportProgress(ctx, jobID, workerID, bytes)
}

// Complete records a finished job and everything that follows from it.
func (q *Queue) Complete(ctx context.Context, c store.Completion) (store.CompletionResult, error) {
	// The attempt cap is decided HERE, from the error class, rather than being
	// the column default for everything (docs/design/11 §2.3). A transient 5xx
	// deserves eight attempts because registries recover; a digest mismatch
	// deserves two because retrying rarely helps; an auth failure or a
	// misconfigured worker deserves one, because credentials do not fix
	// themselves and hammering an auth endpoint gets the fleet throttled.
	//
	// The policy existed and was never consulted, so every job got eight
	// whatever went wrong.
	if c.Outcome == "failed" && c.MaxAttempts == 0 {
		c.MaxAttempts = transfer.MaxAttemptsFor(c.ErrorClass)
	}

	res, err := q.packages.CompleteJob(ctx, c)
	if err != nil {
		return res, err
	}
	if !res.Applied {
		// The expected outcome of a worker finishing after its lease expired.
		// Logged at DEBUG rather than WARN: it is a designed-for event, and at
		// WARN it would be noise on every worker restart.
		q.log.DebugContext(ctx, "completion ignored: the lease had already expired",
			"job", c.JobID, "worker", c.Owner)
		return res, nil
	}

	q.log.InfoContext(ctx, "job completed",
		"job", c.JobID,
		"outcome", c.Outcome,
		"skipReason", c.SkipReason,
		"bytes", c.BytesTransferred,
		"transferState", res.TransferState,
		"waveAdvanced", res.WaveAdvanced,
		// How many artifacts this one completion made pushable. Under wave
		// gating it was always zero until a whole wave drained; a run where it
		// stays zero now means the dependency graph did not get written.
		"promoted", res.Promoted,
	)
	return res, nil
}

// Heartbeat renews a worker's leases and reports what it may keep.
//
// One call carrying two signals: lease renewal, and — by omission — which jobs
// the worker has lost and must abandon. There is no push channel to workers,
// so this is also where cancellation would be delivered (docs/design/09 §7.4).
func (q *Queue) Heartbeat(ctx context.Context, workerID string, activeJobs []int64) ([]int64, error) {
	// A heartbeat is the only signal from a worker holding a long blob: it can
	// go many minutes between leases and completions, so without recording it
	// here the fleet view would call a perfectly healthy worker stale.
	//
	// Capacity is unknown on this path, so the stored ceiling is left as it was
	// — the lease that set it is the caller that knows it.
	q.recordHeartbeat(ctx, workerID, len(activeJobs))
	return q.packages.RenewLeases(ctx, workerID, activeJobs, q.leaseDuration)
}

// RecordWorker notes what a worker just told us about itself.
//
// Best effort by contract: the fleet view is worth having and is worth nothing
// at the cost of a failed lease, so the caller logs and carries on. A worker row
// is observability — nothing in the queue reads it, and crash recovery remains
// the reaper and a timestamp on the job.
//
// Capacity is what is LEFT, so the configured ceiling is capacity plus what the
// worker already holds — the number an operator recognises from their config
// file rather than a remainder that shrinks as work starts.
func (q *Queue) RecordWorker(ctx context.Context, id string, capacity, active int, version string) {
	if id == "" {
		return
	}
	err := q.packages.RecordWorker(ctx, store.WorkerRow{
		ID: id, Version: version,
		MaxConcurrency: capacity + active, ActiveJobs: active,
	})
	if err != nil {
		q.log.WarnContext(ctx, "could not record worker", "worker", id, "error", err)
	}
}

// Workers reports the fleet.
func (q *Queue) Workers(ctx context.Context) ([]store.WorkerSummary, error) {
	return q.packages.ListWorkers(ctx, q.leaseDuration)
}

// recordHeartbeat refreshes liveness and the active count, keeping whatever
// ceiling a previous lease recorded.
func (q *Queue) recordHeartbeat(ctx context.Context, workerID string, active int) {
	if workerID == "" {
		return
	}
	if err := q.packages.TouchWorker(ctx, workerID, active); err != nil {
		q.log.WarnContext(ctx, "could not record worker heartbeat",
			"worker", workerID, "error", err)
	}
}

// Reap returns work held by workers that stopped heartbeating.
func (q *Queue) Reap(ctx context.Context) ([]store.ReapedJob, error) {
	reaped, err := q.packages.ReapExpiredLeases(ctx)
	if err != nil {
		return nil, err
	}
	for _, j := range reaped {
		q.log.WarnContext(ctx, "lease expired; job returned to the queue",
			"job", j.ID, "transfer", j.TransferID, "state", j.State)
	}
	return reaped, nil
}

// Settle marks transfers that can no longer make progress as failed.
//
// Paired with Reap deliberately, and run straight after it: reaping is what
// PRODUCES the terminal failure in the case that matters. When a network outage
// takes the workers with it, nothing completes and nothing reports — the leases
// simply expire, and a lease expiring on a job with no attempts left fails that
// job without any completion path running. Without this the transfer would keep
// saying `running` with nothing running.
func (q *Queue) Settle(ctx context.Context) ([]store.StalledTransfer, error) {
	stalled, err := q.packages.SettleStalledTransfers(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range stalled {
		q.log.WarnContext(ctx, "transfer stalled and cannot restart itself",
			"transfer", t.ID, "failedJobs", t.Failed, "reason", t.Reason)
	}
	return stalled, nil
}

// Retry returns a transfer's failed jobs to the queue.
func (q *Queue) Retry(ctx context.Context, transferID string) (store.RetryResult, error) {
	res, err := q.packages.RetryTransfer(ctx, transferID)
	if err != nil {
		return res, err
	}
	if res.Requeued > 0 {
		q.log.InfoContext(ctx, "requeued failed jobs",
			"transfer", res.TransferID, "jobs", res.Requeued, "state", res.State)
	}
	return res, nil
}

// Retryable lists the transfers a fleet-wide retry would act on.
func (q *Queue) Retryable(ctx context.Context) ([]string, error) {
	return q.packages.RetryableTransfers(ctx)
}
