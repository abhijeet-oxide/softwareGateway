package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// resolverImpl joins configuration, the catalog and the registry client
// factory — the three things neither the planner nor the requester may import.
//
// It lives in the composition root because that is where those three meet, and
// because internal/transfer staying free of them is what lets the planner and
// the resolution rules be tested with literals instead of a config loader.
type resolverImpl struct {
	products *product.Registry
	catalog  *catalog.Catalog
	packages *store.Packages
	clients  *regclient.Clients
	log      *slog.Logger
}

// ProductView is what `transfers create` and `transfers promote` resolve
// against: configured sources and targets, joined to their catalog rows.
//
// A target with no catalog row yet is included with a zero ID rather than
// omitted — configuration declares it, so naming it must produce "nothing has
// been written there yet" rather than "no such target".
func (r *resolverImpl) ProductView(
	ctx context.Context, productName string,
) (transfer.ProductView, error) {
	p, ok := r.products.Get(productName)
	if !ok {
		return transfer.ProductView{}, fmt.Errorf("product %q is not configured", productName)
	}

	from, to := p.PromotionPath()
	view := transfer.ProductView{Name: productName, PromotionFrom: from, PromotionTo: to}

	// Sources carry no row ID. A source that enumerates the registry's catalog
	// owns a row PER REPOSITORY it found, named "<source>/<path>", so there is
	// no single row a configured source name maps to — and pretending
	// otherwise is what produced a zero ID and a foreign-key violation. A
	// replication resolves its origin from the package instead.
	for _, s := range p.Spec.Sources {
		if !s.IsEnabled() {
			continue
		}
		view.Sources = append(view.Sources, transfer.RepoView{
			Name:         s.Name,
			Role:         string(product.RoleSource),
			Registry:     s.Registry,
			RegistryType: string(s.Type),
		})
	}

	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		// A target that has never received a transfer has no row yet. Zero
		// here is not an error — the requester creates one when something is
		// actually copied there.
		id, _ := r.catalog.ResolveRepository(ctx, productName, t.Name)
		view.Targets = append(view.Targets, transfer.RepoView{
			RepositoryID:  id,
			Name:          t.Name,
			Role:          string(product.RoleTarget),
			Environment:   t.Environment,
			Registry:      t.Registry,
			Repository:    t.Repository,
			RegistryType:  string(t.Type),
			PromotionOnly: t.PromotionOnly,
			Default:       t.Default,
		})
	}
	return view, nil
}

// Reader builds a client for the repository a transfer reads from.
//
// The ROW supplies the registry and the credential; the PATH is passed
// separately, because a promotion reads from beneath its origin target's
// configured prefix rather than from the prefix itself. Sharing this factory
// with the worker is the point: a repository the Coordinator can walk is one a
// worker can pull from, and if the two disagreed the failure would appear as a
// transfer that plans perfectly and then cannot fetch a byte.
func (r *resolverImpl) Reader(
	ctx context.Context, repoID int64, repositoryPath string,
) (registry.ManifestReader, error) {
	endpoints, err := r.packages.HydrateEndpoints(ctx, []int64{repoID})
	if err != nil {
		return nil, err
	}
	e, ok := endpoints[repoID]
	if !ok {
		return nil, fmt.Errorf("repository %d is not in the catalog", repoID)
	}
	if repositoryPath == "" {
		repositoryPath = e.Repository
	}

	return r.clients.For(v1.JobEndpoint{
		Product:    e.Product,
		Name:       e.Name,
		Registry:   e.Registry,
		Repository: repositoryPath,
		Type:       e.RegistryType,
		Role:       e.Role,
	})
}

// Related returns the accessories that travel with a package.
//
// Nothing yet, and deliberately nothing rather than a guess. The planner does
// not need it: where a vendor bundles the payload with its signature under a
// wrapper index, discovery records the WRAPPER as the transfer root, so
// walking from that root already reaches both — see internal/expand.Root. The
// hook exists for the M5 case where a signature is discovered by referrers and
// has no place in the payload's tree.
func (r *resolverImpl) Related(
	context.Context, string, store.PackageRow,
) ([]vendors.Related, error) {
	return nil, nil
}

// expanderAdapter narrows transfer.Expander to what queue.Controller needs.
//
// The controller takes an interface of one method so internal/queue does not
// import internal/transfer; this is the two-line adapter that satisfies it.
type expanderAdapter struct{ e *transfer.Expander }

func (a expanderAdapter) Expand(ctx context.Context) (int, int, error) {
	res, err := a.e.Expand(ctx)
	return res.Requests, res.Jobs, err
}

