package replication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Service is the target-replication use case: read, plan, apply, sync.
//
// It owns the ORDER of things — read before write, validate before store,
// record what we sent after a success — and holds no HTTP and no rendering.
// The API and the CLI are both clients of this, which is what keeps the two
// from disagreeing about what an apply means.
type Service struct {
	resolver *Resolver
	store    *store.Replication
	log      *slog.Logger

	auditor Auditor
	metrics Metrics

	// clients is a per-target cache, because building one performs TLS setup
	// and reads a Secret from disk, and a listing touches every target.
	clients map[string]ManagementAPI
}

// NewService builds one. store may be nil for tooling that only reads
// configuration; in that case nothing is recorded and drift is computed
// against an empty applied state, which reports everything as pending.
func NewService(resolver *Resolver, st *store.Replication, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{resolver: resolver, store: st, log: log, clients: map[string]ManagementAPI{}}
}

// WithObservability attaches the audit trail and the metric registry.
//
// Both optional and both nil in every test, because a decision this package
// makes must be assertable without a database or a Prometheus registry.
func (s *Service) WithObservability(a Auditor, m Metrics) *Service {
	s.auditor = a
	s.metrics = m
	return s
}

// Status is everything known about one target's replication.
type Status struct {
	Product string
	Target  string
	Mode    product.ReplicationMode

	// Desired is what the configuration asks for. Nil for a copy target.
	Desired *Desired

	// Observation is what the registry says. Nil when Unreachable is set, or
	// when the mode has nothing to observe.
	Observation *Observation
	// Unreachable is why we could not read the registry. A status with this
	// set is still worth returning: the configured intent and the last apply
	// are both known without a network call, and saying "we could not look" is
	// more useful than saying nothing.
	Unreachable error

	HasApplied        bool
	AppliedConfigHash string
	AppliedAt         string
	AppliedBy         string

	// PendingApply means Git has changed since the last apply. A different
	// question from drift, and both are worth reporting: not having applied
	// yet is normal after a merge; somebody editing the registry by hand is
	// not.
	PendingApply bool
}

// Status reads configuration, the last apply and — where reachable — the
// registry. It never writes to the registry.
func (s *Service) Status(ctx context.Context, p *product.Product, t product.Target) (*Status, error) {
	out := &Status{Product: p.Metadata.Name, Target: t.Name, Mode: t.ReplicationMode()}

	desired, err := s.resolver.Desired(p, t)
	if err != nil {
		return nil, err
	}
	if t.Delegated() {
		out.Desired = desired
	}

	applied := s.appliedRecord(ctx, p.Metadata.Name, t.Name, out)
	if !t.Delegated() {
		return out, nil
	}
	out.PendingApply = Pending(applied.ConfigHash, desired.ConfigHash())

	api, err := s.client(p, t)
	if err != nil {
		out.Unreachable = err
		return out, nil
	}
	obs, err := Observe(ctx, api, desired, applied)
	if err != nil {
		out.Unreachable = err
		return out, nil
	}
	out.Observation = obs

	s.recordObservation(ctx, p.Metadata.Name, t, obs)
	s.reportObservation(ctx, p.Metadata.Name, t, obs)
	return out, nil
}

// appliedRecord reads what we last wrote, filling the status as a side effect.
//
// A missing record is not an error: it means nobody has applied this target
// yet, which is the state every target starts in.
func (s *Service) appliedRecord(ctx context.Context, productName, target string, out *Status) Applied {
	if s.store == nil {
		return Applied{}
	}
	productID, err := s.store.ProductID(ctx, productName)
	if err != nil {
		return Applied{}
	}
	rec, err := s.store.Get(ctx, productID, target)
	if errors.Is(err, store.ErrNoRecord) {
		return Applied{}
	}
	if err != nil {
		s.log.Warn("read replication record", "product", productName, "target", target, "error", err)
		return Applied{}
	}
	out.HasApplied = rec.AppliedAt.Valid
	out.AppliedConfigHash = rec.AppliedConfigHash
	out.AppliedAt = rec.AppliedAt.String
	out.AppliedBy = rec.AppliedBy
	return Applied{ConfigHash: rec.AppliedConfigHash, SecretHash: rec.AppliedSecretHash}
}

// recordObservation persists what we just saw, so a listing and a metric can
// answer without a network call.
//
// Failures are logged and swallowed: an observation is a convenience, and a
// database hiccup must not turn a successful read of the registry into an
// error the operator has to interpret.
func (s *Service) recordObservation(ctx context.Context, productName string, t product.Target, obs *Observation) {
	if s.store == nil {
		return
	}
	productID, err := s.store.ProductID(ctx, productName)
	if err != nil {
		return
	}
	if err := s.store.Observed(ctx, productID, t.Name, string(t.ReplicationMode()),
		obs.Drift.Drifted(), obs.Drift.Summary(), s.resolver.Now()); err != nil {
		s.log.Warn("record replication observation", "product", productName, "target", t.Name, "error", err)
	}
}

