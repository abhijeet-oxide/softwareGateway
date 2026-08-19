package replication

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
)

// ManagementAPI is the slice of Quay's control API this package uses.
//
// An interface rather than the concrete client so the reconciler can be tested
// without HTTP, and so a second registry that grew comparable features would
// not require this file to be rewritten around it.
type ManagementAPI interface {
	GetRepository(ctx context.Context, namespace, name string) (*quay.Repository, error)
	SetRepositoryState(ctx context.Context, namespace, name string, state quay.RepositoryState) error
	GetMirror(ctx context.Context, namespace, name string) (*quay.MirrorState, error)
	PutMirror(ctx context.Context, namespace, name string, spec quay.MirrorSpec) error
	SyncNow(ctx context.Context, namespace, name string) (bool, error)
	CancelSync(ctx context.Context, namespace, name string) (bool, error)

	GetProxyCache(ctx context.Context, org string) (*quay.ProxyCache, error)
	CreateProxyCache(ctx context.Context, org string, spec quay.ProxyCache) error
	DeleteProxyCache(ctx context.Context, org string) error
	ValidateProxyCache(ctx context.Context, org string, spec quay.ProxyCache) error
}

// Observation is the registry's current truth about a target.
type Observation struct {
	Mode  product.ReplicationMode
	Drift Drift

	// RepositoryState is Quay's repository state, for a mirror target.
	RepositoryState string

	// Mirror is what Quay reports, when there is anything to report.
	Mirror *quay.MirrorState
	// SyncStatus is the normalised status; SyncStatusRaw is Quay's own word,
	// kept so an unrecognised value is reported rather than flattened.
	SyncStatus    quay.SyncStatus
	SyncStatusRaw string

	Proxy *quay.ProxyCache
}

// Observe reads the registry's current state for one target and computes drift
// against the desired state.
//
// Read-only, always: nothing here writes, so it is safe to call on every
// reload, from `products check`, and from a metrics sweep. That property is
// what makes continuous drift DETECTION acceptable when continuous
// reconciliation is not (docs/design/18 section 8).
func Observe(ctx context.Context, api ManagementAPI, d *Desired, applied Applied) (*Observation, error) {
	obs := &Observation{Mode: d.Mode}

	switch d.Mode {
	case product.ReplicationMirror:
		state := ""
		if repo, err := api.GetRepository(ctx, d.Namespace, d.Name); err == nil {
			state = repo.State
		} else if !errors.Is(err, registry.ErrNotFound) {
			return nil, fmt.Errorf("read repository %s/%s: %w", d.Namespace, d.Name, err)
		}
		obs.RepositoryState = state

		mirror, err := api.GetMirror(ctx, d.Namespace, d.Name)
		if err != nil && !errors.Is(err, registry.ErrNotFound) {
			return nil, fmt.Errorf("read mirror %s/%s: %w", d.Namespace, d.Name, err)
		}
		obs.Mirror = mirror
		if mirror != nil {
			obs.SyncStatus = mirror.Status()
			obs.SyncStatusRaw = mirror.RawSyncStatus
		}
		obs.Drift = DiffMirror(d.Mirror, mirror, state, applied.SecretHash, d.SecretHash)

	case product.ReplicationProxy:
		cache, err := api.GetProxyCache(ctx, d.Organization)
		if err != nil && !errors.Is(err, registry.ErrNotFound) {
			return nil, fmt.Errorf("read proxy cache %s: %w", d.Organization, err)
		}
		obs.Proxy = cache
		obs.Drift = DiffProxy(d.Proxy, cache, applied.SecretHash, d.SecretHash)
	}
	return obs, nil
}

// Plan is what an apply WOULD do, rendered for a human to approve.
//
// Produced by the same code path that performs the apply, so the preview
// cannot drift from the act.
type Plan struct {
	Target string
	Mode   product.ReplicationMode

	// Steps are the operations in order, described in the reader's terms.
	Steps []string
	// Destructive marks a plan that will make a repository read-only and
	// delete tags the glob does not match. Separated from Steps so a caller
	// cannot render the plan without also being able to render the warning.
	Destructive bool
	// TagsLost names what is present at the destination now and would not
	// survive the glob, when we are able to tell. Empty means we could not
	// enumerate rather than that nothing is at risk, and callers must say so.
	Destroys []string

	Drift Drift
	// NoOp means the registry already says what Git says.
	NoOp bool
}

