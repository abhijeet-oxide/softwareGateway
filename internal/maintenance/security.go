package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// SecurityCacheSweeper keeps the security store inside its budget.
//
// # What this loop is NOT any more
//
// It used to delete every row past an expiry, every fifteen minutes. That is a
// correct cache eviction and it was the wrong policy here: it threw away
// findings nobody had asked it to forget while the counts those findings backed
// lived on forever in `package_security`, so a release ended up with a number
// and an empty table behind it - and the only way back was a twenty-minute sync
// against somebody else's scanner.
//
// So the loop now asks two questions in order. Is anything UNREFERENCED - a
// payload whose scan row is gone, which no read path in the system can reach?
// Those go, always. Is the store OVER ITS BUDGET? Only then does anything else
// go, least recently read first, heavy tiers before light ones. Inside the
// budget nothing is deleted, whatever its age.
//
// LEADER-GATED, like every loop that writes.
type SecurityCacheSweeper struct {
	security *store.Security
	// budget is the ceiling for the regenerable tiers. Zero means no ceiling,
	// which is the default: forgetting is the surprising behaviour and should
	// have to be asked for.
	budget store.CacheBudget
	// packages releases sync claims held by a process that is gone. A release
	// stuck showing "syncing" forever is a release nobody can ever sync again,
	// which is a worse outcome than the rare duplicate a released claim allows.
	packages *store.PackageSecurity
	interval time.Duration
	log      *slog.Logger

	mu     sync.Mutex
	leader bool
}

// DefaultSecuritySweepInterval is how often the store is measured.
//
// Fifteen minutes. Most sweeps now find nothing to do, which is the point: the
// work is measuring, and acting only when the measurement says to.
const DefaultSecuritySweepInterval = 15 * time.Minute

// NewSecurityCacheSweeper builds the loop.
func NewSecurityCacheSweeper(
	security *store.Security, packages *store.PackageSecurity,
	interval time.Duration, budget store.CacheBudget, log *slog.Logger,
) *SecurityCacheSweeper {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultSecuritySweepInterval
	}
	return &SecurityCacheSweeper{
		security: security, packages: packages, interval: interval, budget: budget, log: log,
	}
}

// StaleSyncAfter is how long a sync claim is honoured before it is treated as
// abandoned. The syncer's own bound, rather than a copy of it, so the two
// cannot disagree about what "still running" means.
//
// A claim that has stopped BEATING is abandoned long before this, and the sweep
// picks those up too - see PackageSecurity.ReleaseAbandoned. This bound is for
// the other failure: a sync that is alive and has been going for longer than
// any sync takes.
const StaleSyncAfter = security.StaleClaimAfter

// SetLeader is called by the elector on every leadership change.
func (s *SecurityCacheSweeper) SetLeader(isLeader bool) {
	s.mu.Lock()
	s.leader = isLeader
	s.mu.Unlock()
}

// Enabled reports whether there is a cache to sweep.
func (s *SecurityCacheSweeper) Enabled() bool { return s != nil && s.security != nil }

// Run sweeps on the interval until the context is cancelled.
//
// Never returns an error for a failed sweep, for the same reason the retention
// sweeper does not: the worst outcome of a sweeper that cannot run is a
// database larger than intended, and killing the Coordinator over that would
// turn a disk-usage problem into an outage.
func (s *SecurityCacheSweeper) Run(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}

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
			if err := s.SweepOnce(ctx); err != nil {
				s.log.ErrorContext(ctx, "security cache: sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce runs one sweep.
func (s *SecurityCacheSweeper) SweepOnce(ctx context.Context) error {
	if s.packages != nil {
		released, err := s.packages.ReleaseAbandoned(ctx, StaleSyncAfter)
		if err != nil {
			return err
		}
		if released > 0 {
			s.log.WarnContext(ctx, "security: released abandoned sync claims",
				"releases", released,
				"note", "a Coordinator stopped mid-sync; these can be synced again")
		}
	}

	res, err := s.security.Sweep(ctx, s.budget)
	if err != nil {
		return err
	}
	// Logged only when something happened. A sweep that removes nothing is the
	// steady state - and now the COMMON state - and a line every quarter of an
	// hour saying so is how a log stops being read.
	if res.Freed() {
		s.log.InfoContext(ctx, "security cache: reclaimed space",
			"unreferenced", res.Orphans,
			"details", res.Details, "documents", res.Documents,
			"bytesBefore", res.Before.Bytes(), "bytesAfter", res.After.Bytes(),
			"budget", s.budget.Bytes)
	}
	return nil
}
