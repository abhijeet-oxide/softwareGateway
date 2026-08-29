package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/replication"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Replicator is the target-replication surface the API needs.
//
// A consumer-defined interface rather than *replication.Service, so this
// package depends on the four calls it makes rather than on management
// clients, secret resolution and drift arithmetic (docs/design/15 §6).
type Replicator interface {
	Status(ctx context.Context, p *product.Product, t product.Target) (*replication.Status, error)
	Apply(ctx context.Context, p *product.Product, t product.Target,
		opts replication.ApplyOptions) (*replication.ApplyResult, error)
	Sync(ctx context.Context, p *product.Product, t product.Target, actor string) (*replication.SyncOutcome, error)
	CancelSync(ctx context.Context, p *product.Product, t product.Target, actor string) (*replication.SyncOutcome, error)
}

// maxConcurrentTargetReads bounds the fan-out in handleListReplication.
//
// Eight is above the number of targets a real product document declares, so in
// practice this reads every target at once; it exists so a document with fifty
// targets opens eight connections rather than fifty.
const maxConcurrentTargetReads = 8

// handleGetReplication reports one target's replication state.
func (s *Server) handleGetReplication(w http.ResponseWriter, r *http.Request) {
	p, t, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	st, err := s.deps.Replication.Status(r.Context(), p, t)
	if err != nil {
		Error(w, r, v1.CodeInternal, err.Error())
		return
	}
	WriteJSON(w, r, http.StatusOK, replicationView(st))
}

// handleListReplication reports every target of one product.
//
// Including the copy targets, which is not padding: "which of my targets are
// delegated" is the question this listing exists to answer, and a listing that
// showed only the delegated ones could not answer it.
func (s *Server) handleListReplication(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}

	// SIDE BY SIDE, because each delegated target costs a round trip to its
	// own registry.
	//
	// Read serially, a product with four delegated targets spent four registry
	// latencies end to end - and the Downloads page asks this of EVERY product
	// at once to draw its drift banner, so the slowest registry in the estate
	// set the wall-clock time of the whole page. They are independent reads of
	// different registries; nothing is gained by waiting for one before
	// starting the next.
	//
	// Bounded, because "one goroutine per target" is a fan-out a product
	// document controls. The order of the answer is the order of the
	// configuration - written by index rather than appended - so a listing
	// somebody is comparing against a previous one does not reshuffle itself
	// because a registry was slow this time.
	views := make([]v1.ReplicationView, len(p.Spec.Targets))
	g, gctx := errgroup.WithContext(r.Context())
	g.SetLimit(maxConcurrentTargetReads)
	for i, t := range p.Spec.Targets {
		g.Go(func() error {
			st, err := s.deps.Replication.Status(gctx, p, t)
			if err != nil {
				// One bad target must not blank the whole listing: the row is
				// returned with the reason in it, which is more useful than a
				// 500 that does not say which target failed. So the error is
				// carried in the row and never returned to the group - one
				// unreachable registry must not cancel the reads of the others.
				views[i] = v1.ReplicationView{
					Product: p.Metadata.Name, Target: t.Name,
					Mode: string(t.ReplicationMode()), Unreachable: err.Error(),
				}
				return nil
			}
			views[i] = replicationView(st)
			return nil
		})
	}
	_ = g.Wait() // No goroutine above returns an error; see the comment there.

	WriteJSON(w, r, http.StatusOK,
		v1.ListReplicationResponse{Targets: views})
}

