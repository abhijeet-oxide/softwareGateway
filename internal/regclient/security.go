package regclient

import (
	"context"
	"fmt"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	// Importing the JFrog backend here is also what REGISTERS it with the
	// registry factory, via its init. The factory knows only what a
	// composition root imports, and a product declaring `type: jfrog` must
	// resolve to a backend rather than to "no registry backend is registered".
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/artifactory"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// SecurityResolver builds and caches security providers, one per configured
// repository.
//
// It lives here, beside the registry client factory, for the reason the whole
// package exists: "which credential, which CA bundle, which proxy" must be
// answered once. A security provider that resolved its own would be reaching
// the same JFrog by a different route from the one that replicates from it,
// and the day those two disagree is the day Xray reports nothing while
// replication works perfectly.
type SecurityResolver struct {
	clients *Clients

	mu sync.Mutex
	by map[string]security.Provider
}

// NewSecurityResolver builds a resolver over an existing client factory.
func NewSecurityResolver(clients *Clients) *SecurityResolver {
	return &SecurityResolver{clients: clients, by: map[string]security.Provider{}}
}

// ProviderFor implements security.Resolver.
//
// The repository is named as it is CONFIGURED - "vendor-jfrog" - because that
// is the unit an operator switches Xray on for and the unit the cache is
// scoped by. Roles are tried source-first: a release is read where it was
// discovered, and a name that exists in both roles almost always means one
// registry declared twice.
func (r *SecurityResolver) ProviderFor(ctx context.Context, productName, repository string) (security.Provider, error) {
	key := productName + "|" + repository

	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.by[key]; ok {
		return p, nil
	}

	p, err := r.build(ctx, productName, repository)
	if err != nil {
		return nil, err
	}
	r.by[key] = p
	return p, nil
}

// Invalidate drops cached providers, for a configuration reload.
//
// Without it a `xrayEnabled: false` would take effect only on restart, which
// is the wrong behaviour for a switch whose entire purpose is to stop this
// process talking to a third system.
func (r *SecurityResolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.by = map[string]security.Provider{}
}

func (r *SecurityResolver) build(_ context.Context, productName, repository string) (security.Provider, error) {
	p, ok := r.clients.products.Get(productName)
	if !ok {
		return nil, fmt.Errorf("product %q is not configured", productName)
	}

	role, endpoint, ok := findEndpoint(p, repository)
	if !ok {
		return nil, fmt.Errorf("%w: product %q has no repository %q",
			security.ErrNoProvider, productName, repository)
	}

	// A repository that is not JFrog has no scanner in this implementation.
	// An ordinary answer, not a failure: a Quay source with no scanner is a
	// correctly configured system, and the caller renders it as such.
	if !endpoint.regType.IsJFrog() {
		return security.Disabled{
			Reason: fmt.Sprintf(
				"Security scanning is available on JFrog repositories, and %q is configured as %s.",
				repository, describeType(endpoint.regType)),
		}, nil
	}

	if !endpoint.xray.IsEnabled() {
		return security.Disabled{
			ProviderName: "jfrog-xray",
			Reason:       fmt.Sprintf("JFrog Xray is not enabled for repository %q.", repository),
		}, nil
	}

	cfg, err := ConfigFor(p, r.clients.secrets, v1.JobEndpoint{
		Product:    productName,
		Role:       string(role),
		Name:       repository,
		Registry:   endpoint.registry,
		Repository: endpoint.repository,
		Type:       string(endpoint.regType),
	})
	if err != nil {
		return nil, err
	}
	cfg.Logger = r.clients.log

	// Every transport-shaped value here came from ConfigFor - the same
	// resolution a transfer gets. Nothing about reaching this JFrog is decided
	// twice, and nothing about the credential is decided here at all.
	settings := artifactory.XraySettings{
		Enabled:        true,
		Endpoint:       endpoint.xray.Endpoint,
		RepositoryKey:  endpoint.xray.RepositoryKey,
		Watches:        endpoint.xray.Watches,
		Concurrency:    endpoint.xray.ConcurrencyOrDefault(),
		BatchSize:      endpoint.xray.BatchSizeOrDefault(),
		RequestTimeout: endpoint.xray.TimeoutOrDefault(),
	}
	return artifactory.NewXrayProvider(artifactory.XrayConfigFor(cfg, settings), settings)
}

// resolvedEndpoint is the part of a configured repository this file needs.
type resolvedEndpoint struct {
	registry   string
	repository string
	regType    product.RegistryType
	xray       *product.Xray
}

// findEndpoint looks up a configured repository by name, in either role.
func findEndpoint(p *product.Product, name string) (product.Role, resolvedEndpoint, bool) {
	for _, s := range p.Spec.Sources {
		if s.Name != name {
			continue
		}
		repo := s.Repository
		if repo == "" {
			if declared := s.DeclaredRepositories(); len(declared) > 0 {
				repo = declared[0]
			}
		}
		return product.RoleSource, resolvedEndpoint{
			registry: s.Registry, repository: repo, regType: s.Type, xray: s.Xray,
		}, true
	}
	for _, t := range p.Spec.Targets {
		if t.Name != name {
			continue
		}
		return product.RoleTarget, resolvedEndpoint{
			registry: t.Registry, repository: t.Repository, regType: t.Type, xray: t.Xray,
		}, true
	}
	return "", resolvedEndpoint{}, false
}

func describeType(t product.RegistryType) string {
	if t == "" {
		return string(product.RegistryGeneric) + " (the default)"
	}
	return string(t)
}

// XrayEnabledFor reports whether a product has Xray switched on anywhere.
//
// Used by the deep health check and by the interface, which needs to tell
// "this product has no scanner" apart from "this release has not been scanned"
// without building a provider for every repository first.
func XrayEnabledFor(p *product.Product) bool {
	for _, s := range p.Spec.Sources {
		if s.Type.IsJFrog() && s.Xray.IsEnabled() {
			return true
		}
	}
	for _, t := range p.Spec.Targets {
		if t.Type.IsJFrog() && t.Xray.IsEnabled() {
			return true
		}
	}
	return false
}

// SecurityRepositories lists the configured repositories of a product that have
// a scanner, so a caller can say which ones contribute.
func SecurityRepositories(p *product.Product) []string {
	var out []string
	for _, s := range p.Spec.Sources {
		if s.Type.IsJFrog() && s.Xray.IsEnabled() {
			out = append(out, s.Name)
		}
	}
	for _, t := range p.Spec.Targets {
		if t.Type.IsJFrog() && t.Xray.IsEnabled() {
			out = append(out, t.Name)
		}
	}
	return out
}

// TTLFor resolves the cache retention a repository's configuration asks for.
func TTLFor(p *product.Product, role product.Role, repository string) security.CacheTTL {
	x, _ := p.XrayFor(role, repository)
	return security.CacheTTL{
		Summary: x.SummaryTTLOrDefault(),
		Detail:  x.DetailTTLOrDefault(),
	}
}

var _ security.Resolver = (*SecurityResolver)(nil)

// ScannedRepository is a configured repository that could answer for a release.
type ScannedRepository struct {
	Role       product.Role
	Name       string
	Registry   string
	Repository string
}

// SecurityRepositoryFor chooses which configured repository to ask about a
// release.
//
// # Why this is not "the repository it was discovered in"
//
// Because that is the VENDOR'S registry, and the vendor does not run your
// scanner. In the ordinary estate a release is discovered on a vendor registry -
// Nokia NEAR, say - and replicated into JFrog, and it is the JFrog copy that
// Xray has indexed. Scoping the security read to the source repository finds no
// scanner at all and reports every release as "no scanner configured", on an
// estate where Xray is switched on and working.
//
// That was the first shape of this and it was wrong on the only topology that
// matters.
//
// # The order, and why it is that order
//
//  1. A target the release has actually REACHED. The scanner can only have
//     indexed a copy that exists, and a target the release was never
//     transferred to holds nothing to index.
//  2. The default target. A release queued but not yet transferred will land
//     there, and naming it is a better answer than naming nothing - the sync
//     then reports "not scanned", which is true and actionable.
//  3. Any remaining scanner-enabled repository, targets before sources, so a
//     single-target product needs no ordering rule of its own.
//
// `reached` is the set of target names the release has been transferred to,
// supplied by the caller because transfer history is the store's knowledge and
// this package holds configuration.
func SecurityRepositoryFor(p *product.Product, reached []string) (ScannedRepository, bool) {
	candidates := securityCandidates(p)
	if len(candidates) == 0 {
		return ScannedRepository{}, false
	}

	inReach := make(map[string]bool, len(reached))
	for _, name := range reached {
		inReach[name] = true
	}

	// 1: a target this release actually reached.
	for _, c := range candidates {
		if c.Role == product.RoleTarget && inReach[c.Name] {
			return c, true
		}
	}
	// 2: the default target.
	if def, ok := p.DefaultTarget(); ok {
		for _, c := range candidates {
			if c.Role == product.RoleTarget && c.Name == def.Name {
				return c, true
			}
		}
	}
	// 3: anything left, targets first.
	for _, c := range candidates {
		if c.Role == product.RoleTarget {
			return c, true
		}
	}
	return candidates[0], true
}

// securityCandidates lists the repositories of a product that have a scanner
// switched on, targets first.
func securityCandidates(p *product.Product) []ScannedRepository {
	var out []ScannedRepository
	for _, t := range p.Spec.Targets {
		if t.IsEnabled() && t.Type.IsJFrog() && t.Xray.IsEnabled() {
			out = append(out, ScannedRepository{
				Role: product.RoleTarget, Name: t.Name,
				Registry: t.Registry, Repository: t.Repository,
			})
		}
	}
	for _, s := range p.Spec.Sources {
		if !s.IsEnabled() || !s.Type.IsJFrog() || !s.Xray.IsEnabled() {
			continue
		}
		repo := s.Repository
		if repo == "" {
			if declared := s.DeclaredRepositories(); len(declared) > 0 {
				repo = declared[0]
			}
		}
		out = append(out, ScannedRepository{
			Role: product.RoleSource, Name: s.Name, Registry: s.Registry, Repository: repo,
		})
	}
	return out
}
