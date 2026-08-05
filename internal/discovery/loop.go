package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/backoff"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// maxBackoffMultiple caps the backoff at four times the configured interval.
//
// Bounded deliberately: an unbounded backoff means a source that failed
// overnight is still waiting hours after the registry recovered, and the first
// symptom an operator sees is "it eventually noticed". Four intervals is long
// enough to stop hammering a dead registry and short enough to recover
// promptly.
const maxBackoffMultiple = 4

// Loop runs discovery for every enabled source.
//
// One goroutine per source repository, each on its own interval. Per-repository
// rather than one global loop, because a slow or unreachable vendor must not
// delay every other vendor — a single loop iterating all sources would make one
// dead registry a fleet-wide discovery stall, which is the exact failure that
// turns a vendor's bad afternoon into ours (docs/design/07 §1).
type Loop struct {
	log     *slog.Logger
	metrics *metrics.Registry

	mu      sync.Mutex
	workers map[string]*worker
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewLoop builds a discovery loop.
func NewLoop(log *slog.Logger, m *metrics.Registry) *Loop {
	if log == nil {
		log = slog.Default()
	}
	return &Loop{log: log, metrics: m, workers: map[string]*worker{}}
}

// SourceSpec describes one source to poll.
type SourceSpec struct {
	Product      *product.Product
	ProductID    int64
	SourceName   string
	SourceRepoID int64
	RepoIDs      map[string]int64
	// Client is the registry client for this source. Built by the caller,
	// which owns credential resolution.
	Client   registry.Source
	Interval time.Duration
}

// key identifies a worker. Product plus source name, because a source name is
// only unique within its product.
func (s SourceSpec) key() string { return s.Product.Metadata.Name + "/" + s.SourceName }

// worker polls one source.
type worker struct {
	spec    SourceSpec
	scanner *Scanner
	log     *slog.Logger
	metrics *metrics.Registry

	// trigger carries manual scan requests. Buffered to depth one, which is
	// what collapses concurrent triggers: a second :discover arriving while a
	// scan runs finds the slot occupied and returns the in-progress scan rather
	// than queueing a redundant one (docs/design/07 §8).
	trigger chan chan ScanResult

	mu      sync.Mutex
	last    ScanResult
	lastErr error
	lastRun time.Time
}

// Start begins polling. Call only on the leader.
//
// Discovery writes to the database and creates transfer requests; running it on
// every replica would multiply vendor load by the replica count and race every
// insert against itself. The unique constraints would hold, but the wasted
// requests would not be free (docs/design/04 §9).
func (l *Loop) Start(ctx context.Context, specs []SourceSpec, packages *store.Packages) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.running {
		return errors.New("discovery loop is already running")
	}

	// Every scanner is built BEFORE any goroutine starts, so a bad pattern in
	// one product's configuration fails the whole start rather than leaving
	// some sources polling and others silently absent.
	built := make(map[string]*worker, len(specs))
	for _, spec := range specs {
		scanner, err := NewScanner(ScannerConfig{
			Source:       spec.Client,
			Packages:     packages,
			Logger:       l.log,
			Product:      spec.Product,
			ProductID:    spec.ProductID,
			SourceName:   spec.SourceName,
			SourceRepoID: spec.SourceRepoID,
			RepoIDs:      spec.RepoIDs,
		})
		if err != nil {
			return err
		}
		built[spec.key()] = &worker{
			spec:    spec,
			scanner: scanner,
			log:     l.log.With("product", spec.Product.Metadata.Name, "source", spec.SourceName),
			metrics: l.metrics,
			trigger: make(chan chan ScanResult, 1),
		}
	}

	loopCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.workers = built
	l.running = true

	for _, w := range built {
		l.wg.Add(1)
		go func(w *worker) {
			defer l.wg.Done()
			w.run(loopCtx)
		}(w)
	}

	l.log.InfoContext(ctx, "discovery started", "sources", len(built))
	return nil
}

// Stop halts polling and waits for in-flight scans.
//
// Waiting matters: a scan killed mid-flight would be safe — the next scan is a
// full scan and there is nothing to recover — but waiting means shutdown does
// not leave a half-written package transaction to be rolled back by a timeout.
func (l *Loop) Stop() {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return
	}
	cancel := l.cancel
	l.running = false
	l.workers = map[string]*worker{}
	l.mu.Unlock()

	cancel()
	l.wg.Wait()
	l.log.Info("discovery stopped")
}

// Running reports whether the loop is polling.
func (l *Loop) Running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// ErrNoSuchSource reports that no worker polls the requested source.
var ErrNoSuchSource = errors.New("no discovery source")

// Trigger runs an immediate scan of one source, bypassing the interval.
//
// Idempotent and safe: it is the same scan the loop runs. Concurrent triggers
// are COLLAPSED — a scan already running for that source returns that scan's
// result rather than starting a second one, so a dashboard refresh loop cannot
// turn into a stampede against the vendor (docs/design/07 §8).
func (l *Loop) Trigger(ctx context.Context, productName, sourceName string) (ScanResult, error) {
	l.mu.Lock()
	w, ok := l.workers[productName+"/"+sourceName]
	l.mu.Unlock()

	if !ok {
		return ScanResult{}, fmt.Errorf("%w for product %q source %q",
			ErrNoSuchSource, productName, sourceName)
	}
	return w.triggerScan(ctx)
}