// handleApplyReplication writes the configuration to the registry.
//
// A custom method rather than a PUT on the resource, because it is not an edit
// of our state: it pushes what Git already says into a third-party registry's
// own configuration store. Products remain read-only over this API.
func (s *Server) handleApplyReplication(w http.ResponseWriter, r *http.Request) {
	p, t, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}

	var req v1.ApplyReplicationRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	// ?validateOnly=true is accepted as well as the body field, because it is
	// the spelling every other dry run in this API uses.
	if v := r.URL.Query().Get("validateOnly"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			req.ValidateOnly = b
		}
	}

	if !t.Delegated() {
		Error(w, r, v1.CodeFailedPrecondition, fmt.Sprintf(
			"target %q is in copy mode: our own workers write to it, and there is no registry-side configuration to apply",
			t.Name))
		return
	}

	res, err := s.deps.Replication.Apply(r.Context(), p, t, replication.ApplyOptions{
		ValidateOnly: req.ValidateOnly, Confirm: req.Confirm, Actor: actorOf(r),
	})
	if err != nil {
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}

	resp := v1.ApplyReplicationResponse{
		Product: p.Metadata.Name, Target: t.Name, Mode: string(t.ReplicationMode()),
		Applied: res.Applied, ConfigHash: res.ConfigHash,
		NeedsConfirmation: res.NeedsConfirmation,
	}
	if res.Plan != nil {
		resp.Steps = res.Plan.Steps
		resp.Destructive = res.Plan.Destructive
		resp.NoOp = res.Plan.NoOp
	}
	if res.NeedsConfirmation {
		// 409 rather than 400: the request is well formed and the STATE is the
		// problem. The plan travels in the body, so a caller can show it and
		// ask, rather than making a second request to find out why.
		WriteJSON(w, r, http.StatusConflict, resp)
		return
	}
	WriteJSON(w, r, http.StatusOK, resp)
}

// handleSyncReplication asks the registry to sync now.
func (s *Server) handleSyncReplication(w http.ResponseWriter, r *http.Request) {
	s.syncVerb(w, r, false)
}

// handleCancelSyncReplication stops an in-progress sync.
func (s *Server) handleCancelSyncReplication(w http.ResponseWriter, r *http.Request) {
	s.syncVerb(w, r, true)
}

func (s *Server) syncVerb(w http.ResponseWriter, r *http.Request, cancel bool) {
	p, t, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	if t.ReplicationMode() != product.ReplicationMirror {
		Error(w, r, v1.CodeFailedPrecondition, syncRefusal(t))
		return
	}

	var (
		out *replication.SyncOutcome
		err error
	)
	if cancel {
		out, err = s.deps.Replication.CancelSync(r.Context(), p, t, actorOf(r))
	} else {
		out, err = s.deps.Replication.Sync(r.Context(), p, t, actorOf(r))
	}
	if err != nil {
		Error(w, r, v1.CodeInternal, err.Error())
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.SyncReplicationResponse{
		Product: p.Metadata.Name, Target: t.Name,
		Requested: out.Requested, AlreadyRunning: out.AlreadyRunning,
		At: out.At.UTC().Format(time.RFC3339), SyncID: out.SyncID,
	})
}

// syncRefusal explains what to do instead, per mode.
//
// A refusal that named only what is wrong would leave a proxy-cache user with
// no next step, and `warm` is exactly the next step.
func syncRefusal(t product.Target) string {
	switch t.ReplicationMode() {
	case product.ReplicationProxy:
		return fmt.Sprintf(
			"target %q is a proxy cache: content enters when something pulls through it, so there is no sync to request. Use `transferctl warm` to populate it deliberately",
			t.Name)
	default:
		return fmt.Sprintf(
			"target %q is in copy mode: our own workers move the bytes, so there is no registry-side sync. Use `transferctl download`",
			t.Name)
	}
}