// reportObservation publishes what a read found.
//
// Split from recordObservation because the two have different failure
// tolerances: a metric that cannot be set is nothing, and a database write that
// fails is worth a log line.
func (s *Service) reportObservation(ctx context.Context, productName string, t product.Target, obs *Observation) {
	if s.metrics != nil {
		s.metrics.SetDrift(productName, t.Name, obs.Drift.Drifted())
		if obs.Proxy != nil || t.ReplicationMode() == product.ReplicationProxy {
			s.metrics.RecordProxyProbe(productName, t.Name, probeResult(obs))
		}
	}
	// Audited only when it is NEWS. Drift is re-observed on every reload and
	// from the metrics sweep; an event per observation would bury the five
	// real ones under thousands of routine ones, which is how an audit trail
	// stops being read. The gauge carries the steady state.
	if obs.Drift.Drifted() && !obs.Drift.Absent {
		s.audit(ctx, store.AuditRow{
			EventType: AuditConfigDrifted, ActorKind: "system",
			ProductName: productName, SubjectKind: "target", SubjectID: t.Name,
			Outcome: "detected", Detail: obs.Drift.Summary(),
		})
	}
}

func probeResult(obs *Observation) string {
	if obs.Proxy == nil {
		return "absent"
	}
	return "configured"
}

// ApplyOptions govern one apply.
type ApplyOptions struct {
	// ValidateOnly renders the plan and writes nothing.
	ValidateOnly bool
	// Confirm is required for a destructive plan.
	Confirm bool
	Actor   string
}

// ApplyResult is what happened, or what would.
type ApplyResult struct {
	Plan *Plan
	// Applied reports whether anything was written.
	Applied bool
	// NeedsConfirmation means the plan is destructive and Confirm was not set.
	// Refusing here rather than in a caller means every client — CLI, API,
	// and whatever comes next — inherits the same guard rail.
	NeedsConfirmation bool
	ConfigHash        string
}

// Apply writes a target's replication configuration to the registry.
//
// Never called by a configuration reload. Applying a mirror flips the
// destination repository into MIRROR state, which makes it read-only to
// everyone but a robot and converges its content to a tag glob — deleting
// whatever does not match. A `kubectl apply` of a ConfigMap with a typo in
// `tags` must not be able to empty a production repository, and the only way
// to guarantee that is for the write to be a separate, deliberate act
// (docs/design/18 section 8).
func (s *Service) Apply(ctx context.Context, p *product.Product, t product.Target, opts ApplyOptions) (*ApplyResult, error) {
	if !t.Delegated() {
		return nil, fmt.Errorf(
			"target %q is in copy mode: our own workers write to it, and there is no registry-side configuration to apply", t.Name)
	}
	if mode := manageModeOf(t); mode == product.ManageDetect {
		return nil, fmt.Errorf(
			"target %q is configured `manage: detect`, which never writes: drift is reported and closing it is a deliberate change to that field", t.Name)
	}

	desired, err := s.resolver.Desired(p, t)
	if err != nil {
		return nil, err
	}
	api, err := s.client(p, t)
	if err != nil {
		return nil, err
	}

	var status Status
	applied := s.appliedRecord(ctx, p.Metadata.Name, t.Name, &status)

	plan, err := PlanApply(ctx, api, desired, applied)
	if err != nil {
		return nil, err
	}
	res := &ApplyResult{Plan: plan, ConfigHash: desired.ConfigHash()}

	switch {
	case opts.ValidateOnly:
		return res, nil
	case plan.NoOp:
		// Recorded anyway: a no-op still proves the registry says what Git
		// says at this moment, and that is exactly the fact `drift` reports.
		s.recordApply(ctx, p, t, desired, opts.Actor)
		return res, nil
	case plan.Destructive && !opts.Confirm:
		res.NeedsConfirmation = true
		return res, nil
	}

	if _, err := Apply(ctx, api, desired, s.resolver.Now()); err != nil {
		return nil, err
	}
	s.recordApply(ctx, p, t, desired, opts.Actor)
	res.Applied = true

	event := AuditConfigApplied
	if t.ReplicationMode() == product.ReplicationProxy {
		event = AuditProxyConfigured
	}
	s.audit(ctx, store.AuditRow{
		EventType: event, Actor: opts.Actor, ActorKind: "user",
		ProductName: p.Metadata.Name, SubjectKind: "target", SubjectID: t.Name,
		Outcome: "success",
		Detail:  strings.Join(plan.Steps, "; "),
	})
	return res, nil
}

