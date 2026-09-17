package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// AvailabilityRecorder writes down that this replica is serving.
//
// # Why the product records its own availability at all
//
// Because "was it up this morning?" is the first question anybody asks about a
// service, and until this loop existed nothing in the product could answer it.
// A metrics stack holds `up` for whoever has a dashboard open and knows the
// query; the Overview page - where somebody actually looks - had nothing, and
// the one availability claim the interface made came from a browser inferring
// an outage from a single endpoint's error. That inference was wrong, loudly
// and repeatedly, which is what an interface does when it has to guess at a
// fact nobody is recording.
//
// # NOT LEADER-GATED, and that is the point
//
// Every other loop in this package writes once for the estate and is gated on
// leadership. This one is the opposite: each replica records ITS OWN runs,
// because the fact being recorded is that THIS process was serving. A gated
// recorder would go quiet the moment the leader died - the exact event it
// exists to capture - and the follower that took over would have no record of
// the gap.
//
// The write is tiny and idempotent per replica: one UPDATE per beat, one
// INSERT per restart. See internal/store/availability.go.
type AvailabilityRecorder struct {
	availability *store.Availability
	health       *health.Registry
	component    string
	instance     string
	version      string
	interval     time.Duration
	keep         time.Duration
	log          *slog.Logger

	mu     sync.Mutex
	leader bool
}

// NewAvailabilityRecorder builds the loop.
//
// `instance` must be stable for the life of the process and different for each
// replica - a pod name, or a generated id. Two replicas sharing one would
// interleave their beats into a single run and report a rolling restart as
// uninterrupted service, which is the one claim this must never make falsely.
func NewAvailabilityRecorder(
	availability *store.Availability,
	reg *health.Registry,
	component, instance, version string,
	interval time.Duration,
	log *slog.Logger,
) *AvailabilityRecorder {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = store.DefaultAvailabilityInterval
	}
	return &AvailabilityRecorder{
		availability: availability,
		health:       reg,
		component:    component,
		instance:     instance,
		version:      version,
		interval:     interval,
		keep:         store.DefaultAvailabilityRetention,
		log:          log,
	}
}

// SetLeader is called by the elector on every leadership change.
//
// The recording is not gated on it; the PRUNING is. One replica trimming the
// history is enough, and several doing it at once is a write storm to delete
// rows that are already gone.
func (r *AvailabilityRecorder) SetLeader(isLeader bool) {
	r.mu.Lock()
	r.leader = isLeader
	r.mu.Unlock()
}

// Enabled reports whether there is anywhere to record.
func (r *AvailabilityRecorder) Enabled() bool { return r != nil && r.availability != nil }

// Run beats until the context is cancelled.
//
// A failed beat is logged and nothing else. The availability record is a record
// ABOUT the service, and a service that stopped serving because it could not
// write its own diary would be a joke at the expense of the people using it.
func (r *AvailabilityRecorder) Run(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}

	// THE FIRST BEAT IS IMMEDIATE. Waiting a tick would leave the interval
	// between starting up and the first write looking, to a later reader,
	// exactly like the tail of the outage that preceded it - so a restart
	// would always appear to have lasted one beat longer than it did.
	r.beat(ctx)

	// No `component` key: the logger this is handed already carries one, and a
	// line with the same key twice is a line a log query cannot filter on.
	r.log.Info("availability: recording",
		"instance", r.instance, "interval", r.interval)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	prune := time.NewTicker(time.Hour)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			// Deliberately no closing write. A clean shutdown and a crash are
			// the same thing to somebody who could not reach the service, and
			// marking one of them tidily would make the record kinder to this
			// process than to its users.
			return nil
		case <-ticker.C:
			r.beat(ctx)
		case <-prune.C:
			r.mu.Lock()
			isLeader := r.leader
			r.mu.Unlock()
			if !isLeader {
				continue
			}
			if n, err := r.availability.Prune(ctx, r.component, r.keep); err != nil {
				r.log.WarnContext(ctx, "availability: could not prune the record", "error", err)
			} else if n > 0 {
				r.log.InfoContext(ctx, "availability: pruned old runs", "runs", n)
			}
		}
	}
}

// beat records one interval, at whatever status the health registry reports.
func (r *AvailabilityRecorder) beat(ctx context.Context) {
	status := store.AvailabilityHealthy
	if r.health != nil {
		// READY, not deep. The question is whether this replica is serving
		// requests, and a deep check makes outbound calls to registries and
		// scanners: a vendor being slow is not this service being unavailable,
		// and a beat that waited on one would be late for reasons that have
		// nothing to do with the fact it is recording.
		if rep := r.health.Ready(ctx); rep.Status != health.StatusHealthy {
			status = store.AvailabilityDegraded
		}
	}

	// A short deadline of its own. This runs on a ticker forever, and a beat
	// that inherited a stuck database's wait would pile up goroutines behind a
	// dependency the record is trying to describe.
	beatCtx, cancel := context.WithTimeout(ctx, r.interval)
	defer cancel()

	started, err := r.availability.Beat(
		beatCtx, r.component, r.instance, r.version, status, store.AvailabilityContinuity)
	if err != nil {
		// Warn, not error: the database being unreachable is already reported
		// by everything that matters, and a failed beat is a fact about the
		// record rather than about a request somebody made.
		r.log.WarnContext(ctx, "availability: could not record this interval", "error", err)
		return
	}
	if started {
		r.log.InfoContext(ctx, "availability: started a new run",
			"instance", r.instance, "status", string(status))
	}
}