// PlanApply computes what applying this desired state would change.
func PlanApply(ctx context.Context, api ManagementAPI, d *Desired, applied Applied) (*Plan, error) {
	obs, err := Observe(ctx, api, d, applied)
	if err != nil {
		return nil, err
	}
	p := &Plan{Target: d.Target, Mode: d.Mode, Drift: obs.Drift}

	switch d.Mode {
	case product.ReplicationCopy:
		p.NoOp = true
		p.Steps = append(p.Steps, "nothing to apply: this target is written to by our own workers")
		return p, nil

	case product.ReplicationMirror:
		// Checked directly rather than through Drift.StateWrong, because a
		// repository with NO mirror configuration reports Absent and carries
		// no field differences - and a NORMAL repository still has to be
		// flipped. Reading the state question out of the drift result would
		// silently drop the destructive step in exactly that case, which is
		// the first apply and therefore the most common one.
		if obs.RepositoryState == "" {
			p.Steps = append(p.Steps,
				fmt.Sprintf("repository %s/%s does not exist yet; Quay creates it on the first sync",
					d.Namespace, d.Name))
		} else if !strings.EqualFold(obs.RepositoryState, string(quay.StateMirror)) {
			p.Destructive = true
			p.Steps = append(p.Steps, fmt.Sprintf(
				"set repository %s/%s state %s -> MIRROR, which makes it READ-ONLY to everyone but %s",
				d.Namespace, d.Name, obs.RepositoryState, d.Mirror.RobotUsername))
		}
		if obs.Mirror == nil {
			p.Steps = append(p.Steps, fmt.Sprintf(
				"create the mirror configuration: pull %s every %s, tags %s",
				d.Mirror.ExtRef, secondsStr(d.Mirror.SyncInterval), renderList(d.Mirror.RootRule.RuleValue)))
			p.Destructive = true
		} else if len(obs.Drift.Fields) > 0 || obs.Drift.CredentialsRotated {
			p.Steps = append(p.Steps, "update the mirror configuration:")
			for _, f := range obs.Drift.Fields {
				p.Steps = append(p.Steps, "  "+f.String())
				if f.Field == "tags" {
					// Narrowing a glob is the destructive edit, and it is the
					// one that looks harmless in a diff.
					p.Destructive = true
				}
			}
			if obs.Drift.CredentialsRotated {
				p.Steps = append(p.Steps, "  sourceCredentials: rotated since the last apply")
			}
		}

	case product.ReplicationProxy:
		if obs.Proxy == nil {
			p.Steps = append(p.Steps, fmt.Sprintf(
				"create the proxy cache on organization %s, fronting %s with a %s window",
				d.Organization, d.Proxy.UpstreamRegistry, secondsStr(d.Proxy.ExpirationS)))
		} else if len(obs.Drift.Fields) > 0 || obs.Drift.CredentialsRotated {
			// Quay has no proxy-cache update: changing one is a delete and a
			// create, and between the two the cache is empty. Saying so is the
			// difference between a planned change and a surprise cache miss
			// storm on a production cluster.
			p.Steps = append(p.Steps, fmt.Sprintf(
				"replace the proxy cache on organization %s (Quay has no update: it is deleted and recreated, and the cache is empty until it refills):",
				d.Organization))
			for _, f := range obs.Drift.Fields {
				p.Steps = append(p.Steps, "  "+f.String())
			}
			p.Destructive = true
		}
	}

	if len(p.Steps) == 0 {
		p.NoOp = true
		p.Steps = append(p.Steps, "no change: the registry already says what the configuration says")
	}
	return p, nil
}

// Apply writes the desired state to the registry.
//
// Never called by a configuration reload - see the decision in docs/design/18
// section 8. The caller has already shown a plan to a human, or has been told
// explicitly not to.
func Apply(ctx context.Context, api ManagementAPI, d *Desired, now time.Time) (Applied, error) {
	switch d.Mode {
	case product.ReplicationCopy:
		return Applied{}, nil

	case product.ReplicationMirror:
		// State first. A mirror configuration written to a repository that is
		// not in MIRROR state is inert: it looks configured and syncs nothing,
		// which is the worst of both outcomes.
		if err := api.SetRepositoryState(ctx, d.Namespace, d.Name, quay.StateMirror); err != nil {
			return Applied{}, fmt.Errorf("set %s/%s to MIRROR state: %w", d.Namespace, d.Name, err)
		}
		if err := api.PutMirror(ctx, d.Namespace, d.Name, *d.Mirror); err != nil {
			return Applied{}, fmt.Errorf("write mirror configuration for %s/%s: %w", d.Namespace, d.Name, err)
		}

	case product.ReplicationProxy:
		// Validate before storing. A cache that cannot reach its upstream
		// fails when a pod pulls, which is the worst moment to find out and
		// the hardest place to trace back from.
		if err := api.ValidateProxyCache(ctx, d.Organization, *d.Proxy); err != nil {
			return Applied{}, fmt.Errorf("validate proxy cache for %s: %w", d.Organization, err)
		}
		// Quay has no update verb here; an existing cache is replaced.
		if err := api.DeleteProxyCache(ctx, d.Organization); err != nil && !errors.Is(err, registry.ErrNotFound) {
			return Applied{}, fmt.Errorf("remove existing proxy cache for %s: %w", d.Organization, err)
		}
		if err := api.CreateProxyCache(ctx, d.Organization, *d.Proxy); err != nil {
			return Applied{}, fmt.Errorf("create proxy cache for %s: %w", d.Organization, err)
		}
	}

	return Applied{ConfigHash: d.ConfigHash(), SecretHash: d.SecretHash}, nil
}

// SyncResult is the outcome of asking for a sync now.
type SyncResult struct {
	// AlreadyRunning means Quay was mid-sync, which satisfies the request
	// rather than failing it.
	AlreadyRunning bool
	RequestedAt    time.Time
}

// RequestSync asks Quay to sync immediately.
func RequestSync(ctx context.Context, api ManagementAPI, d *Desired, now time.Time) (SyncResult, error) {
	if d.Mode != product.ReplicationMirror {
		return SyncResult{}, fmt.Errorf("target %q is in %s mode and has no sync to request", d.Target, d.Mode)
	}
	running, err := api.SyncNow(ctx, d.Namespace, d.Name)
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{AlreadyRunning: running, RequestedAt: now}, nil
}

// CancelSync stops an in-progress sync.
func CancelSync(ctx context.Context, api ManagementAPI, d *Desired) (bool, error) {
	if d.Mode != product.ReplicationMirror {
		return false, fmt.Errorf("target %q is in %s mode and has no sync to cancel", d.Target, d.Mode)
	}
	return api.CancelSync(ctx, d.Namespace, d.Name)
}
