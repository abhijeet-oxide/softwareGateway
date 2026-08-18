package replication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The watcher settles delegated transfers.
//
// It exists because the registry is under no obligation to finish while we are
// looking. A mirror that completes overnight, during a Coordinator restart,
// must still settle correctly in the morning — so the outcome is derived from
// polling persisted state rather than from a goroutine that happened to be
// waiting when the sync ended.
//
// Two questions per tick, and they are genuinely different:
//
//	1. What does the registry SAY?   — its sync status
//	2. What is actually THERE?       — a walk of the destination
//
// Only the second can distinguish `succeeded` from `diverged`, which is why a
// completed sync is never settled on the registry's word alone. See
// docs/design/18 section 6.2.

// DestinationReader resolves a tag at a configured target.
//
// A consumer-defined interface: the watcher needs one lookup, not the client
// factory, the credential resolution and the transport tuning behind it.
type DestinationReader interface {
	// ResolveAtTarget returns the manifest digest a tag currently points at in
	// a target repository, or registry.ErrNotFound when the tag is absent.
	ResolveAtTarget(ctx context.Context, productName, targetName, tag string) (string, error)
}

// SyncRecorder is the persistence a watcher tick needs.
//
// A consumer-defined interface rather than *store.Replication, for the same
// reason the API defines Replicator: the watcher uses three calls, and taking
// the whole store would make every test of a DECISION need a database.
type SyncRecorder interface {
	OpenSyncs(ctx context.Context, limit int) ([]store.OpenSync, error)
	UpdateSync(ctx context.Context, id int64, s store.MirrorSync) error
	SettleDelegated(ctx context.Context, transferID, state, reason string) error
}

// Products supplies the loaded configuration a watcher tick needs.
type Products interface {
	Get(name string) (*product.Product, bool)
}

// Watcher polls delegated transfers to a terminal state.
type Watcher struct {
	service  *Service
	store    SyncRecorder
	products Products
	dest     DestinationReader
	log      *slog.Logger

	auditor Auditor
	metrics Metrics

	mu     sync.Mutex
	leader bool

	// batch bounds one tick, so a backlog does not hold the leader in a loop.
	batch int
	// interval is how often the registry is polled. Not aggressive: a mirror
	// sync takes minutes to hours, and polling somebody else's control API
	// every second would be rude for no information.
	interval time.Duration
	// timeout is how long a sync may stay unsettled before it is failed.
	//
	// Not optional, and not generous. Without it a mirror the registry quietly
	// forgot leaves a transfer in `syncing` forever — the one delegated
	// failure mode with no error anywhere to notice it by.
	timeout time.Duration
}

// Watcher pacing.
const (
	// DefaultSyncTimeout bounds an unsettled sync.
	DefaultSyncTimeout = 6 * time.Hour
	// DefaultWatchInterval is how often the registry is polled.
	DefaultWatchInterval = 30 * time.Second
)

// NewWatcher builds one.
func NewWatcher(svc *Service, st SyncRecorder, products Products, dest DestinationReader, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		service: svc, store: st, products: products, dest: dest, log: log,
		batch: 25, timeout: DefaultSyncTimeout, interval: DefaultWatchInterval,
	}
}

// WithObservability attaches the audit trail and the metric registry.
func (w *Watcher) WithObservability(a Auditor, m Metrics) *Watcher {
	w.auditor = a
	w.metrics = m
	return w
}

