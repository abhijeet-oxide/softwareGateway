// Package regclient turns a job endpoint plus product configuration into a
// registry client.
//
// It exists because BOTH planes need the same translation and must not
// disagree about it. The worker resolves a leased job's two endpoints so it
// can move bytes; the Coordinator resolves a request's source so the planner
// can walk the manifest tree. Two copies of "which credential, which CA
// bundle, which proxy" is two chances for discovery to work while transfers
// fail at the TLS handshake.
//
// Credentials are resolved by NAME, from the projected secret volume this
// process mounts. That is what lets a lease response carry a credential
// REFERENCE rather than a credential: no secret is ever serialized over the
// API, and rotating one is a volume update rather than a fleet-wide restart.
package regclient

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"

	// Registers the generic backend with the factory, via its init. The worker
	// is a composition root for registry backends in the same way the
	// Coordinator is; without this import the factory knows none.
	_ "github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
)

// Clients builds and caches repository handles.
//
// CACHING IS THE POINT, not an optimization. One handle per repository means
// one connection pool, one rate limiter and one token cache for every job
// against it — and without that, an 850-blob package performs 850 token
// exchanges against the vendor's auth endpoint, adding a round trip to every
// blob and frequently tripping the registry's own rate limits. It is one of
// the highest-value small optimizations in the system and one of the easiest
// to omit by accident (docs/design/05 §5).
type Clients struct {
	products *product.Registry
	secrets  *product.SecretResolver
	log      *slog.Logger
	// sourceDir is where the products came from, carried only so a failure to
	// find one can say where it looked.
	sourceDir string

	mu     sync.Mutex
	byKey  map[string]registry.Repository
	shared map[string]*transport.Shared
}

// NewClients builds a client cache.
//
// sourceDir is where products were loaded from. It is used for one thing —
// telling a reader where this process looked when it cannot find a product —
// and an empty value simply omits that from the message.
func NewClients(
	products *product.Registry, secrets *product.SecretResolver,
	sourceDir string, log *slog.Logger,
) *Clients {
	if log == nil {
		log = slog.Default()
	}
	return &Clients{
		products:  products,
		secrets:   secrets,
		sourceDir: sourceDir,
		log:       log,
		byKey:     map[string]registry.Repository{},
		shared:    map[string]*transport.Shared{},
	}
}

