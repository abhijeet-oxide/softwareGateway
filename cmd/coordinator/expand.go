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

// resolver answers the three questions the expander cannot answer itself:
// where a request should go, how to read its source, and what accessories
// travel with it.
//
// It lives in the composition root because it is where configuration, the
// catalog and the registry client factory meet — and internal/transfer must
// depend on none of those, or the planner could not be tested without them.
type resolverImpl struct {
	products *product.Registry
	catalog  *catalog.Catalog
	packages *store.Packages
	clients  *regclient.Clients
	log      *slog.Logger
}

// Destinations returns the enabled targets a request should expand into.
//
// A `promote` request is deliberately NOT handled here: promotion's source is
// another target and its destination is chosen by the request rather than by
// "every enabled target", so treating it like replication would fan a
// promotion out across every environment at once. It arrives with the rest of
// promotion in M4; until then a promote request expands into nothing and says
// so, which is a visible non-answer rather than a wrong one.
func (r *resolverImpl) Destinations(
	ctx context.Context, productName, operation string,
) ([]transfer.Destination, error) {
	if operation == "promote" {
		return nil, fmt.Errorf(
			"promotion is not implemented yet: a promote request cannot be expanded (M4)")
	}

	p, ok := r.products.Get(productName)
	if !ok {
		return nil, fmt.Errorf("product %q is not configured", productName)
	}

	var out []transfer.Destination
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		// promotionOnly means a production registry is reachable only by
		// promotion from another target, never by direct replication from a
		// vendor. Skipping it here is what enforces that.
		if t.PromotionOnly {
			continue
		}

		id, err := r.catalog.ResolveRepository(ctx, productName, t.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, transfer.Destination{
			RepositoryID: id, Name: t.Name, Repository: t.Repository,
			Registry: t.Registry, Type: string(t.Type),
		})
	}
	return out, nil
}

// SourceReader builds a client for the repository a request names.
//
// The repository row already carries everything needed to identify it, so this
// resolves the row and hands it to the same client factory the worker uses.
// Sharing that factory is the point: a source the Coordinator can walk is a
// source a worker can pull from, and if the two disagreed the failure would
// appear as a transfer that plans perfectly and then cannot fetch a byte.
func (r *resolverImpl) SourceReader(
	ctx context.Context, productName string, repoID int64,
) (registry.ManifestReader, error) {
	endpoints, err := r.packages.HydrateEndpoints(ctx, []int64{repoID})
	if err != nil {
		return nil, err
	}
	e, ok := endpoints[repoID]
	if !ok {
		return nil, fmt.Errorf("repository %d is not in the catalog", repoID)
	}

	return r.clients.For(v1.JobEndpoint{
		Product:    e.Product,
		Name:       e.Name,
		Registry:   e.Registry,
		Repository: e.Repository,
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
