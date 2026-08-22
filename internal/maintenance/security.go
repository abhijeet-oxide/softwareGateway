package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// SecurityCacheSweeper removes expired security cache rows.
//
// # Why expiry needs a sweeper at all, when every read already filters on it
//
// Because the filter answers correctness and the sweeper answers size. A read
// that served an expired row would be the bug; a read that DELETED one would
// turn every page render into a write transaction, and the detail tier expires
// in minutes, so that is a write per artifact per quarter of an hour on a
// database whose sole writer is the Coordinator.
//
// So the rows are filtered on read and removed here. Without this loop the
// complete-response tier - the largest thing this feature stores, and the one
// deliberately given the shortest life - would accumulate forever while never
// being served, which is the worst of both.
//
// LEADER-GATED, like every loop that writes.
type SecurityCacheSweeper struct {
	security *store.Security
	// packages releases sync claims held by a process that is gone. A release
	// stuck showing "syncing" forever is a release nobody can ever sync again,
	// which is a worse outcome than the rare duplicate a released claim allows.
	packages *store.PackageSecurity
	interval time.Duration
	log      *slog.Logger

	mu     sync.Mutex
	leader bool
}

// DefaultSecuritySweepInterval is how often expired rows are collected.
//
// Fifteen minutes, which is the default detail retention: sweeping much more
// often finds nothing, and much less often lets a quarter-hour cache occupy
// disk for hours after it stopped being readable.
const DefaultSecuritySweepInterval = 15 * time.Minute

// NewSecurityCacheSweeper builds the loop.
func NewSecurityCacheSweeper(
	security *store.Security, packages *store.PackageSecurity,
	interval time.Duration, log *slog.Logger,
) *SecurityCacheSweeper {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultSecuritySweepInterval
	}
	return &SecurityCacheSweeper{
		security: security, packages: packages, interval: interval, log: log,
	}
}

// StaleSyncAfter is how long a sync claim is honoured before it is treated as
// abandoned. Matches the syncer's own bound, so the two cannot disagree about
// what "still running" means.
const StaleSyncAfter = 30 * time.Minute

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

	scans, details, err := s.security.Sweep(ctx)
	if err != nil {
		return err
	}
	// Logged only when something happened. A sweep that removes nothing is the
	// steady state, and a line every quarter of an hour saying so is how a log
	// stops being read.
	if scans > 0 || details > 0 {
		s.log.InfoContext(ctx, "security cache: removed expired rows",
			"summaries", scans, "details", details)
	}
	return nil
}
