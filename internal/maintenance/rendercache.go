package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// RenderCacheSweeper keeps the cache of rendered charts inside its budget.
//
// LEADER-GATED, like every loop that writes. Two replicas evicting the same
// rows would not corrupt anything - a re-render rebuilds any row either of them
// removes - but the second one's log line would report having freed nothing,
// which reads like a bug.
//
// What it removes and what it must not is store.SweepRenderCache's business.
// This decides WHEN, and what to say about it.
type RenderCacheSweeper struct {
	packages *store.Packages
	policy   store.RenderCachePolicy
	interval time.Duration
	log      *slog.Logger

	mu     sync.Mutex
	leader bool
}

// NewRenderCacheSweeper builds the loop.
func NewRenderCacheSweeper(
	packages *store.Packages, policy store.RenderCachePolicy,
	interval time.Duration, log *slog.Logger,
) *RenderCacheSweeper {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &RenderCacheSweeper{
		packages: packages, policy: policy, interval: interval, log: log,
	}
}

// SetLeader is called by the elector on every leadership change.
func (s *RenderCacheSweeper) SetLeader(isLeader bool) {
	s.mu.Lock()
	s.leader = isLeader
	s.mu.Unlock()
}

// Enabled reports whether anything would be reclaimed.
func (s *RenderCacheSweeper) Enabled() bool {
	return s.packages != nil && s.policy.Enabled()
}

// Run sweeps on the interval until the context is cancelled.
func (s *RenderCacheSweeper) Run(ctx context.Context) error {
	if !s.Enabled() {
		s.log.Info("render cache: no budget and no ttl configured; nothing will be reclaimed")
		return nil
	}
	s.log.Info("render cache: sweeping",
		"budgetBytes", s.policy.Budget, "ttl", s.policy.TTL, "interval", s.interval)

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
				s.log.ErrorContext(ctx, "render cache: sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce runs one pass, and is what a test drives.
//
// Logged only when something was reclaimed. A sweep that removes nothing is the
// steady state and would otherwise be a log line an hour, forever, saying
// nothing - which is how a log stops being read.
func (s *RenderCacheSweeper) SweepOnce(ctx context.Context) (store.RenderCacheResult, error) {
	res, err := s.packages.SweepRenderCache(ctx, s.policy)
	if err != nil {
		return res, err
	}
	if res.Rows() > 0 {
		s.log.InfoContext(ctx, "render cache: reclaimed",
			"expired", res.Expired, "trimmed", res.Trimmed, "bytes", res.Bytes)
	}
	return res, nil
}