// WithInterval replaces the polling interval.
func (w *Watcher) WithInterval(d time.Duration) *Watcher {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithTimeout replaces the unsettled-sync bound.
func (w *Watcher) WithTimeout(d time.Duration) *Watcher {
	if d > 0 {
		w.timeout = d
	}
	return w
}

// WatchResult reports one tick.
type WatchResult struct {
	Checked  int
	Settled  int
	Diverged int
	Failed   int
}

// Tick polls every open delegated transfer once.
func (w *Watcher) Tick(ctx context.Context) (WatchResult, error) {
	var res WatchResult
	if w.store == nil {
		return res, nil
	}

	open, err := w.store.OpenSyncs(ctx, w.batch)
	if err != nil {
		return res, err
	}

	for _, s := range open {
		res.Checked++
		outcome, err := w.check(ctx, s)
		if err != nil {
			// One unreachable registry must not stop the others. The transfer
			// stays in `syncing` and is retried next tick, which is correct:
			// not being able to look is not the same as the sync failing.
			w.log.WarnContext(ctx, "could not check a delegated transfer",
				"transfer", s.TransferID, "target", s.TargetName, "error", err)
			continue
		}
		if outcome == "" {
			continue // still running
		}
		res.Settled++
		switch outcome {
		case store.SyncDiverged:
			res.Diverged++
		case store.SyncFailed:
			res.Failed++
		}
	}
	return res, nil
}

// check polls one delegated transfer, settling it when the registry is done.
//
// Returns the outcome it settled on, or "" when the sync is still running.
func (w *Watcher) check(ctx context.Context, s store.OpenSync) (string, error) {
	p, ok := w.products.Get(s.Product)
	if !ok {
		// The product was removed from configuration while a sync was open.
		// Settling as failed rather than leaving it: the transfer can never
		// complete now, and a row that hangs forever is worse than one that
		// says why it stopped.
		return w.settle(ctx, s, store.SyncFailed, "", "",
			fmt.Sprintf("product %q is no longer configured", s.Product))
	}
	t, ok := p.Target(s.TargetName)
	if !ok {
		return w.settle(ctx, s, store.SyncFailed, "", "",
			fmt.Sprintf("target %q is no longer configured", s.TargetName))
	}

	status, err := w.service.Status(ctx, p, t)
	if err != nil {
		return "", err
	}
	if status.Unreachable != nil {
		return "", status.Unreachable
	}
	if status.Observation == nil || status.Observation.Mirror == nil {
		return w.settle(ctx, s, store.SyncFailed, "", "",
			"the registry holds no mirror configuration for this target")
	}

	quayStatus := status.Observation.SyncStatus
	raw := status.Observation.SyncStatusRaw

	switch {
	case quayStatus.Running():
		return "", w.markRunning(ctx, s, raw)

	case quayStatus == quay.SyncFail:
		return w.settle(ctx, s, store.SyncFailed, raw, "", "the registry reported a failed sync")

	case quayStatus == quay.SyncSuccess:
		return w.verifyArrival(ctx, s, raw)

	case quayStatus == quay.SyncNeverRun:
		// We asked and the registry has not started. Normal for a few seconds,
		// a fault after the timeout — and the timeout is the only thing that
		// can tell the two apart.
		if w.expired(s) {
			return w.settle(ctx, s, store.SyncFailed, raw, "",
				fmt.Sprintf("the registry never started the sync within %s", w.timeout))
		}
		return "", nil

	default:
		// An unrecognised status. Not settled either way, because guessing is
		// exactly what docs/design/18 refuses to do — but not ignored either:
		// the timeout still applies, so this cannot hang forever.
		if w.expired(s) {
			return w.settle(ctx, s, store.SyncFailed, raw, "", fmt.Sprintf(
				"the registry reported %q for %s, which this build does not recognise", raw, w.timeout))
		}
		return "", w.markRunning(ctx, s, raw)
	}
}

// verifyArrival is the step that makes a delegated outcome an OBSERVATION
// rather than an assertion.
//
// The registry says the sync succeeded. That tells us it did something, not
// what. So the tag is resolved at the destination and compared with the digest
// we asked for, which is the only way `diverged` can be distinguished from
// `succeeded` at all.
func (w *Watcher) verifyArrival(ctx context.Context, s store.OpenSync, raw string) (string, error) {
	if w.dest == nil {
		// Without a reader we cannot look, and reporting success on the
		// registry's word alone would be the assertion this design refuses to
		// make. Recorded as unknown, which is honest and visible.
		return w.settle(ctx, s, store.SyncUnknown, raw, "",
			"the sync completed, but no destination reader is configured to confirm what arrived")
	}

	observed, err := w.dest.ResolveAtTarget(ctx, s.Product, s.TargetName, s.Tag)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		// The most common misconfiguration, and it must say WHICH of the two
		// it is: the sync succeeded and the tag is not there, which almost
		// always means the tag glob never matched it.
		return w.settle(ctx, s, store.SyncFailed, raw, "", fmt.Sprintf(
			"the sync completed but %q is not at the destination — the tag globs most likely do not match it", s.Tag))
	case err != nil:
		return "", err
	}

	if s.ExpectedDigest != "" && observed != s.ExpectedDigest {
		return w.settle(ctx, s, store.SyncDiverged, raw, observed, fmt.Sprintf(
			"the destination holds %s for %q, not the %s we asked for — the upstream tag has moved",
			shortDigest(observed), s.Tag, shortDigest(s.ExpectedDigest)))
	}
	return w.settle(ctx, s, store.SyncSucceeded, raw, observed, "")
}

// markRunning records that the registry is working on it.
func (w *Watcher) markRunning(ctx context.Context, s store.OpenSync, raw string) error {
	if s.SyncID == 0 {
		return nil
	}
	return w.store.UpdateSync(ctx, s.SyncID, store.MirrorSync{
		Status: store.SyncRunning, QuayStatus: raw,
		StartedAt: nowString(),
	})
}

