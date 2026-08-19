package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/backoff"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
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
//
// A source covers one registry and one or more repositories on it, so the
// caller supplies a client FACTORY rather than a client: the repository set is
// not known until a scan resolves it, and catalog enumeration can return
// repositories that did not exist when the loop started.
type SourceSpec struct {
	Product    *product.Product
	ProductID  int64
	SourceName string
	// RepoIDs maps configured repository names to catalog row IDs, for
	// auto-download target resolution.
	RepoIDs map[string]int64
	// NewClient builds a client for one repository on this source's registry.
	NewClient ClientFactory
	// Catalog enumerates the registry. Nil when repositoryDiscovery is off.
	Catalog  CatalogLister
	Interval time.Duration

	// Layout groups this source's tags into packages. Nil means the standard
	// layout: one tag, one package.
	Layout vendors.Layout

	// InsecureTLS records that this source's clients do not verify certificates.
	//
	// Carried on the spec purely so the controller can say so out loud at every
	// reconcile. A security downgrade that is only visible by reading the
	// ConfigMap is one that outlives the incident it was added for.
	InsecureTLS bool
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

	// trigger wakes the polling loop for a manual scan. It carries no payload:
	// the scan itself is claimed through inflight, so a wake-up that arrives
	// after someone else already started the scan is simply a no-op.
	trigger chan struct{}

	// ctx is the loop's context, held so a scan is never bound to the lifetime
	// of the HTTP request that asked for it. A caller giving up must stop the
	// caller waiting, not the scan — other callers may be joined to it, and the
	// scheduled loop certainly is.
	ctx context.Context

	mu sync.Mutex
	// inflight is the scan currently running, if any. It is what makes
	// concurrent triggers COLLAPSE: a second :discover joins this one and gets
	// its real result (docs/design/07 §8).
	//
	// This used to be a one-deep buffered channel, and it did not work. A
	// trigger that found the slot occupied returned w.last — the PREVIOUS
	// scan's result, or the zero value when no scan had completed yet — with no
	// error and no indication that nothing had run. The caller saw a successful
	// scan of zero repositories in 0ms, and `packages discover` printed
	// "Nothing new. A scan that finds nothing is the normal steady state, not a
	// failure." It was not a steady state. No scan had happened at all.
	inflight *scanCall

	last    ScanResult
	lastErr error
	lastRun time.Time
}

// scanCall is one execution of a scan, shared by everyone waiting on it.
type scanCall struct {
	done chan struct{}
	// running distinguishes "claimed by a trigger, not yet started" from
	// "executing". Without it the polling loop cannot tell a claim it should
	// pick up from a scan it should leave alone, and a trigger that claimed a
	// scan nobody executes waits forever.
	running bool // guarded by worker.mu

	res ScanResult
	err error
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
			Packages:   packages,
			Logger:     l.log,
			Product:    spec.Product,
			ProductID:  spec.ProductID,
			SourceName: spec.SourceName,
			NewClient:  spec.NewClient,
			Catalog:    spec.Catalog,
			RepoIDs:    spec.RepoIDs,
			Layout:     spec.Layout,
		})
		if err != nil {
			return err
		}
		built[spec.key()] = &worker{
			spec:    spec,
			scanner: scanner,
			log:     l.log.With("product", spec.Product.Metadata.Name, "source", spec.SourceName),
			metrics: l.metrics,
			trigger: make(chan struct{}, 1),
		}
	}

	loopCtx, cancel := context.WithCancel(ctx)

	// Set before the workers become reachable through l.workers, so a :discover
	// arriving in the first milliseconds cannot find a worker with no context
	// and no way to observe shutdown.
	for _, w := range built {
		w.ctx = loopCtx
	}

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
		combined.Repositories += res.Repositories
		combined.RepositoriesFromCatalog += res.RepositoriesFromCatalog
		combined.RepositoriesFiltered += res.RepositoriesFiltered
		combined.TagsListed += res.TagsListed
		combined.TagsAdmitted += res.TagsAdmitted
		combined.New += res.New
		combined.Superseded += res.Superseded
		combined.Requests += res.Requests
		combined.TagErrors = append(combined.TagErrors, res.TagErrors...)
		combined.RepositoryErrors = append(combined.RepositoryErrors, res.RepositoryErrors...)
		combined.Duration += res.Duration
		// Any source that was joined rather than started makes the whole
		// response a partly-collapsed one. Reported pessimistically: claiming a
		// fresh scan when part of it was not is the error that misleads.
		combined.Collapsed = combined.Collapsed || res.Collapsed
	}
	return combined, firstErr
}

