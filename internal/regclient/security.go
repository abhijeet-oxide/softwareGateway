package regclient

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	// Importing the JFrog backend here is also what REGISTERS it with the
	// registry factory, via its init. The factory knows only what a
	// composition root imports, and a product declaring `type: jfrog` must
	// resolve to a backend rather than to "no registry backend is registered".
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/artifactory"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/security/anchore"
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
	// tuning is how hard to push a scanner, from the SYSTEM configuration.
	//
	// Not from the product document: how many requests a scanner tolerates is a
	// property of the scanner and the network to it, not of the software being
	// replicated. Stated per product it would be repeated in every one and
	// drift between them.
	tuning SecurityTuning

	mu sync.Mutex
	by map[string]security.Provider
}

// SecurityTuning is the operator-level half of the scanner configuration.
type SecurityTuning struct {
	Concurrency    int
	BatchSize      int
	RequestTimeout time.Duration

	// Anchore is where the deployment's Anchore lives and how hard to push it.
	// Zero - specifically an empty endpoint - means this deployment has no
	// Anchore, and a repository asking for one is told so rather than failing.
	Anchore AnchoreTuning
}

// AnchoreTuning is the deployment's Anchore, resolved from the system
// configuration.
//
// A struct of plain values rather than the config type, for the same reason
// ClientConfig is not the product type: this package must not import the
// configuration loader, and a composition root translating once is cheaper than
// a dependency that makes internal/regclient depend on koanf.
type AnchoreTuning struct {
	Endpoint string
	Username string
	Password string
	Account  string

	Concurrency    int
	RequestTimeout time.Duration
	AnalysisWait   time.Duration
	PollInterval   time.Duration

	Submit     bool
	Grouping   bool
	SBOMFormat string
}

// Available reports whether this deployment can reach an Anchore at all.
func (a AnchoreTuning) Available() bool { return strings.TrimSpace(a.Endpoint) != "" }

// NewSecurityResolver builds a resolver over an existing client factory.
func NewSecurityResolver(clients *Clients, tuning SecurityTuning) *SecurityResolver {
	return &SecurityResolver{clients: clients, tuning: tuning, by: map[string]security.Provider{}}
}

// ProviderFor implements security.Resolver.
//
// The repository is named as it is CONFIGURED - "vendor-jfrog" - because that
// is the unit an operator switches a scanner on for and the unit storage is
// scoped by. The scope's Provider names WHICH scanner: a repository may have
// both Xray and Anchore switched on, and each answers into its own scope.
func (r *SecurityResolver) ProviderFor(ctx context.Context, scope security.Scope) (security.Provider, error) {
	key := scope.Product + "|" + scope.Repository + "|" + scope.Provider

	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.by[key]; ok {
		return p, nil
	}

	p, err := r.build(ctx, scope)
	if err != nil {
		return nil, err
	}
	r.by[key] = p
	return p, nil
}

// ProvidersFor implements security.Resolver: which scanners answer for one
// configured repository, worst-informed last.
//
// Xray first because it is the one that indexes a repository and therefore
// answers without being asked to do anything first; Anchore second because a
// first sync of a release spends minutes there waiting for analysis, and a
// reader watching a transcript should see the fast answer arrive before the
// slow one starts.
func (r *SecurityResolver) ProvidersFor(
	_ context.Context, productName, repository string,
) ([]string, error) {
	p, ok := r.clients.products.Get(productName)
	if !ok {
		return nil, fmt.Errorf("product %q is not configured", productName)
	}
	_, endpoint, ok := findEndpoint(p, repository)
	if !ok {
		return nil, nil
	}

	var out []string
	if endpoint.regType.IsJFrog() && endpoint.xrayEnabled {
		out = append(out, providerXray)
	}
	// The deployment's own switch comes first. A product asking for a scanner
	// this Coordinator has no address for must not produce a provider that
	// fails every request; it produces no provider, and the interface says the
	// deployment has no Anchore.
	if endpoint.anchoreEnabled && r.tuning.Anchore.Available() {
		out = append(out, anchore.ProviderName)
	}
	return out, nil
}

// providerXray is the Xray provider's stable name, mirrored here so this file
// can name it without importing the plugin's unexported constant.
const providerXray = "jfrog-xray"

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