// describeLoaded names the products this process can actually see.
func describeLoaded(reg *product.Registry) string {
	loaded := reg.List()
	if len(loaded) == 0 {
		return "none"
	}
	names := make([]string, 0, len(loaded))
	for _, p := range loaded {
		names = append(names, p.Metadata.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// For returns a repository client for one end of a job.
func (c *Clients) For(e v1.JobEndpoint) (registry.Repository, error) {
	key := e.Product + "|" + e.Role + "|" + e.Name + "|" + e.Registry + "|" + e.Repository

	c.mu.Lock()
	defer c.mu.Unlock()

	if cached, ok := c.byKey[key]; ok {
		return cached, nil
	}

	cfg, err := c.configFor(e)
	if err != nil {
		return nil, err
	}

	// One transport per REGISTRY, not per repository. Without this,
	// `maxConnections: 32` across sixteen repositories on one host permits 512
	// connections and `requestsPerSecond: 50` permits 800 — the configured
	// ceiling silently became a per-repository allowance.
	sharedKey := e.Product + "|" + e.Registry
	sh, ok := c.shared[sharedKey]
	if !ok {
		sh, err = transport.NewShared(sharedTransport(cfg))
		if err != nil {
			return nil, fmt.Errorf("transport for %s: %w", e.Registry, err)
		}
		c.shared[sharedKey] = sh
	}
	cfg.Shared = sh
	cfg.Logger = c.log

	src, err := registry.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client for %s/%s: %w", e.Registry, e.Repository, err)
	}

	// A worker needs the FULL contract — it writes. A backend that only reads
	// cannot serve a transfer, and finding that out here, by name, beats
	// finding it out as a nil-pointer panic mid-blob.
	repo, ok := src.(registry.Repository)
	if !ok {
		return nil, fmt.Errorf(
			"registry backend %q for %s can read but not write, so it cannot be a transfer endpoint",
			cfg.Type, e.Registry)
	}

	c.byKey[key] = repo
	return repo, nil
}

// configFor translates configuration into a client config for one endpoint.
//
// This is the only place a worker reads a credential, and the resolution is by
// NAME: the lease response said which product, which role and which configured
// repository, and the secret comes from this process's own projected volume.
func (c *Clients) configFor(e v1.JobEndpoint) (registry.ClientConfig, error) {
	p, ok := c.products.Get(e.Product)
	if !ok {
		// Say what this process DOES have. "Not configured" alone sends the
		// reader to the Coordinator, which is the wrong end: the Coordinator
		// planned the job correctly and it is this process that cannot see the
		// product. Naming the directory and what was found in it turns the
		// message into the fix.
		return registry.ClientConfig{}, fmt.Errorf(
			"product %q is not configured on this worker"+
				"\nthis worker loaded %d product(s) from %s: %s"+
				"\npoint it at the same configuration the Coordinator uses "+
				"(--config, or configDir)",
			e.Product, c.products.Count(), c.sourceDir, describeLoaded(c.products))
	}

	cfg := registry.ClientConfig{
		Registry:   e.Registry,
		Repository: strings.Trim(e.Repository, "/"),
		Type:       e.Type,
		UserAgent:  "softwaregateway/" + version.Get("worker").Version,
		PlainHTTP:  isLoopback(e.Registry),
	}

	spec, err := endpointSpec(p, e)
	if err != nil {
		return registry.ClientConfig{}, err
	}

	cfg.MaxConnections = spec.limits.PerRegistry
	cfg.RequestsPerSecond = spec.limits.RequestsPerSecond
	cfg.Burst = spec.limits.Burst()

	n, err := product.ResolveNetwork(p, spec.network, c.secrets)
	if err != nil {
		return registry.ClientConfig{}, fmt.Errorf("product %q %s %q: %w",
			e.Product, e.Role, e.Name, err)
	}
	cfg.CABundle = n.CABundle
	cfg.HTTPSProxy = n.HTTPSProxy
	cfg.NoProxy = n.NoProxy
	cfg.DirectConnect = n.DirectConnect
	cfg.InsecureSkipVerify = n.InsecureSkipVerify
	cfg.ConnectTimeout = n.ConnectTimeout
	cfg.ResponseHeaderTimeout = n.ResponseHeaderTimeout

	// Anonymous is explicit, never inferred from a missing credentialsRef. A
	// typo'd secret name must fail loudly rather than silently downgrade to
	// anonymous access and then fail later as a confusing 401.
	switch {
	case spec.anonymous:
	case spec.creds != nil:
		creds, err := c.secrets.Credentials(*spec.creds)
		if err != nil {
			return registry.ClientConfig{}, fmt.Errorf("product %q %s %q credentials: %w",
				e.Product, e.Role, e.Name, err)
		}
		cfg.Username = creds.Username
		cfg.Password = creds.Password.Reveal()
	default:
		return registry.ClientConfig{}, fmt.Errorf(
			"product %q %s %q has neither credentialsRef nor anonymous: true",
			e.Product, e.Role, e.Name)
	}

	return cfg, nil
}

// endpointSpec finds the configured source or target the job names.
type resolvedSpec struct {
	anonymous bool
	creds     *product.CredentialsRef
	network   *product.Network
	limits    product.Concurrency
}

func endpointSpec(p *product.Product, e v1.JobEndpoint) (resolvedSpec, error) {
	name := configuredName(e.Name)

	if e.Role == string(product.RoleTarget) {
		for _, t := range p.Spec.Targets {
			if t.Name == name {
				return resolvedSpec{
					anonymous: t.Anonymous, creds: t.CredentialsRef,
					network: t.Network, limits: t.Concurrency,
				}, nil
			}
		}
		return resolvedSpec{}, fmt.Errorf(
			"product %q has no target %q (from repository row %q)",
			p.Metadata.Name, name, e.Name)
	}

	for _, s := range p.Spec.Sources {
		if s.Name == name {
			return resolvedSpec{
				anonymous: s.Anonymous, creds: s.CredentialsRef,
				network: s.Network, limits: s.Concurrency,
			}, nil
		}
	}
	return resolvedSpec{}, fmt.Errorf(
		"product %q has no source %q (from repository row %q)",
		p.Metadata.Name, name, e.Name)
}

// configuredName recovers which configured source or target a repository row
// belongs to.
//
// One configured entry can own SEVERAL repository rows — a source that
// enumerates the registry catalog, and a target a bundle spreads across — and
// the (product, role, name) unique constraint means they cannot all be called
// the same thing. Both producers use "<configured>/<path>", so the configured
// name is everything before the first slash.
//
// A configured name may not itself contain a slash, which is what makes the
// split unambiguous rather than a guess. Enforced by config validation; if it
// ever stops being true, credential resolution is where it would show.
func configuredName(rowName string) string {
	if i := strings.Index(rowName, "/"); i > 0 {
		return rowName[:i]
	}
	return rowName
}

// sharedTransport extracts the parts of a client config that describe the
// REGISTRY rather than one repository.
//
// Repository, and therefore the token scope, is deliberately absent: that
// belongs to the per-repository client built on top of the shared parts.
func sharedTransport(c registry.ClientConfig) transport.Config {
	return transport.Config{
		Registry:              c.Registry,
		RequestsPerSecond:     c.RequestsPerSecond,
		Burst:                 c.Burst,
		MaxConnections:        c.MaxConnections,
		CABundle:              c.CABundle,
		HTTPSProxy:            c.HTTPSProxy,
		NoProxy:               c.NoProxy,
		DirectConnect:         c.DirectConnect,
		InsecureSkipVerify:    c.InsecureSkipVerify,
		ConnectTimeout:        c.ConnectTimeout,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout,
		// HTTP/1.1 for the blob data plane. Sixteen concurrent blobs are
		// sixteen independent TCP connections, each with its own congestion
		// window; on h2 they share one, and per-stream flow-control windows
		// throttle a multi-gigabyte body far below link capacity
		// (docs/design/05 §5). This matters more here than anywhere else in
		// the system, because this is where the large bodies actually move.
		ForceHTTP1: true,
	}
}

// isLoopback reports whether to talk http:// rather than https://.
//
// Restricted to loopback. A vendor registry is always TLS, and an option to
// disable it for arbitrary hosts is a footgun that eventually gets set in
// production "temporarily".
func isLoopback(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], "]") {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