// TriggerProduct scans every source of one product.
func (l *Loop) TriggerProduct(ctx context.Context, productName string) (ScanResult, error) {
	l.mu.Lock()
	var targets []*worker
	for _, w := range l.workers {
		if w.spec.Product.Metadata.Name == productName {
			targets = append(targets, w)
		}
	}
	l.mu.Unlock()

	if len(targets) == 0 {
		return ScanResult{}, fmt.Errorf("%w for product %q", ErrNoSuchSource, productName)
	}

	var combined ScanResult
	var firstErr error
	for _, w := range targets {
		res, err := w.triggerScan(ctx)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		combined.TagsListed += res.TagsListed
		combined.TagsAdmitted += res.TagsAdmitted
		combined.New += res.New
		combined.Superseded += res.Superseded
		combined.Requests += res.Requests
		combined.TagErrors = append(combined.TagErrors, res.TagErrors...)
		combined.Duration += res.Duration
	}
	return combined, firstErr
}

// Status reports the last scan of every source.
type Status struct {
	Product  string
	Source   string
	LastRun  time.Time
	Last     ScanResult
	LastErr  string
	Interval time.Duration
}

// Status returns a snapshot for the health and status surfaces.
func (l *Loop) Status() []Status {
	l.mu.Lock()
	workers := make([]*worker, 0, len(l.workers))
	for _, w := range l.workers {
		workers = append(workers, w)
	}
	l.mu.Unlock()

	out := make([]Status, 0, len(workers))
	for _, w := range workers {
		w.mu.Lock()
		s := Status{
			Product:  w.spec.Product.Metadata.Name,
			Source:   w.spec.SourceName,
			LastRun:  w.lastRun,
			Last:     w.last,
			Interval: w.spec.Interval,
		}
		if w.lastErr != nil {
			s.LastErr = w.lastErr.Error()
		}
		w.mu.Unlock()
		out = append(out, s)
	}
	return out
}

// run is one source's polling loop.
func (w *worker) run(ctx context.Context) {
	interval := w.spec.Interval
	if interval <= 0 {
		interval = product.DefaultDiscoveryInterval
	}

	// Backoff is applied to the WAIT, never by disabling the source. A vendor
	// outage must not require human re-enablement afterwards — a source that
	// turned itself off is a source nobody remembers to turn back on
	// (docs/design/07 §7).
	policy := backoff.Policy{Base: interval, Cap: interval * maxBackoffMultiple}
	failures := 0

	timer := time.NewTimer(0) // scan once at startup rather than waiting a full interval
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case reply := <-w.trigger:
			res, err := w.scanOnce(ctx)
			if err == nil {
				failures = 0
			}
			// A manual scan resets the schedule: having just scanned, the next
			// automatic one is a full interval away.
			resetTimer(timer, interval)
			select {
			case reply <- res:
			default:
			}

		case <-timer.C:
			_, err := w.scanOnce(ctx)
			if err != nil {
				failures++
				wait := policy.Delay(failures - 1)
				w.log.WarnContext(ctx, "discovery scan failed, backing off",
					"error", err, "consecutiveFailures", failures, "retryIn", wait)
				resetTimer(timer, wait)
				continue
			}
			failures = 0
			resetTimer(timer, interval)
		}
	}
}

// resetTimer safely restarts a timer that has already fired.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// triggerScan asks the worker to scan now and waits for the result.
//
// If the trigger slot is already occupied, this call joins that pending scan
// rather than queueing another.
func (w *worker) triggerScan(ctx context.Context) (ScanResult, error) {
	reply := make(chan ScanResult, 1)

	select {
	case w.trigger <- reply:
	case <-ctx.Done():
		return ScanResult{}, ctx.Err()
	default:
		// A scan is already pending or running. Report its last known result
		// rather than starting a second one.
		w.mu.Lock()
		last, err := w.last, w.lastErr
		w.mu.Unlock()
		return last, err
	}

	select {
	case res := <-reply:
		w.mu.Lock()
		err := w.lastErr
		w.mu.Unlock()
		return res, err
	case <-ctx.Done():
		return ScanResult{}, ctx.Err()
	}
}

// scanOnce runs one scan and records its outcome.
func (w *worker) scanOnce(ctx context.Context) (ScanResult, error) {
	productName := w.spec.Product.Metadata.Name
	res, err := w.scanner.Scan(ctx)

	w.mu.Lock()
	w.last = res
	w.lastErr = err
	w.lastRun = time.Now()
	w.mu.Unlock()

	if w.metrics != nil {
		w.metrics.DiscoveryDuration.WithLabelValues(productName, w.spec.SourceName).
			Observe(res.Duration.Seconds())
	}

	if err != nil {
		if w.metrics != nil {
			w.metrics.DiscoveryScans.WithLabelValues(productName, w.spec.SourceName, "failure").Inc()
			w.metrics.DiscoveryErrors.WithLabelValues(
				productName, w.spec.SourceName, string(registry.ClassOf(err))).Inc()
		}
		return res, err
	}

	if w.metrics != nil {
		w.metrics.DiscoveryScans.WithLabelValues(productName, w.spec.SourceName, "success").Inc()
		w.metrics.DiscoveryLastSuccess.WithLabelValues(productName, w.spec.SourceName).
			Set(float64(time.Now().Unix()))
		if res.New > 0 {
			w.metrics.DiscoveryPackages.WithLabelValues(productName, w.spec.SourceName).
				Add(float64(res.New))
		}
		// Per-tag failures did not stop the scan, but they are still failures
		// and must not be invisible just because the scan "succeeded".
		for range res.TagErrors {
			w.metrics.DiscoveryErrors.WithLabelValues(productName, w.spec.SourceName, "tag").Inc()
		}
	}

	w.log.DebugContext(ctx, "discovery scan complete",
		"tags", res.TagsListed, "admitted", res.TagsAdmitted,
		"new", res.New, "superseded", res.Superseded, "requests", res.Requests,
		"tagErrors", len(res.TagErrors), "duration", res.Duration)

	return res, nil
}
