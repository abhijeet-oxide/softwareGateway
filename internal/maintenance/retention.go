package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// RetentionSweeper deletes history past its retention.
//
// LEADER-GATED, like every loop that writes. Two replicas deleting the same
// rows would not corrupt anything — the statements are idempotent — but the
// second one's log line would report having removed nothing, which reads like a
// bug.
//
// What it removes and what it must not is store.SweepRetention's business, not
// this loop's: this decides WHEN, and what to say about it.
type RetentionSweeper struct {
	packages *store.Packages
	policy   store.RetentionPolicy
	interval time.Duration
	log      *slog.Logger

	mu     sync.Mutex
	leader bool
}

// NewRetentionSweeper builds the loop.
func NewRetentionSweeper(
	packages *store.Packages, policy store.RetentionPolicy,
	interval time.Duration, log *slog.Logger,
) *RetentionSweeper {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &RetentionSweeper{
		packages: packages, policy: policy, interval: interval, log: log,
	}
}

// SetLeader is called by the elector on every leadership change.
func (s *RetentionSweeper) SetLeader(isLeader bool) {
	s.mu.Lock()
	s.leader = isLeader
	s.mu.Unlock()
}

// Enabled reports whether any retention is in force.
func (s *RetentionSweeper) Enabled() bool {
	return s.packages != nil && s.policy.Enabled()
}

// Run sweeps on the interval until the context is cancelled.
//
// Never returns an error for a failed sweep, for the same reason the manifest
// cache sweeper does not: the worst outcome of a sweeper that cannot run is a
// database larger than intended, and killing the Coordinator over that would
// turn a disk-usage problem into an outage.
func (s *RetentionSweeper) Run(ctx context.Context) error {
	if !s.Enabled() {
		s.log.Info("retention: nothing is configured to expire; history is kept forever")
		return nil
	}

	s.log.Info("retention: sweeping",
		"transfers", s.policy.Transfers,
		"workerLogs", s.policy.WorkerLogs,
		"auditEvents", s.policy.AuditEvents,
		"placements", s.policy.Placements,
		"interval", s.interval)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.mu.Lock()
			isLeader := s.leader
			s.mu.Unlock()
			if !isLeader {
				continue
			}
			if _, err := s.SweepOnce(ctx); err != nil {
				s.log.ErrorContext(ctx, "retention: sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce runs one sweep and reports what it removed.
func (s *RetentionSweeper) SweepOnce(ctx context.Context) (store.RetentionResult, error) {
	res, err := s.packages.SweepRetention(ctx, s.policy)
	if err != nil {
		return res, err
	}

	// Logged only when something happened. A sweep that removes nothing is the
	// steady state and would otherwise be a log line an hour, forever, saying
	// nothing — which is how a log stops being read.
	if res.Rows() > 0 {
		s.log.InfoContext(ctx, "retention: removed",
			"transfers", res.Transfers,
			"requests", res.Requests,
			"workerLogs", res.WorkerLogs,
			"auditEvents", res.AuditEvents,
			"placements", res.Placements)
	}
	return res, nil
}