// settle closes both records: the sync row, which is the evidence, and the
// transfer, which is what everything else reads.
func (w *Watcher) settle(ctx context.Context, s store.OpenSync, outcome, raw, observed, message string) (string, error) {
	if s.SyncID != 0 {
		if err := w.store.UpdateSync(ctx, s.SyncID, store.MirrorSync{
			Status: outcome, QuayStatus: raw,
			FinishedAt: nowString(), ObservedDigest: observed, Message: message,
		}); err != nil {
			return "", err
		}
	}

	// `unknown` settles the transfer as failed, because a transfer has no
	// "we could not tell" outcome and inventing one to mirror the sync row
	// would put a state nobody can act on in front of every reader. The sync
	// row keeps the distinction, which is where somebody investigating looks.
	transferState := outcome
	if outcome == store.SyncUnknown {
		transferState = store.SyncFailed
	}
	if err := w.store.SettleDelegated(ctx, s.TransferID, transferState, message); err != nil {
		return "", err
	}

	w.log.InfoContext(ctx, "delegated transfer settled",
		"transfer", s.TransferID, "target", s.TargetName,
		"outcome", outcome, "registryStatus", raw, "message", message)

	if w.metrics != nil {
		w.metrics.RecordSync(s.Product, s.TargetName, outcome)
		// Observed between OUR request and the registry reporting it done —
		// which is not a measurement of the registry's own work, and the
		// metric's help text says so. Recorded only when we know when we
		// asked, because a duration from an unknown start is a made-up number.
		if at, ok := quay.ParseTime(s.RequestedAt.String); ok && s.RequestedAt.Valid {
			w.metrics.RecordSyncDuration(s.Product, s.TargetName, time.Since(at).Seconds())
		}
	}
	w.audit(ctx, s, outcome, message)
	return outcome, nil
}

// audit records the outcome as a durable business fact.
//
// Note the event names: every one says what we DID or OBSERVED. None of them
// says "transferred", because we did not.
func (w *Watcher) audit(ctx context.Context, s store.OpenSync, outcome, message string) {
	if w.auditor == nil {
		return
	}
	event := AuditSyncFailed
	switch outcome {
	case store.SyncSucceeded:
		event = AuditSyncSucceeded
	case store.SyncDiverged:
		event = AuditContentDiverge
	}
	row := store.AuditRow{
		EventType: event, ActorKind: "system",
		ProductName: s.Product, SubjectKind: "transfer", SubjectID: s.TransferID,
		Outcome: outcome, Detail: message,
	}
	if err := w.auditor.Record(ctx, row); err != nil {
		w.log.WarnContext(ctx, "could not record a sync audit event",
			"transfer", s.TransferID, "event", event, "error", err)
	}
}

// expired reports whether a sync has been open longer than the bound.
func (w *Watcher) expired(s store.OpenSync) bool {
	if !s.RequestedAt.Valid {
		return false
	}
	at, ok := quay.ParseTime(s.RequestedAt.String)
	if !ok {
		return false
	}
	return time.Since(at) > w.timeout
}

func nowString() sql.NullString {
	return sql.NullString{String: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Valid: true}
}

func shortDigest(d string) string {
	if i := strings.Index(d, ":"); i >= 0 && len(d) > i+13 {
		return d[:i+13] + "…"
	}
	return d
}

// SetLeader is called by the elector on every leadership change.
//
// Leader-gated for the same reason the expander is: every tick is idempotent,
// so running it on both replicas would be harmless — and would double the
// polling load on somebody else's registry for no benefit.
func (w *Watcher) SetLeader(isLeader bool) {
	w.mu.Lock()
	w.leader = isLeader
	w.mu.Unlock()
}

// Run polls on the interval until the context is cancelled.
//
// Never returns an error for a failed tick. The worst outcome of a watcher
// that cannot run is a delegated transfer that settles late; killing the
// Coordinator over an unreachable third-party registry would turn somebody
// else's outage into ours.
func (w *Watcher) Run(ctx context.Context) error {
	if w.store == nil {
		return nil
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.log.Info("delegated replication: watching for syncs to settle",
		"interval", w.interval, "timeout", w.timeout)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.mu.Lock()
			isLeader := w.leader
			w.mu.Unlock()
			if !isLeader {
				continue
			}
			res, err := w.Tick(ctx)
			if err != nil {
				w.log.ErrorContext(ctx, "delegated replication: tick failed", "error", err)
				continue
			}
			if res.Settled > 0 {
				w.log.InfoContext(ctx, "delegated replication: settled transfers",
					"checked", res.Checked, "settled", res.Settled,
					"diverged", res.Diverged, "failed", res.Failed)
			}
		}
	}
}