func (s *Service) recordApply(ctx context.Context, p *product.Product, t product.Target, d *Desired, actor string) {
	if s.store == nil {
		return
	}
	productID, err := s.store.ProductID(ctx, p.Metadata.Name)
	if err != nil {
		s.log.Warn("record apply", "product", p.Metadata.Name, "target", t.Name, "error", err)
		return
	}
	if err := s.store.Applied(ctx, productID, t.Name, string(t.ReplicationMode()),
		d.ConfigHash(), d.SecretHash, actor, s.resolver.Now()); err != nil {
		s.log.Warn("record apply", "product", p.Metadata.Name, "target", t.Name, "error", err)
	}
}

// SyncOutcome is the result of asking the registry to sync now.
type SyncOutcome struct {
	Requested      bool
	AlreadyRunning bool
	At             time.Time
	SyncID         int64
}

// Sync asks the registry to sync a mirror target immediately.
func (s *Service) Sync(ctx context.Context, p *product.Product, t product.Target, actor string) (*SyncOutcome, error) {
	desired, api, err := s.mirrorTarget(p, t)
	if err != nil {
		return nil, err
	}

	now := s.resolver.Now()
	res, err := RequestSync(ctx, api, desired, now)
	if err != nil {
		return nil, err
	}

	out := &SyncOutcome{Requested: true, AlreadyRunning: res.AlreadyRunning, At: now}
	if s.store != nil {
		if productID, err := s.store.ProductID(ctx, p.Metadata.Name); err == nil {
			// The sync record opens here and is closed by whatever observes
			// the outcome. Opening it at the request rather than at the
			// completion is what makes "we asked and nothing ever happened"
			// a visible state rather than an absence.
			id, err := s.store.RecordSyncRequested(ctx, productID, t.Name, sql.NullInt64{}, "", now)
			if err != nil {
				s.log.Warn("record sync request", "target", t.Name, "error", err)
			}
			out.SyncID = id
		}
	}
	s.log.Info("mirror sync requested",
		"product", p.Metadata.Name, "target", t.Name, "actor", actor,
		"alreadyRunning", res.AlreadyRunning)
	s.audit(ctx, store.AuditRow{
		EventType: AuditSyncRequested, Actor: actor, ActorKind: "user",
		ProductName: p.Metadata.Name, SubjectKind: "target", SubjectID: t.Name,
		Outcome: "success",
		Detail:  syncDetail(res.AlreadyRunning),
	})
	return out, nil
}

func syncDetail(alreadyRunning bool) string {
	if alreadyRunning {
		return "a sync was already in progress, which satisfies the request"
	}
	return "the registry accepted the request"
}

// CancelSync stops an in-progress sync.
func (s *Service) CancelSync(ctx context.Context, p *product.Product, t product.Target, actor string) (*SyncOutcome, error) {
	desired, api, err := s.mirrorTarget(p, t)
	if err != nil {
		return nil, err
	}
	wasRunning, err := CancelSync(ctx, api, desired)
	if err != nil {
		return nil, err
	}
	s.log.Info("mirror sync cancelled",
		"product", p.Metadata.Name, "target", t.Name, "actor", actor, "wasRunning", wasRunning)
	return &SyncOutcome{Requested: wasRunning, At: s.resolver.Now()}, nil
}

func (s *Service) mirrorTarget(p *product.Product, t product.Target) (*Desired, ManagementAPI, error) {
	if t.ReplicationMode() != product.ReplicationMirror {
		return nil, nil, fmt.Errorf("target %q is in %s mode and has no sync to request",
			t.Name, t.ReplicationMode())
	}
	desired, err := s.resolver.Desired(p, t)
	if err != nil {
		return nil, nil, err
	}
	api, err := s.client(p, t)
	if err != nil {
		return nil, nil, err
	}
	return desired, api, nil
}

// client returns a cached management client for a target's registry.
func (s *Service) client(p *product.Product, t product.Target) (ManagementAPI, error) {
	key := p.Metadata.Name + "/" + t.Name
	if c, ok := s.clients[key]; ok {
		return c, nil
	}
	c, err := s.resolver.Client(p, t)
	if err != nil {
		return nil, err
	}
	s.clients[key] = c
	return c, nil
}

// SetClient injects a management client for one target. Tests only.
func (s *Service) SetClient(product, target string, api ManagementAPI) {
	s.clients[product+"/"+target] = api
}

func manageModeOf(t product.Target) product.ManageMode {
	if m, ok := t.MirrorSettings(); ok {
		return m.ManageMode()
	}
	if c, ok := t.ProxySettings(); ok {
		return c.ManageMode()
	}
	return product.ManageApply
}