// Products lists the product names the loop is polling, sorted.
//
// Needed by a global scan, which must know what "everything" is without the
// caller enumerating configuration a second time and disagreeing about which
// products are enabled.
func (l *Loop) Products() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	seen := map[string]bool{}
	for _, w := range l.workers {
		seen[w.spec.Product.Metadata.Name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// StartProduct begins a scan of every source of one product WITHOUT waiting.
//
// The counterpart to TriggerProduct, for a caller that wants the scan to happen
// but does not want to hold an HTTP request open for the minutes a slow
// registry can take. Progress is then read from Progress, and the outcome from
// Status.
//
// Returns how many sources it started and how many were already scanning, so
// the caller can say which rather than implying a fresh scan of everything.
func (l *Loop) StartProduct(productName string) (started, alreadyRunning int, err error) {
	l.mu.Lock()
	var targets []*worker
	for _, w := range l.workers {
		if w.spec.Product.Metadata.Name == productName {
			targets = append(targets, w)
		}
	}
	l.mu.Unlock()

	if len(targets) == 0 {
		return 0, 0, fmt.Errorf("%w for product %q", ErrNoSuchSource, productName)
	}

	for _, w := range targets {
		if w.startScan() {
			started++
		} else {
			alreadyRunning++
		}
	}
	return started, alreadyRunning, nil
}

// ErrNotInspectable reports that no running source can reach a package.
var ErrNotInspectable = errors.New("no source can reach this package")

// InspectPackage expands one package's manifest tree on demand.
//
// Routed through the loop rather than given the API its own registry client, so
// it uses the SAME per-source stack discovery uses: one connection pool, one
// rate limiter, one cached token, the configured proxy and CA. An inspect that
// opened its own connections would be invisible to the ceilings the operator
// set, which is the bug that made scans slow in the first place.
func (l *Loop) InspectPackage(
	ctx context.Context, packages *store.Packages, pkg store.PackageRow, productName string,
) (InspectResult, error) {
	if pkg.SourceRepository == "" {
		return InspectResult{}, fmt.Errorf(
			"%w: package %d has no source repository recorded", ErrNotInspectable, pkg.ID)
	}

	l.mu.Lock()
	var found *worker
	for _, w := range l.workers {
		if w.spec.Product.Metadata.Name != productName {
			continue
		}
		// The first source of the product that can build a client for this
		// repository path. A product's sources are distinct registries, and a
		// package belongs to exactly one of them.
		found = w
		break
	}
	l.mu.Unlock()

	if found == nil {
		return InspectResult{}, fmt.Errorf(
			"%w: no source of product %q is running; discovery runs on the leader",
			ErrNotInspectable, productName)
	}

	client, err := found.scanner.clientFor(pkg.SourceRepository)
	if err != nil {
		return InspectResult{}, fmt.Errorf("build client for %s: %w", pkg.SourceRepository, err)
	}

	// The same ceiling a scan uses, from the source this package came from.
	return InspectPackage(ctx, packages, pkg, client, found.scanner.sourceCfg.Concurrency.PerRegistry)
}

// Progress reports the live state of every source of one product.
//
// Cheap and safe to poll: it reads a snapshot behind a mutex the scan holds
// only long enough to bump a counter, never across a network call.
func (l *Loop) Progress(productName string) []SourceProgress {
	l.mu.Lock()
	workers := make([]*worker, 0, len(l.workers))
	for _, w := range l.workers {
		if productName == "" || w.spec.Product.Metadata.Name == productName {
			workers = append(workers, w)
		}
	}
	l.mu.Unlock()

	out := make([]SourceProgress, 0, len(workers))
	for _, w := range workers {
		sp := SourceProgress{
			Product:  w.spec.Product.Metadata.Name,
			Source:   w.spec.SourceName,
			Progress: w.scanner.Progress(),
			Interval: w.spec.Interval,
		}

		w.mu.Lock()
		sp.LastRun = w.lastRun
		sp.Last = w.last
		if w.lastErr != nil {
			sp.LastErr = w.lastErr.Error()
		}
		w.mu.Unlock()

		out = append(out, sp)
	}

	sort.Slice(out, func(a, b int) bool {
		if out[a].Product != out[b].Product {
			return out[a].Product < out[b].Product
		}
		return out[a].Source < out[b].Source
	})
	return out
}

// SourceProgress is one source's live and last-completed state.
type SourceProgress struct {
	Product  string
	Source   string
	Progress ScanProgress
	Interval time.Duration

	LastRun time.Time
	Last    ScanResult
	LastErr string
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
	// Beside the loop, with the loop's own lifetime: a walk started by one scan
	// must be able to outlive that scan, and must stop when the loop does.
	go w.scanner.runAnalyser(ctx)

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

		case <-w.trigger:
			// A wake-up, not a request: the caller already registered its claim.
			// So the token can be STALE — if the interval timer fired at the same
			// moment, that arm picked the claim up and ran it, and this token is
			// left over. Acting on it anyway would start a second, redundant scan
			// of the same source moments after the first, which is exactly the
			// duplicate work the collapse exists to prevent.
			if !w.hasPendingClaim() {
				continue
			}
			_, err := w.scan(ctx)
			if err == nil {
				failures = 0
			}
			// A manual scan resets the schedule: having just scanned, the next
			// automatic one is a full interval away.
			resetTimer(timer, interval)

		case <-timer.C:
			_, err := w.scan(ctx)
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
// If a scan is already running — whether started by the interval or by another
// caller — this joins it and returns THAT scan's result, marked Collapsed. It
// never starts a second concurrent scan against the same source, and it never
// returns a result from a scan that has already finished: joining a running
// scan gives fresh data, reporting a previous one does not.
func (w *worker) triggerScan(ctx context.Context) (ScanResult, error) {
	call, mine := w.joinScan()

	if mine {
		// Wake the polling loop, which owns execution so that a scan is always
		// on the same goroutine as the backoff and the schedule. Buffered to
		// depth one and non-blocking: the claim is what matters, and a wake-up
		// that finds the slot full means the loop is already about to look.
		select {
		case w.trigger <- struct{}{}:
		default:
		}
	}

	select {
	case <-call.done:
		res := call.res
		res.Collapsed = !mine
		return res, call.err

	case <-ctx.Done():
		// The caller gave up. The scan continues — it is the loop's work now,
		// other callers may be waiting on it, and abandoning it would waste the
		// registry round trips already spent.
		return ScanResult{}, ctx.Err()

	case <-w.loopDone():
		// The loop stopped, most often because leadership was lost. Say so
		// rather than waiting on a scan that will never run.
		return ScanResult{}, fmt.Errorf("%w: discovery stopped while the scan was pending",
			ErrNoSuchSource)
	}
}

// startScan registers a scan and returns without waiting for it.
//
// Reports true when it started one, false when a scan was already running and
// this is therefore a no-op — which the caller must be told, or "started" would
// be a claim it cannot back up.
func (w *worker) startScan() bool {
	_, mine := w.joinScan()
	if !mine {
		return false
	}
	select {
	case w.trigger <- struct{}{}:
	default:
	}
	return true
}

// hasPendingClaim reports whether a scan has been registered and not yet
// started — the only condition under which a wake-up token is still meaningful.
func (w *worker) hasPendingClaim() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inflight != nil && !w.inflight.running
}

// joinScan is the caller-side claim: join a scan in progress, or register one
// for the polling loop to run.
//
// It deliberately does NOT mark the call running. Execution belongs to the loop
// goroutine, so that a scan is always on the same goroutine as the backoff
// counter and the interval timer — the alternative is a scan whose failure the
// schedule never learns about.
func (w *worker) joinScan() (call *scanCall, mine bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.inflight != nil {
		return w.inflight, false
	}
	c := &scanCall{done: make(chan struct{})}
	w.inflight = c
	return c, true
}

// takeScan is the loop-side claim: pick up a scan a trigger registered, or
// start a fresh one, or report that one is already executing.
func (w *worker) takeScan() (call *scanCall, run bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.inflight != nil {
		if w.inflight.running {
			return w.inflight, false
		}
		// Registered by a trigger and waiting for someone to run it.
		w.inflight.running = true
		return w.inflight, true
	}

	c := &scanCall{done: make(chan struct{}), running: true}
	w.inflight = c
	return c, true
}

// scan runs a scan, or waits for the one already executing.
//
// Called from the polling loop for both the scheduled and the triggered case,
// so there is one path and one set of semantics rather than two that drift.
func (w *worker) scan(ctx context.Context) (ScanResult, error) {
	call, run := w.takeScan()
	if !run {
		// Already executing. Waiting rather than starting a second concurrent
		// scan of the same source: two racing scans would double the vendor's
		// load and contend on every insert, and the second would find exactly
		// what the first is already finding.
		select {
		case <-call.done:
			return call.res, call.err
		case <-ctx.Done():
			return ScanResult{}, ctx.Err()
		}
	}

	call.res, call.err = w.scanOnce(ctx)

	w.mu.Lock()
	w.inflight = nil
	w.mu.Unlock()
	close(call.done)

	return call.res, call.err
}

// loopDone reports the loop's shutdown channel, tolerating a worker whose
// context has not been set yet.
func (w *worker) loopDone() <-chan struct{} {
	if w.ctx == nil {
		return nil // nil channel: blocks forever, which is the right no-op here
	}
	return w.ctx.Done()
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
		for range res.RepositoryErrors {
			w.metrics.DiscoveryErrors.WithLabelValues(productName, w.spec.SourceName, "repository").Inc()
		}
	}

	// Walk what was just found, before anybody asks for it — WITHOUT WAITING
	// FOR IT. This used to run inline, which made a scan as slow as the
	// walking: a scan that found ten releases took as long as walking ten
	// manifest trees and reported nothing during it. The analyser runs beside
	// the loop and says what it is doing in each release's own state.
	w.scanner.wakeAnalyser()

	w.log.DebugContext(ctx, "discovery scan complete",
		"repositories", res.Repositories, "fromCatalog", res.RepositoriesFromCatalog,
		"tags", res.TagsListed, "admitted", res.TagsAdmitted,
		"new", res.New, "superseded", res.Superseded, "requests", res.Requests,
		"tagErrors", len(res.TagErrors), "duration", res.Duration)

	return res, nil
}
