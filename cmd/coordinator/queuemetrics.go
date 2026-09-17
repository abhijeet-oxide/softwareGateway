package main

import (
	"context"
	"log/slog"
	"time"

	plog "github.com/abhijeet-oxide/softwareGateway/internal/platform/log"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// How often the queue gauges are refreshed.
//
// Not the scrape interval, and deliberately independent of it: the cost of
// sampling is paid once here however many Prometheus servers are watching,
// which is the whole reason this is a timer rather than a collector. Fifteen
// seconds is fast enough that a queue draining over a minute is visible as a
// slope rather than a step, and slow enough that four small indexed reads are
// nothing next to what the API is doing.
const queueSampleInterval = 15 * time.Second

// How long one sample may take before it is abandoned.
//
// A sample that blocks is worse than a sample that fails. It holds a
// connection from the same pool the API is using, and the thing most likely to
// make it slow - a saturated database - is exactly when the API can least
// afford to lose one. Failing is visible (queue_sample_failures_total) and
// costs nothing; blocking is invisible and costs a connection.
const queueSampleTimeout = 5 * time.Second

// queueSampler refreshes the gauges that describe the product's own work.
//
// See internal/store/queuestats.go for why the queue is READ rather than
// counted, and internal/platform/metrics for what each gauge means.
type queueSampler struct {
	packages *store.Packages
	metrics  *metrics.Registry
	logger   *slog.Logger
	interval time.Duration
	// logs is the log shipper, whose counters ride this same timer. It has no
	// timer of its own and needs none: publishing "how many log lines were
	// dropped" on the interval the other gauges use is one goroutine instead
	// of two, and the number does not need to be fresher than that.
	logs *plog.Shipper
}

func newQueueSampler(p *store.Packages, m *metrics.Registry, l *slog.Logger) *queueSampler {
	return &queueSampler{packages: p, metrics: m, logger: l, interval: queueSampleInterval}
}

// Run samples until the context ends. It never returns an error: a queue gauge
// that could stop the Coordinator would be a monitoring system with the power
// to cause the outage it is watching for.
func (s *queueSampler) Run(ctx context.Context) error {
	// Once immediately, so a freshly started Coordinator does not serve a
	// minute of zeros that read as an empty queue.
	s.sample(ctx)
	s.observeLogs()

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.sample(ctx)
			s.observeLogs()
		}
	}
}

// logShipping is the shipper whose counters are published alongside the queue
// gauges. Nil when shipping is off, which is the default.
func (s *queueSampler) withLogShipper(sh *plog.Shipper) *queueSampler {
	s.logs = sh
	return s
}

func (s *queueSampler) sample(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, queueSampleTimeout)
	defer cancel()

	start := time.Now()
	snap, err := s.packages.QueueSnapshot(ctx)
	s.metrics.QueueSampleDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// The gauges keep their previous values on purpose. Zeroing them
		// would report an empty queue, which is the one reading that must
		// never come from a failure - hence the counter, which is what an
		// alert should watch.
		s.observeFailure(ctx, parent, err)
		return
	}
	observeQueue(s.metrics, snap)
}

// observeLogs publishes the shipper's totals. Separate from the queue sample
// because it must happen even when that sample failed - a database that has
// stopped answering is exactly when the dropped-log count matters.
func (s *queueSampler) observeLogs() {
	sent, dropped, failed := s.logs.Stats()
	s.metrics.ObserveLogShipping(sent, dropped, failed)
}

// observeFailure records a sample that did not complete.
//
// PARENT decides whether to log, not the timeout context derived from it.
// Asking the derived one silences the timeout as well - which is the single
// failure most worth a line, because it means the sample could not get a
// database connection inside its budget and every gauge below is now stale.
// Only a shutdown is expected, and only a shutdown is quiet.
func (s *queueSampler) observeFailure(ctx, parent context.Context, err error) {
	s.metrics.QueueSampleFailures.Inc()
	if parent.Err() != nil {
		return
	}
	s.logger.Warn("queue sample failed",
		"error", err,
		"timedOut", ctx.Err() != nil,
		"timeout", queueSampleTimeout)
}

// observeQueue writes one snapshot into the registry.
//
// Reset before Set for every vector, so a state that has emptied since the
// last pass goes to zero rather than keeping its last value forever. Without
// it, the last worker to drain would leave `workers{state="active"}` reading 1
// for the life of the process.
func observeQueue(m *metrics.Registry, s store.QueueSnapshot) {
	m.QueueJobs.Reset()
	for state, n := range s.Jobs {
		m.QueueJobs.WithLabelValues(state).Set(float64(n))
	}
	m.QueueJobsPaused.Set(float64(s.JobsPaused))
	m.QueueOldestPending.Set(s.OldestPendingSeconds)

	m.QueueBytes.Reset()
	m.QueueBytes.WithLabelValues("pending").Set(float64(s.PendingBytes))
	m.QueueBytes.WithLabelValues("in_flight").Set(float64(s.InFlightBytes))

	m.QueueTransfers.Reset()
	for state, n := range s.Transfers {
		m.QueueTransfers.WithLabelValues(state).Set(float64(n))
	}

	m.Workers.Reset()
	for state, n := range s.Workers {
		m.Workers.WithLabelValues(state).Set(float64(n))
	}

	m.WorkerSlots.Reset()
	m.WorkerSlots.WithLabelValues("active").Set(float64(s.SlotsActive))
	m.WorkerSlots.WithLabelValues("granted").Set(float64(s.SlotsGranted))
	m.WorkerSlots.WithLabelValues("max").Set(float64(s.SlotsMax))

	m.DatabaseBytes.Set(float64(s.DatabaseBytes))
}
