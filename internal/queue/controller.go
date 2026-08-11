package queue

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// The Coordinator's background loops for the queue: the reaper, and the
// expander that turns requests into work.
//
// BOTH RUN ON THE LEADER ONLY. Not for correctness — every step is idempotent,
// so a brief period with two leaders duplicates work without corrupting
// anything (docs/design/04 §9) — but because a second replica reaping and
// expanding buys nothing and doubles the registry walks.

// Expander turns pending transfer requests into planned transfers.
//
// A consumer-defined interface so this package does not import
// internal/transfer, which would point the dependency the wrong way: the
// planner already depends on the store, and the queue would then depend on the
// planner for one call.
type Expander interface {
	Expand(ctx context.Context) (requests, jobs int, err error)
}

// ControllerOptions tune the two loops.
type ControllerOptions struct {
	// ReapInterval is how often expired leases are collected.
	ReapInterval time.Duration
	// ExpandInterval is how often pending requests are turned into jobs.
	ExpandInterval time.Duration
}

// Controller runs the leader-gated queue loops.
type Controller struct {
	queue    *Queue
	expander Expander
	opts     ControllerOptions
	log      *slog.Logger

	leader atomic.Bool
}

// DefaultExpandInterval is how often the leader looks for new requests.
//
// Ten seconds, matching the scheduler tick in docs/design/04 §10: a request
// created by an auto-download rule should become work promptly, and a poll
// this cheap — one indexed query returning nothing — costs nothing when idle.
const DefaultExpandInterval = 10 * time.Second

// NewController builds the background loops.
func NewController(q *Queue, e Expander, opts ControllerOptions, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	if opts.ReapInterval <= 0 {
		opts.ReapInterval = DefaultReapInterval
	}
	if opts.ExpandInterval <= 0 {
		opts.ExpandInterval = DefaultExpandInterval
	}
	return &Controller{queue: q, expander: e, opts: opts, log: log}
}

// SetLeader gates the loops. Called by leader election.
func (c *Controller) SetLeader(isLeader bool) { c.leader.Store(isLeader) }

// IsLeader reports whether the loops are running.
func (c *Controller) IsLeader() bool { return c.leader.Load() }

// Run ticks both loops until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	reap := time.NewTicker(c.opts.ReapInterval)
	defer reap.Stop()
	expand := time.NewTicker(c.opts.ExpandInterval)
	defer expand.Stop()

	// The reaper runs IMMEDIATELY on becoming leader rather than waiting a full
	// interval. After a Coordinator restart, leases held by workers that died
	// during the outage are requeued at once instead of thirty seconds later
	// (docs/design/04 §12).
	var wasLeader bool

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-reap.C:
			if !c.leader.Load() {
				wasLeader = false
				continue
			}
			c.reap(ctx)

		case <-expand.C:
			if !c.leader.Load() {
				wasLeader = false
				continue
			}
			if !wasLeader {
				wasLeader = true
				c.reap(ctx)
			}
			c.expand(ctx)
		}
	}
}

func (c *Controller) reap(ctx context.Context) {
	reaped, err := c.queue.Reap(ctx)
	if err != nil {
		c.log.ErrorContext(ctx, "the reaper failed", "error", err)
		return
	}
	if len(reaped) > 0 {
		c.log.InfoContext(ctx, "returned expired leases to the queue", "jobs", len(reaped))
	}

	// Immediately after reaping, because reaping is what turns a worker that
	// vanished into jobs that have finally run out of attempts. A transfer that
	// can no longer progress is marked failed here rather than being left to
	// report `running` until somebody notices.
	if _, err := c.queue.Settle(ctx); err != nil {
		c.log.ErrorContext(ctx, "could not settle stalled transfers", "error", err)
	}
}

func (c *Controller) expand(ctx context.Context) {
	if c.expander == nil {
		return
	}
	requests, jobs, err := c.expander.Expand(ctx)
	if err != nil {
		c.log.ErrorContext(ctx, "could not expand transfer requests", "error", err)
		return
	}
	if requests > 0 {
		c.log.InfoContext(ctx, "expanded transfer requests",
			"requests", requests, "jobs", jobs)
	}
}