func (r *SecurityResolver) build(_ context.Context, scope security.Scope) (security.Provider, error) {
	productName, repository := scope.Product, scope.Repository
	p, ok := r.clients.products.Get(productName)
	if !ok {
		return nil, fmt.Errorf("product %q is not configured", productName)
	}

	role, endpoint, ok := findEndpoint(p, repository)
	if !ok {
		return nil, fmt.Errorf("%w: product %q has no repository %q",
			security.ErrNoProvider, productName, repository)
	}

	// Anchore is not a JFrog endpoint and does not go through the checks below:
	// it pulls over the registry API, from its own network, with its own
	// credential. Everything it needs is the deployment's stanza plus this
	// repository's registry path.
	if scope.Provider == anchore.ProviderName {
		return r.buildAnchore(p, repository, endpoint)
	}

	// A repository that is not JFrog has no XRAY in this implementation. An
	// ordinary answer, not a failure: a Quay source with no scanner is a
	// correctly configured system, and the caller renders it as such.
	if !endpoint.regType.IsJFrog() {
		return security.Disabled{
			Reason: fmt.Sprintf(
				"JFrog Xray is available on JFrog repositories, and %q is configured as %s.",
				repository, describeType(endpoint.regType)),
		}, nil
	}

	if !endpoint.xrayEnabled {
		return security.Disabled{
			ProviderName: providerXray,
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
		Enabled:  true,
		Endpoint: endpoint.xrayEndpoint,
		// DERIVED, never declared. Artifactory addresses content as
		// `<repoKey>/<path>`, so the repository this document already names
		// begins with its own key - and a second place to state that is the
		// place that goes stale.
		RepositoryKey:  product.XrayRepositoryKey(endpoint.repository),
		Concurrency:    r.tuning.Concurrency,
		BatchSize:      r.tuning.BatchSize,
		RequestTimeout: r.tuning.RequestTimeout,
	}
	return artifactory.NewXrayProvider(artifactory.XrayConfigFor(cfg, settings), settings)
}

// buildAnchore builds the Anchore provider for one configured repository.
//
// # What it takes from where, and why none of it is duplicated
//
// The ADDRESS and the CREDENTIAL come from the deployment's stanza, because
// there is one Anchore in an estate and a per-product copy of its host is a set
// of copies that drift. The REGISTRY PATH comes from the repository, because
// that is what Anchore is told to pull and it is already written down. The
// APPLICATION and VERSION names are supplied per release by the caller, because
// a provider is built per repository and a release is not.
func (r *SecurityResolver) buildAnchore(
	p *product.Product, repository string, endpoint resolvedEndpoint,
) (security.Provider, error) {
	if !endpoint.anchoreEnabled {
		return security.Disabled{
			ProviderName: anchore.ProviderName,
			Reason:       fmt.Sprintf("Anchore is not enabled for repository %q.", repository),
		}, nil
	}
	tuning := r.tuning.Anchore
	if !tuning.Available() {
		// The product asked for a scanner this deployment has no address for.
		// A disabled provider with a sentence naming the knob, rather than a
		// live one that fails every request against an empty URL.
		return security.Disabled{
			ProviderName: anchore.ProviderName,
			Reason: fmt.Sprintf(
				"Repository %q asks for Anchore, and this Coordinator has none configured. "+
					"Set coordinator.security.anchore.endpoint.", repository),
		}, nil
	}

	// The network path to Anchore is the PRODUCT's, not the repository's.
	//
	// A repository's own network block describes how to reach that registry -
	// a vendor's proxy, a vendor's CA - and Anchore is neither. The product's
	// block is the estate's own network settings, which is what reaching an
	// internal service uses.
	network, err := product.ResolveNetwork(p, nil, r.clients.secrets)
	if err != nil {
		return nil, fmt.Errorf("anchore: resolve network for product %q: %w", p.Metadata.Name, err)
	}

	cfg := anchore.Config{
		Endpoint:              tuning.Endpoint,
		Username:              tuning.Username,
		Password:              tuning.Password,
		Account:               tuning.Account,
		CABundle:              network.CABundle,
		HTTPSProxy:            network.HTTPSProxy,
		NoProxy:               network.NoProxy,
		DirectConnect:         network.DirectConnect,
		InsecureSkipVerify:    network.InsecureSkipVerify,
		ConnectTimeout:        network.ConnectTimeout,
		ResponseHeaderTimeout: network.ResponseHeaderTimeout,
		RequestTimeout:        tuning.RequestTimeout,
		UserAgent:             anchoreUserAgent(),
		Logger:                r.clients.log,
	}
	settings := anchore.Settings{
		Enabled:      true,
		Registry:     endpoint.registry,
		Repository:   endpoint.repository,
		Concurrency:  tuning.Concurrency,
		AnalysisWait: tuning.AnalysisWait,
		PollInterval: tuning.PollInterval,
		Submit:       tuning.Submit,
		Grouping:     tuning.Grouping,
		SBOMFormat:   tuning.SBOMFormat,
	}
	return anchore.New(cfg, settings)
}

// anchoreUserAgent identifies this platform to Anchore, so an operator reading
// Anchore's own access log can tell our submissions from anybody else's.
func anchoreUserAgent() string {
	return "softwaregateway/" + version.Get("coordinator").Version
}

// resolvedEndpoint is the part of a configured repository this file needs.
type resolvedEndpoint struct {
	registry       string
	repository     string
	regType        product.RegistryType
	xrayEnabled    bool
	xrayEndpoint   string
	anchoreEnabled bool
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
			registry: s.Registry, repository: repo, regType: s.Type,
			xrayEnabled: product.XrayIsEnabled(s.XrayEnabled), xrayEndpoint: s.XrayEndpoint,
			anchoreEnabled: product.AnchoreIsEnabled(s.AnchoreEnabled),
		}, true
	}
	for _, t := range p.Spec.Targets {
		if t.Name != name {
			continue
		}
		return product.RoleTarget, resolvedEndpoint{
			registry: t.Registry, repository: t.Repository, regType: t.Type,
			xrayEnabled: product.XrayIsEnabled(t.XrayEnabled), xrayEndpoint: t.XrayEndpoint,
			anchoreEnabled: product.AnchoreIsEnabled(t.AnchoreEnabled),
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
		if s.Type.IsJFrog() && product.XrayIsEnabled(s.XrayEnabled) {
			return true
		}
	}
	for _, t := range p.Spec.Targets {
		if t.Type.IsJFrog() && product.XrayIsEnabled(t.XrayEnabled) {
			return true
		}
	}
	return false
}

