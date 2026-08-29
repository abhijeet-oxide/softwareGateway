package worker

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// How fast the loop can turn work over, independently of how fast a registry
// is.
//
// # The question
//
// A release is thousands of jobs, and most of them are SMALL: a manifest is one
// PUT of a few kilobytes, and a deduplicated blob is one HEAD that moves nothing
// at all. On a well-populated destination the majority of a transfer's jobs
// finish in milliseconds. What decides the wall clock then is not the network -
// it is how quickly the worker asks for the next batch.
//
// # The harness
//
// A Coordinator that hands out `total` jobs as fast as it is asked, and an
// endpoint resolver that fails instantly, so every job completes in
// microseconds. Whatever time the run takes is the loop's own overhead.
type instantCoordinator struct {
	remaining atomic.Int64
	leases    atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
}

func newInstantCoordinator(total int) *instantCoordinator {
	c := &instantCoordinator{done: make(chan struct{})}
	c.remaining.Store(int64(total))
	return c
}

func (c *instantCoordinator) LeaseJobs(
	_ context.Context, req v1.LeaseRequest,
) (*v1.LeaseResponse, error) {
	c.leases.Add(1)

	// The server's own batch ceiling, from dev/config.yaml.
	const maxBatch = 32
	want := min(req.Capacity, maxBatch)

	// The real Coordinator's answers: DefaultBusyPoll when it handed out work,
	// DefaultIdlePoll when it had none. See internal/queue/queue.go.
	out := &v1.LeaseResponse{LeaseDurationSeconds: 120, NextPollAfterSeconds: 5}
	for range want {
		left := c.remaining.Add(-1)
		if left < 0 {
			c.remaining.Add(1)
			break
		}
		id := strconv.FormatInt(left, 10)
		out.Jobs = append(out.Jobs, v1.LeasedJob{
			JobID: id, TransferID: "t", Kind: "blob", Digest: "sha256:" + id,
			// No product is configured on this worker, so endpoint resolution
			// fails immediately and the job finishes without touching a
			// registry. What is being timed is the loop, not a transfer.
			Source: v1.JobEndpoint{Product: "absent", Role: "source"},
			Target: v1.JobEndpoint{Product: "absent", Role: "target"},
		})
	}
	if len(out.Jobs) > 0 {
		out.NextPollAfterSeconds = 1
	}
	return out, nil
}

func (c *instantCoordinator) ReportProgress(context.Context, string, v1.ProgressRequest) error {
	return nil
}

func (c *instantCoordinator) CompleteJob(
	context.Context, string, v1.CompleteRequest,
) (*v1.CompleteResponse, error) {
	if c.remaining.Load() <= 0 {
		c.closeOnce.Do(func() { close(c.done) })
	}
	return &v1.CompleteResponse{}, nil
}

func (c *instantCoordinator) Heartbeat(
	context.Context, string, v1.HeartbeatRequest,
) (*v1.HeartbeatResponse, error) {
	return &v1.HeartbeatResponse{}, nil
}

// A release's worth of instantly-completing jobs, timed.
//
// The number to look at is jobs per second. Anything close to
// `MaxConcurrentJobs / pollInterval` means the loop is sleeping between
// batches rather than working.
func TestLoopThroughputOnInstantJobs(t *testing.T) {
	const (
		total       = 640
		concurrency = 16
	)
	coord := newInstantCoordinator(total)

	// A real registry with no products in it: endpoint resolution fails
	// immediately, so a job finishes without touching a network and what the
	// clock measures is the loop.
	clients := regclient.NewClients(product.NewRegistry(),
		product.NewSecretResolver(""), "", slog.New(slog.DiscardHandler))

	loop := NewLoop(coord, clients, Options{
		WorkerID:          "bench",
		MaxConcurrentJobs: concurrency,
		HeartbeatInterval: time.Hour,
		ProgressInterval:  time.Second,
	}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	start := time.Now()
	go func() { _ = loop.Run(ctx) }()

	select {
	case <-coord.done:
	case <-ctx.Done():
		t.Fatalf("only %d of %d jobs were handed out in %v",
			int64(total)-coord.remaining.Load(), total, time.Since(start))
	}
	elapsed := time.Since(start)

	t.Logf("%d jobs at concurrency %d in %v = %.0f jobs/s, over %d lease calls",
		total, concurrency, elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds(), coord.leases.Load())

	/*
	  Measured, on this harness:

	                       time    jobs/s   lease calls
	    sleeping loop      39.0s       16            40
	    refill on wake      3ms   186,000           317
	    plus a debounce   992ms       645            41

	  The first row is the bug: exactly MaxConcurrentJobs per poll interval,
	  with the network idle throughout. The second fixes it and asks the
	  Coordinator eight times as often - one round trip per two jobs, each a
	  transaction and three hydration queries. The third keeps the throughput
	  and puts the round trips back where they were.

	  The assertions are deliberately loose. What is being checked is that the
	  loop is not gated on a clock; the exact rate belongs to whatever machine
	  is running it.
	*/
	if elapsed > 5*time.Second {
		t.Errorf("%d instantly-completing jobs took %v - the loop is sleeping "+
			"between batches rather than refilling as capacity frees",
			total, elapsed.Round(time.Millisecond))
	}

	// One lease call per job would be the other failure: a refill that fires on
	// every completion rather than on the burst.
	if calls := coord.leases.Load(); calls > total/4 {
		t.Errorf("%d lease calls for %d jobs - refills are not being batched, and "+
			"each one costs the Coordinator a transaction", calls, total)
	}
}
