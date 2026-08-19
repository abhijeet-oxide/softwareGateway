package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The lease loop.
//
// Ask for work up to remaining capacity, run what comes back, report each
// result, ask again. A heartbeat runs alongside, renewing the leases held.
//
// There is no state to recover on restart and no shutdown handshake to get
// right: a worker that dies mid-transfer simply stops renewing, and the
// Coordinator's reaper returns its jobs to the queue one lease period later.
// That is the whole crash-recovery story, and it is the same story for a
// SIGKILL, a preempted node and a network partition (docs/design/04 §4.3).

// Coordinator is the worker plane as this loop needs it.
//
// A consumer-defined interface so the loop can be tested against a fake
// without an HTTP server, and so the real client stays a leaf dependency.
type Coordinator interface {
	LeaseJobs(ctx context.Context, req v1.LeaseRequest) (*v1.LeaseResponse, error)
	ReportProgress(ctx context.Context, jobID string, req v1.ProgressRequest) error
	CompleteJob(ctx context.Context, jobID string, req v1.CompleteRequest) (*v1.CompleteResponse, error)
	Heartbeat(ctx context.Context, workerID string, req v1.HeartbeatRequest) (*v1.HeartbeatResponse, error)
}

// Options configure a worker.
type Options struct {
	WorkerID string
	Version  string
	// MaxConcurrentJobs is how many blobs this process moves at once. The
	// memory ceiling follows directly from it (docs/design/05 §4.5).
	MaxConcurrentJobs int
	CopyBufferSize    int
	HeartbeatInterval time.Duration
	// ProgressInterval throttles progress reports. A 250 MB blob should
	// produce a handful, not thousands.
	ProgressInterval time.Duration
	// StallTimeout is how long ONE job may make no progress before the worker
	// gives up on it. Zero uses DefaultStallTimeout; negative disables it.
	//
	// Not a deadline: a job that is moving beats the clock and may run for
	// hours, which an 8 GB blob on a slow link legitimately needs. See
	// watchdog.go for why the distinction is movement rather than elapsed time.
	StallTimeout time.Duration
}

// Loop runs jobs until its context is cancelled.
type Loop struct {
	opts    Options
	coord   Coordinator
	clients *regclient.Clients
	engine  *transfer.Engine
	log     *slog.Logger

	sem *semaphore.Weighted

	// active tracks what this worker holds, for the heartbeat to renew.
	mu     sync.Mutex
	active map[int64]*watchdog
}

// Defaults for the knobs a deployment usually leaves alone.
const (
	DefaultMaxConcurrentJobs = 16
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultProgressInterval  = 2 * time.Second
	// DefaultStallTimeout is how long one job may make no progress.
	//
	// Fifteen minutes. Long enough that an ordinary slow exchange over a
	// high-latency proxy is never mistaken for a stall - the longest legitimate
	// single request observed on such a link took about half that - and short
	// enough that a registry which stops answering costs one attempt rather
	// than a night. A job that IS moving is unaffected at any duration, because
	// progress resets the clock.
	DefaultStallTimeout = 15 * time.Minute
)

// NewLoop builds the worker's run loop.
func NewLoop(coord Coordinator, clients *regclient.Clients, opts Options, log *slog.Logger) *Loop {
	if log == nil {
		log = slog.Default()
	}
	if opts.MaxConcurrentJobs <= 0 {
		opts.MaxConcurrentJobs = DefaultMaxConcurrentJobs
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = DefaultProgressInterval
	}

	return &Loop{
		opts:    opts,
		coord:   coord,
		clients: clients,
		engine:  transfer.NewEngine(opts.CopyBufferSize, log),
		log:     log,
		sem:     semaphore.NewWeighted(int64(opts.MaxConcurrentJobs)),
		active:  map[int64]*watchdog{},
	}
}

// Run leases and executes until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.heartbeatLoop(ctx)
	}()

	l.log.InfoContext(ctx, "worker started",
		"worker", l.opts.WorkerID, "maxConcurrentJobs", l.opts.MaxConcurrentJobs)

	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			// Jobs in flight are abandoned rather than waited for. Their
			// leases expire and the queue redoes them, which is cheaper and
			// far simpler than a drain protocol - and it is the path that has
			// to work anyway, because a SIGKILL gets no drain.
			wg.Wait()
			l.log.InfoContext(ctx, "worker stopped", "worker", l.opts.WorkerID)
			return nil
		case <-time.After(wait):
		}

		wait = l.tick(ctx)
	}
}