// RepositoryByID resolves an existing catalog row.
//
// This is how a replication learns its origin: the PACKAGE names the row it
// was discovered in, which is authoritative in a way a configured source name
// is not — one source can own many rows.
func (r *resolverImpl) RepositoryByID(
	ctx context.Context, repositoryID int64,
) (transfer.RepoView, error) {
	endpoints, err := r.packages.HydrateEndpoints(ctx, []int64{repositoryID})
	if err != nil {
		return transfer.RepoView{}, err
	}
	e, ok := endpoints[repositoryID]
	if !ok {
		return transfer.RepoView{}, fmt.Errorf("repository %d is not in the catalog", repositoryID)
	}
	return transfer.RepoView{
		RepositoryID: e.RepositoryID,
		Name:         e.Name,
		Role:         e.Role,
		Registry:     e.Registry,
		Repository:   e.Repository,
		RegistryType: e.RegistryType,
	}, nil
}

// EnsureTarget gives a configured target a catalog row.
//
// Created on first use rather than at reconciliation, because a target that
// has never received anything has nothing to point at — and a transfer needs a
// row before it can name one.
func (r *resolverImpl) EnsureTarget(
	ctx context.Context, productName string, target transfer.RepoView,
) (int64, error) {
	p, ok := r.products.Get(productName)
	if !ok {
		return 0, fmt.Errorf("product %q is not configured", productName)
	}

	tx, err := r.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin target registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// productID comes from the loaded configuration's row, which reconciliation
	// keeps current — the same row every other repository of this product hangs
	// off.
	productID, err := r.productID(ctx, p.Metadata.Name)
	if err != nil {
		return 0, err
	}

	id, err := r.packages.EnsureRepository(ctx, tx, productID,
		string(product.RoleTarget), target.Name, target.Registry, target.Repository,
		target.RegistryType, "config", "")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit target registration: %w", err)
	}
	return id, nil
}

// productID finds the active products row for a configured product.
func (r *resolverImpl) productID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := r.packages.DB().QueryRowContext(ctx,
		r.packages.Dialect().Rewrite(
			`SELECT id FROM products WHERE name = ? AND active = `+
				r.packages.Dialect().Bool(true)+` LIMIT 1`), name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("product %q has no active catalog row: %w", name, err)
	}
	return id, nil
}

// ResolveAtTarget answers what a tag currently points at in a target.
//
// This is the destination walk that turns a delegated transfer's outcome into
// an OBSERVATION. The registry told us a sync succeeded; that says it did
// something, not what. Resolving the tag here and comparing the digest is the
// only way `diverged` can be told from `succeeded` at all.
//
// It reads through the same client factory as everything else, so a target
// unreachable to a transfer is unreachable to this too — which is correct: a
// confirmation that could succeed where a transfer could not would be
// confirming the wrong thing.
func (r *resolverImpl) ResolveAtTarget(
	ctx context.Context, productName, targetName, tag string,
) (string, error) {
	p, ok := r.products.Get(productName)
	if !ok {
		return "", fmt.Errorf("product %q is not loaded", productName)
	}
	t, ok := p.Target(targetName)
	if !ok {
		return "", fmt.Errorf("product %q has no target %q", productName, targetName)
	}

	reader, err := r.clients.For(v1.JobEndpoint{
		Product:    productName,
		Name:       t.Name,
		Registry:   t.Registry,
		Repository: t.Repository,
		Type:       string(t.Type),
		Role:       "target",
	})
	if err != nil {
		return "", err
	}

	desc, err := reader.ResolveTag(ctx, tag)
	if err != nil {
		return "", err
	}
	return string(desc.Digest), nil
}

// replicationMetrics adapts the metric registry to the replication package's
// narrow interface.
//
// An adapter rather than the registry itself, so internal/replication depends
// on four method names rather than on Prometheus — which is what lets every
// decision in it be tested without a registry.
type replicationMetrics struct{ m *metrics.Registry }

func (r replicationMetrics) RecordSync(product, target, result string) {
	r.m.MirrorSyncs.WithLabelValues(product, target, result).Inc()
}

func (r replicationMetrics) RecordSyncDuration(product, target string, seconds float64) {
	r.m.MirrorSyncDuration.WithLabelValues(product, target).Observe(seconds)
}

func (r replicationMetrics) SetDrift(product, target string, drifted bool) {
	v := 0.0
	if drifted {
		v = 1
	}
	r.m.MirrorConfigDrift.WithLabelValues(product, target).Set(v)
}

func (r replicationMetrics) RecordProxyProbe(product, target, result string) {
	r.m.ProxyCacheProbes.WithLabelValues(product, target, result).Inc()
}
