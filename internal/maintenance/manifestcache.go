// Package maintenance holds the Coordinator's housekeeping loops.
//
// A loop here owns no domain logic. It decides WHEN a piece of upkeep runs and
// what to say about it; the upkeep itself lives in the package that owns the
// data. That split is what makes both testable — the store method against a
// database, the schedule against a clock.
package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// ManifestCacheSweeper keeps the cached manifest bodies inside their budget.
//
// LEADER-GATED, like every other loop that writes. Two replicas sweeping the
// same table would not corrupt anything — the statements are idempotent — but
// they would double the write load to reclaim the same bytes, and the second
// one's log line would report having freed nothing, which reads like a bug.
//
// The thing being swept is a cache, not a record. See
// internal/store/manifestcache.go for why a package's manifest BODIES are
// evictable while everything else about it is kept forever.
type ManifestCacheSweeper struct {
	packages *store.Packages
	policy   store.ManifestCachePolicy
	interval time.Duration
	log      *slog.Logger
	metrics  *metrics.Registry

	mu     sync.Mutex
	leader bool
}

// NewManifestCacheSweeper builds the loop.
func NewManifestCacheSweeper(
	packages *store.Packages,
	policy store.ManifestCachePolicy,
	interval time.Duration,
	log *slog.Logger,
	m *metrics.Registry,
) *ManifestCacheSweeper {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &ManifestCacheSweeper{
		packages: packages, policy: policy, interval: interval, log: log, metrics: m,
	}
}

// SetLeader is called by the elector on every leadership change.
func (s *ManifestCacheSweeper) SetLeader(isLeader bool) {
	s.mu.Lock()
	s.leader = isLeader
	s.mu.Unlock()
}

// Enabled reports whether either bound is in force.
//
// A sweeper with no budget and no TTL is configuration saying "keep
// everything", which is a legitimate choice for a deployment with disk to
// spare. It runs no statements rather than running them and freeing nothing.
func (s *ManifestCacheSweeper) Enabled() bool {
	return s.packages != nil && (s.policy.BudgetBytes > 0 || s.policy.TTL > 0)
}

// Run sweeps on the interval until the context is cancelled.
//
// Never returns an error for a failed sweep. The cache being over budget is not
// a reason to stop serving: the worst outcome of a sweeper that cannot run is a
// database larger than intended, and killing the Coordinator over that would
// turn a disk-usage problem into an outage.
func (s *ManifestCacheSweeper) Run(ctx context.Context) error {
	if !s.Enabled() {
		s.log.Info("manifest cache: no budget and no ttl configured; nothing will be reclaimed")
		return nil
	}

	s.log.Info("manifest cache: sweeping",
		"budgetBytes", s.policy.BudgetBytes,
		"ttl", s.policy.TTL,
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
				s.log.ErrorContext(ctx, "manifest cache: sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce runs one sweep and reports what it reclaimed.
//
// Exported so a test — and, later, an operator-triggered maintenance call — can
// drive it without waiting for a tick.
func (s *ManifestCacheSweeper) SweepOnce(ctx context.Context) (store.SweepResult, error) {
	res, err := s.packages.SweepManifestCache(ctx, s.policy)
	if err != nil {
		return store.SweepResult{}, err
	}

	if s.metrics != nil {
		s.metrics.ManifestCacheBytes.Set(float64(res.RemainingBytes))
		if stats, err := s.packages.ManifestCacheStats(ctx); err == nil {
			s.metrics.ManifestCacheManifests.Set(float64(stats.CachedManifests))
		}
		if res.ExpiredManifests > 0 {
			s.metrics.ManifestCacheEvicted.WithLabelValues("expired").Add(float64(res.ExpiredManifests))
		}
		if res.EvictedManifests > 0 {
			s.metrics.ManifestCacheEvicted.WithLabelValues("budget").Add(float64(res.EvictedManifests))
		}
	}

	// Logged only when something happened. A sweep that reclaims nothing is the
	// steady state and would otherwise be four log lines an hour, forever,
	// saying nothing — which is how a log stops being read.
	if res.Manifests() > 0 {
		s.log.InfoContext(ctx, "manifest cache: reclaimed",
			"expiredManifests", res.ExpiredManifests,
			"expiredBytes", res.ExpiredBytes,
			"budgetManifests", res.EvictedManifests,
			"budgetBytes", res.EvictedBytes,
			"remainingBytes", res.RemainingBytes)
	}
	return res, nil
}