// tick performs one lease-and-dispatch, returning how long to wait next.
func (l *Loop) tick(ctx context.Context) time.Duration {
	capacity := l.capacity()
	if capacity == 0 {
		// Full. Come back when something finishes rather than asking for work
		// there is nowhere to put.
		return l.opts.ProgressInterval
	}

	res, err := l.coord.LeaseJobs(ctx, v1.LeaseRequest{
		WorkerID:   l.opts.WorkerID,
		Capacity:   capacity,
		ActiveJobs: l.activeCount(),
		Version:    l.opts.Version,
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		// A Coordinator that cannot be reached is not this worker's problem to
		// solve. Back off and keep asking: jobs already in flight carry on,
		// because bytes do not flow through the Coordinator.
		l.log.WarnContext(ctx, "could not lease work", "error", err)
		return DefaultLeaseRetry
	}

	for _, job := range res.Jobs {
		l.dispatch(ctx, job)
	}

	// Server-directed. An idle worker is told when to come back, so an empty
	// queue with forty workers does not become a poll storm.
	if res.NextPollAfterSeconds > 0 {
		return time.Duration(res.NextPollAfterSeconds) * time.Second
	}
	return DefaultLeaseRetry
}

// DefaultLeaseRetry is the fallback poll interval when the Coordinator
// directs none - a failed lease, or an old Coordinator.
const DefaultLeaseRetry = 5 * time.Second

// dispatch starts one job, bounded by the semaphore.
func (l *Loop) dispatch(ctx context.Context, job v1.LeasedJob) {
	id, err := strconv.ParseInt(job.JobID, 10, 64)
	if err != nil {
		l.log.ErrorContext(ctx, "the Coordinator sent an unusable job ID",
			"jobId", job.JobID, "error", err)
		return
	}

	if err := l.sem.Acquire(ctx, 1); err != nil {
		return // context cancelled
	}

	jobCtx, cancel := context.WithCancel(ctx)
	dog := newWatchdog(cancel, l.stallTimeout())
	l.track(id, dog)

	go func() {
		defer l.sem.Release(1)
		defer l.untrack(id)
		defer dog.stop()
		defer cancel()

		l.run(jobCtx, id, job, dog)
	}()
}

// run executes one job and reports the outcome.
func (l *Loop) run(ctx context.Context, id int64, job v1.LeasedJob, dog *watchdog) {
	source, target, err := l.endpoints(job)
	if err != nil {
		l.report(ctx, id, job, transfer.Result{
			Outcome:    transfer.OutcomeFailed,
			ErrorClass: transfer.ClassConfiguration,
			Err:        err,
		})
		return
	}

	size, err := strconv.ParseInt(string(job.SizeBytes), 10, 64)
	if err != nil {
		size = 0 // an unknown size is not fatal; the registry validates the digest
	}

	res := l.engine.Run(ctx, transfer.Job{
		ID:             id,
		TransferID:     job.TransferID,
		Kind:           job.Kind,
		Digest:         job.Digest,
		SizeBytes:      size,
		MediaType:      job.MediaType,
		Attempt:        job.Attempt,
		Source:         source,
		Target:         target,
		KnownPlacement: job.KnownPlacement,
		RepairLevel:    job.RepairLevel,
		MountFrom:      job.MountFromRepository,
		Tags:           job.Tags,
	}, l.progressReporter(ctx, job.JobID, dog))

	// A job the watchdog cancelled looks, from here, exactly like one cancelled
	// by shutdown: a context error. They are opposite things - one is a
	// registry that stopped answering and must be recorded, the other is a
	// worker going away and must not be - so the outcome is rewritten with what
	// only the watchdog knows.
	if dog.tripped() {
		res = transfer.Result{
			Outcome:    transfer.OutcomeFailed,
			ErrorClass: string(registry.ClassTimeout),
			BytesMoved: res.BytesMoved,
			Duration:   res.Duration,
			Err: fmt.Errorf("%w: no progress for %s; abandoned so another attempt can run",
				registry.ErrTimeout, l.stallTimeout()),
		}
	}

	l.report(ctx, id, job, res)
}

func (l *Loop) endpoints(job v1.LeasedJob) (source, target registry.Repository, err error) {
	src, err := l.clients.For(job.Source)
	if err != nil {
		return nil, nil, err
	}
	dst, err := l.clients.For(job.Target)
	if err != nil {
		return nil, nil, err
	}
	return src, dst, nil
}

// progressReporter throttles progress to one report per interval.
//
// The engine reports every buffer; sending each would turn a 250 MB blob into
// thousands of HTTP calls to report a number nobody is watching that closely.
func (l *Loop) progressReporter(
	ctx context.Context, jobID string, dog *watchdog,
) transfer.ProgressFunc {
	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(bytes int64) {
		// BEFORE the throttle. The throttle exists to spare the Coordinator
		// thousands of reports for one blob; the watchdog needs every one of
		// them, because "did this move" is exactly the question it asks and a
		// throttled beat would let a transferring blob be abandoned.
		dog.beat()

		mu.Lock()
		if time.Since(last) < l.opts.ProgressInterval {
			mu.Unlock()
			return
		}
		last = time.Now()
		mu.Unlock()

		err := l.coord.ReportProgress(ctx, jobID, v1.ProgressRequest{
			WorkerID:         l.opts.WorkerID,
			BytesTransferred: v1.Int64String(strconv.FormatInt(bytes, 10)),
		})
		if err != nil && ctx.Err() == nil {
			// Lossy by design. A dropped progress report costs a stale number
			// in a UI; abandoning a half-transferred blob over one would cost
			// the blob.
			l.log.DebugContext(ctx, "progress report dropped", "job", jobID, "error", err)
		}
	}
}

// report tells the Coordinator what happened.
//
// Completion is NOT lossy, so a failure to report is retried briefly. Beyond
// that the lease expires and the job is redone - correct, because content
// addressing makes double execution harmless, and cheaper than blocking a
// worker slot on an unreachable Coordinator.
func (l *Loop) report(ctx context.Context, id int64, job v1.LeasedJob, res transfer.Result) {
	req := v1.CompleteRequest{
		WorkerID:         l.opts.WorkerID,
		Outcome:          outcomeDTO(res.Outcome),
		BytesTransferred: v1.Int64String(strconv.FormatInt(res.BytesMoved, 10)),
		SkipReason:       v1.SkipReason(res.SkipReason),
		ErrorClass:       res.ErrorClass,
		DurationMs:       res.Duration.Milliseconds(),
		Attempt:          job.Attempt,
		Placed:           res.Placed,
	}
	if res.Err != nil {
		req.ErrorMessage = res.Err.Error()
		l.log.WarnContext(ctx, "job failed",
			"job", id, "kind", job.Kind, "digest", job.Digest,
			"class", res.ErrorClass, "error", res.Err)
	}

	// A short bounded retry, on a context that survives the job's own
	// cancellation: a cancelled job still has an outcome worth recording.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-reportCtx.Done():
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if _, err := l.coord.CompleteJob(reportCtx, job.JobID, req); err == nil {
			return
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
	}
	l.log.ErrorContext(ctx, "could not report a completion; the lease will expire and the job will be redone",
		"job", id)
}

func outcomeDTO(o transfer.Outcome) v1.JobState {
	switch o {
	case transfer.OutcomeSucceeded:
		return v1.JobSucceeded
	case transfer.OutcomeSkipped:
		return v1.JobSkipped
	default:
		return v1.JobFailed
	}
}

// heartbeatLoop renews the leases this worker holds.
//
// It also delivers cancellation, which has no push channel: a cancelled job
// learns of it here and aborts within one interval. Polling rather than a
// stream is deliberate - no connection management, no reconnect logic, no
// server-side registry of live connections, and it degrades gracefully.
func (l *Loop) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(l.opts.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		held := l.heldIDs()
		if len(held) == 0 {
			continue
		}

		res, err := l.coord.Heartbeat(ctx, l.opts.WorkerID, v1.HeartbeatRequest{ActiveJobIDs: held})
		if err != nil {
			if ctx.Err() == nil {
				l.log.WarnContext(ctx, "heartbeat failed", "error", err)
			}
			continue
		}

		l.abandonLost(ctx, held, res)
	}
}