// ScannerEnabledFor reports whether a product has ANY scanner switched on.
//
// The question XrayEnabledFor used to answer, and the reason it can no longer
// answer it: a product scanned only by Anchore has no Xray anywhere and is not
// a product without a scanner. Every caller asking "does this product have
// security at all" wants this one.
func ScannerEnabledFor(p *product.Product) bool {
	return XrayEnabledFor(p) || product.AnchoreEnabledAnywhere(p)
}

// SecurityRepositories lists the configured repositories of a product that have
// a scanner, so a caller can say which ones contribute.
func SecurityRepositories(p *product.Product) []string {
	var out []string
	for _, c := range securityCandidates(p) {
		out = append(out, c.Name)
	}
	return out
}

var _ security.Resolver = (*SecurityResolver)(nil)

// ScannedRepository is a configured repository that could answer for a release,
// and which scanners answer there.
type ScannedRepository struct {
	Role       product.Role
	Name       string
	Registry   string
	Repository string
	// XrayEndpoint is the platform base URL override, where one is configured.
	XrayEndpoint string
	// Providers names every scanner switched on for this repository.
	//
	// A list rather than a flag, because that is the change a second scanner
	// makes: "the scanner for this repository" was a question with one answer
	// until it was not, and a caller that reads a flag would silently ask only
	// the first of two.
	Providers []string
}

// HasProvider reports whether one named scanner answers here.
func (s ScannedRepository) HasProvider(name string) bool {
	for _, p := range s.Providers {
		if p == name {
			return true
		}
	}
	return false
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
//
// A repository qualifies if EITHER scanner is on, and it carries both. Before
// there was a second scanner this filtered on `type: jfrog` as well as on the
// switch - which is right for Xray, because Xray is a JFrog endpoint, and wrong
// for Anchore, which pulls over the registry API from any registry it can
// reach.
func securityCandidates(p *product.Product) []ScannedRepository {
	var out []ScannedRepository
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		providers := providersOn(t.Type, t.XrayEnabled, t.AnchoreEnabled)
		if len(providers) == 0 {
			continue
		}
		out = append(out, ScannedRepository{
			Role: product.RoleTarget, Name: t.Name,
			Registry: t.Registry, Repository: t.Repository, XrayEndpoint: t.XrayEndpoint,
			Providers: providers,
		})
	}
	for _, s := range p.Spec.Sources {
		if !s.IsEnabled() {
			continue
		}
		providers := providersOn(s.Type, s.XrayEnabled, s.AnchoreEnabled)
		if len(providers) == 0 {
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
			XrayEndpoint: s.XrayEndpoint, Providers: providers,
		})
	}
	return out
}

// providersOn is which scanners one repository's switches ask for.
//
// Xray first, and see ProvidersFor for why: it answers without being asked to
// do anything first, and a reader watching a transcript should see the fast
// answer arrive before the slow one starts.
func providersOn(typ product.RegistryType, xray, anchoreOn *bool) []string {
	var out []string
	if typ.IsJFrog() && product.XrayIsEnabled(xray) {
		out = append(out, providerXray)
	}
	if product.AnchoreIsEnabled(anchoreOn) {
		out = append(out, anchore.ProviderName)
	}
	return out
}
