package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
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

	for _, s := range p.Spec.Sources {
		if !s.IsEnabled() {
			continue
		}
		id, _ := r.catalog.ResolveRepository(ctx, productName, s.Name)
		view.Sources = append(view.Sources, transfer.RepoView{
			RepositoryID: id,
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
