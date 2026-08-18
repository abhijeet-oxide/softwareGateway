package replication

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// Delegation adapts the replication service to the transfer package's seam.
//
// It lives here rather than in internal/transfer because it is the side that
// knows about products, Quay and secrets — which is precisely what the seam
// exists to keep out of the transfer engine.
type Delegation struct {
	service  *Service
	products Products
	store    *store.Replication
}

// NewDelegation builds the adapter.
func NewDelegation(svc *Service, products Products, st *store.Replication) *Delegation {
	return &Delegation{service: svc, products: products, store: st}
}

// Strategy reports how content reaches a configured target.
//
// A pure configuration lookup with no network call, because the expander asks
// on every plan and a round trip per transfer would be a tax on the copy path
// for the benefit of the delegated one.
func (d *Delegation) Strategy(_ context.Context, productName, targetName string) (transfer.Strategy, error) {
	p, ok := d.products.Get(productName)
	if !ok {
		return transfer.StrategyCopy, fmt.Errorf("product %q is not loaded", productName)
	}
	t, ok := p.Target(targetName)
	if !ok {
		return transfer.StrategyCopy, fmt.Errorf("product %q has no target %q", productName, targetName)
	}
	switch t.ReplicationMode() {
	case product.ReplicationMirror:
		return transfer.StrategyMirror, nil
	case product.ReplicationProxy:
		return transfer.StrategyProxy, nil
	default:
		return transfer.StrategyCopy, nil
	}
}

// RequestSync asks the registry to fetch now.
//
// It deliberately does NOT apply configuration first, which is a divergence
// from the first draft of docs/design/18 §6 and is written down there. The
// reason: applying a mirror flips the destination repository read-only and
// converges its content to a tag glob, and `download v3.2.1` is not a request
// to do that. A target nobody has applied fails here, naming the command that
// would.
func (d *Delegation) RequestSync(ctx context.Context, req transfer.SyncRequest) (transfer.SyncHandle, error) {
	p, ok := d.products.Get(req.ProductName)
	if !ok {
		return transfer.SyncHandle{}, fmt.Errorf("product %q is not loaded", req.ProductName)
	}
	t, ok := p.Target(req.TargetName)
	if !ok {
		return transfer.SyncHandle{}, fmt.Errorf("product %q has no target %q", req.ProductName, req.TargetName)
	}

	if m, ok := t.MirrorSettings(); ok && !m.SyncsOnRequest() {
		return transfer.SyncHandle{}, fmt.Errorf(
			"target %q has syncOnRequest disabled: it converges on its own %s schedule and a transfer has nothing to do",
			t.Name, m.EffectiveInterval())
	}

	status, err := d.service.Status(ctx, p, t)
	if err != nil {
		return transfer.SyncHandle{}, err
	}
	if status.Unreachable != nil {
		return transfer.SyncHandle{}, status.Unreachable
	}
	// The precondition that turns a silent no-op into a sentence. Asking an
	// unconfigured mirror to sync succeeds at the API level and moves nothing.
	if status.Observation == nil || status.Observation.Drift.Absent {
		return transfer.SyncHandle{}, fmt.Errorf(
			"target %q has no mirror configuration on the registry yet — run `transferctl targets apply %s %s` first",
			t.Name, req.ProductName, t.Name)
	}

	out, err := d.service.Sync(ctx, p, t, "transfer "+req.TransferID)
	if err != nil {
		return transfer.SyncHandle{}, err
	}

	// The expected digest is recorded on the sync row here rather than at
	// settle time, because it is what we asked for — and comparing against a
	// digest looked up after the fact would compare the destination with
	// itself.
	if d.store != nil && out.SyncID != 0 && req.ExpectedDigest != "" {
		if err := d.store.SetExpectedDigest(ctx, out.SyncID, req.ExpectedDigest); err != nil {
			return transfer.SyncHandle{}, err
		}
	}

	return transfer.SyncHandle{SyncID: out.SyncID, AlreadyRunning: out.AlreadyRunning}, nil
}

var _ transfer.Delegation = (*Delegation)(nil)