// handleListSyncs returns a target's observed sync history.
func (s *Server) handleListSyncs(w http.ResponseWriter, r *http.Request) {
	p, t, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}

	limit := 50
	if v := r.URL.Query().Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	rows, err := s.deps.ReplicationStore.ListSyncs(r.Context(), p.Metadata.Name, t.Name, limit)
	if err != nil {
		Error(w, r, v1.CodeInternal, err.Error())
		return
	}

	out := v1.ListSyncsResponse{Syncs: make([]v1.MirrorSyncView, 0, len(rows))}
	for _, row := range rows {
		out.Syncs = append(out.Syncs, v1.MirrorSyncView{
			ID: row.ID, Target: row.TargetName,
			Status: row.Status, QuayStatus: row.QuayStatus,
			RequestedAt: row.RequestedAt.String, StartedAt: row.StartedAt.String,
			FinishedAt:     row.FinishedAt.String,
			ExpectedDigest: row.ExpectedDigest, ObservedDigest: row.ObservedDigest,
			TransferID: transferRef(row.TransferID),
			Message:    row.Message,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// replicationView maps the domain status onto the wire form.
//
// Note what is NOT mapped: no credential in any form, and no progress field of
// any kind. A delegated target reports a state, never a percentage.
func replicationView(st *replication.Status) v1.ReplicationView {
	out := v1.ReplicationView{
		Product: st.Product, Target: st.Target, Mode: string(st.Mode),
		PendingApply: st.PendingApply,
	}
	if st.Unreachable != nil {
		out.Unreachable = st.Unreachable.Error()
	}
	if st.HasApplied {
		out.Applied = &v1.ReplicationApplied{
			ConfigHash: st.AppliedConfigHash, At: st.AppliedAt, By: st.AppliedBy,
		}
	}
	if d := st.Desired; d != nil {
		out.Desired = desiredView(d)
	}
	if obs := st.Observation; obs != nil {
		out.Observed = observedView(obs)
		out.Drift = driftView(obs)
	}
	return out
}

func desiredView(d *replication.Desired) *v1.ReplicationDesired {
	view := &v1.ReplicationDesired{ConfigHash: d.ConfigHash()}
	if m := d.Mirror; m != nil {
		view.Upstream = m.ExtRef
		view.Tags = m.RootRule.RuleValue
		view.Interval = (time.Duration(m.SyncInterval) * time.Second).String()
		view.Robot = m.RobotUsername
	}
	if c := d.Proxy; c != nil {
		view.Organization = d.Organization
		view.Upstream = c.UpstreamRegistry
		view.Expiration = (time.Duration(c.ExpirationS) * time.Second).String()
	}
	return view
}

func observedView(obs *replication.Observation) *v1.ReplicationObserved {
	view := &v1.ReplicationObserved{RepositoryState: obs.RepositoryState}
	if m := obs.Mirror; m != nil {
		view.Configured = true
		view.Tags = m.RootRule.RuleValue
		view.Upstream = m.ExtRef
		view.SyncStatus = string(obs.SyncStatus)
		view.SyncStatusRaw = obs.SyncStatusRaw
	}
	if c := obs.Proxy; c != nil {
		view.Configured = true
		view.Upstream = c.UpstreamRegistry
	}
	return view
}

func driftView(obs *replication.Observation) *v1.ReplicationDrift {
	d := obs.Drift
	view := &v1.ReplicationDrift{
		Detected: d.Drifted(), Summary: d.Summary(),
		Absent: d.Absent, StateWrong: d.StateWrong,
		CredentialsRotated: d.CredentialsRotated,
	}
	for _, f := range d.Fields {
		view.Fields = append(view.Fields, v1.ReplicationField{Field: f.Field, Want: f.Want, Got: f.Got})
	}
	return view
}

func transferRef(id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	return strconv.FormatInt(id.Int64, 10)
}

// resolveProduct looks up the product named in the path.
func (s *Server) resolveProduct(w http.ResponseWriter, r *http.Request) (*product.Product, bool) {
	name := chi.URLParam(r, "product")
	p, ok := s.deps.Products.Get(name)
	if !ok {
		NotFound(w, r, "product", name)
		return nil, false
	}
	return p, true
}

// resolveTarget looks up the product and target named in the path.
func (s *Server) resolveTarget(w http.ResponseWriter, r *http.Request) (*product.Product, product.Target, bool) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return nil, product.Target{}, false
	}
	name := chi.URLParam(r, "target")
	t, ok := p.Target(name)
	if !ok {
		// Naming the alternatives, because a target name is something an
		// operator types and the failure is nearly always a typo.
		names := make([]string, 0, len(p.Spec.Targets))
		for _, t := range p.Spec.Targets {
			names = append(names, t.Name)
		}
		Error(w, r, v1.CodeNotFound, fmt.Sprintf(
			"product %q has no target %q; it has: %s",
			p.Metadata.Name, name, strings.Join(names, ", ")))
		return nil, product.Target{}, false
	}
	return p, t, true
}

// actorOf is who is asking. Until authentication lands this is "anonymous",
// and it is plumbed anyway so the audit trail has the field it will need
// rather than acquiring a year of unattributable history first.
func actorOf(r *http.Request) string {
	return middleware.IdentityFrom(r.Context()).Subject
}