// abandonLost stops work on jobs this worker no longer holds.
//
// A job missing from LeasesRenewed has been reaped and may already be running
// elsewhere. Continuing to push its bytes would not corrupt anything - content
// addressing sees to that - but it would waste bandwidth against a vendor
// registry for a result that will be discarded.
func (l *Loop) abandonLost(ctx context.Context, held []string, res *v1.HeartbeatResponse) {
	renewed := make(map[string]bool, len(res.LeasesRenewed))
	for _, id := range res.LeasesRenewed {
		renewed[id] = true
	}
	for _, id := range res.CancelledJobIDs {
		renewed[id] = false
	}

	for _, id := range held {
		if renewed[id] {
			continue
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}
		l.mu.Lock()
		dog, ok := l.active[n]
		l.mu.Unlock()
		if ok {
			l.log.InfoContext(ctx, "abandoning a job this worker no longer holds", "job", id)
			dog.cancel()
		}
	}
}

// ---------------------------------------------------------------------------
// Bookkeeping
// ---------------------------------------------------------------------------

func (l *Loop) track(id int64, dog *watchdog) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active[id] = dog
}

// stallTimeout is the configured bound, or the default.
func (l *Loop) stallTimeout() time.Duration {
	if l.opts.StallTimeout != 0 {
		return l.opts.StallTimeout
	}
	return DefaultStallTimeout
}

func (l *Loop) untrack(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.active, id)
}

func (l *Loop) activeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.active)
}

func (l *Loop) capacity() int {
	return l.opts.MaxConcurrentJobs - l.activeCount()
}

func (l *Loop) heldIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]string, 0, len(l.active))
	for id := range l.active {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}
