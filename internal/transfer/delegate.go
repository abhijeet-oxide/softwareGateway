package transfer

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The strategy seam.
//
// Everything in this package is written against "our workers move the bytes",
// and that stays true. What changes is that a destination may not want them
// moved: a Quay target in `mirror` mode fetches for itself, so the correct
// plan for it is no jobs at all.
//
// The seam is deliberately ONE LEVEL ABOVE the planner and the engine, at the
// point where a transfer becomes work. The planner still knows only about
// manifests and blobs; the engine still knows only about streams. Pushing
// delegation down into either would have made every one of their code paths
// answer "and what if we are not actually moving anything?" - see
// docs/design/18 section 7.
//
// It is also an optional dependency. A deployment with no delegated targets -
// which is every deployment before M8 - leaves it nil, and every transfer is a
// copy exactly as before.

// Strategy names how content reaches a destination.
type Strategy string

const (
	// StrategyCopy is our workers pulling from the origin and pushing.
	StrategyCopy Strategy = "copy"
	// StrategyMirror is the destination registry pulling for itself.
	StrategyMirror Strategy = "mirror"
	// StrategyProxy is a pull-through cache, which cannot be a destination at
	// all - nothing can be pushed into one. It appears here only so that a
	// transfer that somehow reaches the expander fails with a message naming
	// what to do instead.
	StrategyProxy Strategy = "proxy"
)

// SyncRequest is what the delegated side needs to ask a registry to fetch.
type SyncRequest struct {
	TransferID  string
	ProductID   int64
	ProductName string
	TargetName  string

	// ExpectedDigest is the manifest digest the destination should end up
	// holding. It is what makes `diverged` detectable: without it a completed
	// sync could only be reported as "the registry says it worked".
	ExpectedDigest string
	Tag            string
}

// SyncHandle identifies the sync record that will carry the outcome.
type SyncHandle struct {
	SyncID int64
	// AlreadyRunning means the registry was mid-sync. That SATISFIES the
	// request rather than failing it - the caller wanted a sync and a sync is
	// happening - so it is carried as a fact rather than as an error.
	AlreadyRunning bool
}

// Delegation is the delegated half of the seam.
//
// A consumer-defined interface, and the reason this package still has no
// knowledge of Quay, of product configuration or of secrets: it needs two
// answers, not the management client and the reconciler behind them.
type Delegation interface {
	// Strategy reports how content reaches a configured target. It is called
	// on every plan, so it must be cheap - a configuration lookup, not a
	// network call.
	Strategy(ctx context.Context, productName, targetName string) (Strategy, error)

	// RequestSync asks the registry to fetch now. It must NOT apply
	// configuration: applying a mirror is destructive, and `download v3.2.1`
	// is not a request to make a repository read-only. A target that has not
	// been applied fails here, naming `targets apply`.
	RequestSync(ctx context.Context, req SyncRequest) (SyncHandle, error)
}

// delegate plans a transfer whose destination fetches for itself.
//
// It creates no jobs and never will. The transfer enters `syncing` and is
// settled later by whatever observes the registry - which is the only honest
// shape, because the registry is under no obligation to finish while we are
// watching, and a mirror that completes overnight while the Coordinator is
// being restarted must still settle correctly in the morning.
func (e *Expander) delegate(
	ctx context.Context, t store.PendingTransfer, strategy Strategy, targetName string, pkg store.PackageRow,
) error {
	if e.delegation == nil {
		return fmt.Errorf("target %q is in %s mode, but no replication service is configured", targetName, strategy)
	}
	if strategy == StrategyProxy {
		return fmt.Errorf(
			"target %q is a proxy cache: content enters when something pulls through it, so there is nothing to transfer. Use `transferctl warm` to populate it deliberately",
			targetName)
	}

	handle, err := e.delegation.RequestSync(ctx, SyncRequest{
		TransferID:     t.ID,
		ProductID:      t.ProductID,
		ProductName:    t.ProductName,
		TargetName:     targetName,
		ExpectedDigest: pkg.ManifestDigest,
		Tag:            pkg.Tag,
	})
	if err != nil {
		return err
	}

	if err := e.replication.MarkSyncing(ctx, t.ID, string(strategy)); err != nil {
		return err
	}
	if handle.SyncID != 0 {
		if err := e.replication.LinkSync(ctx, handle.SyncID, t.ID); err != nil {
			// Not fatal. The transfer is in `syncing` and the watcher finds it
			// from `transfers`, not from the sync row - which is exactly why
			// the watcher is driven from that side.
			e.log.WarnContext(ctx, "could not link the sync record to its transfer",
				"transfer", t.ID, "sync", handle.SyncID, "error", err)
		}
	}

	e.log.InfoContext(ctx, "transfer delegated to the destination registry",
		"transfer", t.ID, "target", targetName, "strategy", strategy,
		"alreadySyncing", handle.AlreadyRunning)
	return nil
}
