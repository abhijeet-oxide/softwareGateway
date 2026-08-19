package discovery

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"

	// Also registers the generic backend with the factory, via its init.
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
)

// clientConfigFor builds the backend-neutral config for a source.
//
// This is where configuration meets I/O, and the only place credentials are
// read. Secrets come from projected volume mounts written by VSO - the process
// never talks to the Kubernetes API and never needs Secret read permission
// (docs/design/02 §3).
//
// The repository path is left empty: a source may cover several, so the caller
// fills it in per repository.
func clientConfigFor(
	p *product.Product, src product.Source, secrets *product.SecretResolver,
) (registry.ClientConfig, error) {
	cfg := registry.ClientConfig{
		Type:      string(src.Type),
		Registry:  src.Registry,
		UserAgent: userAgent(),
		PlainHTTP: isPlainHTTP(src.Registry),
	}

	// Already resolved against the application-level defaults by the loader, so
	// there is nothing to fill in here - and MaxConnections is PerRegistry
	// because the pool IS the concurrency limit. See product.Concurrency.
	cfg.MaxConnections = src.Concurrency.PerRegistry
	cfg.RequestsPerSecond = src.Concurrency.RequestsPerSecond
	cfg.Burst = src.Concurrency.Burst()

	if err := applyNetwork(&cfg, p, src.Network, secrets); err != nil {
		return registry.ClientConfig{}, err
	}

	// Anonymous is explicit, not inferred from a missing credentialsRef. A
	// typo'd secret name should fail loudly rather than silently downgrade to
	// anonymous access and then fail later as a confusing 401.
	switch {
	case src.Anonymous:
	case src.CredentialsRef != nil:
		creds, err := secrets.Credentials(*src.CredentialsRef)
		if err != nil {
			return registry.ClientConfig{}, fmt.Errorf("product %q source %q credentials: %w",
				p.Metadata.Name, src.Name, err)
		}
		cfg.Username = creds.Username
		cfg.Password = creds.Password.Reveal()
	default:
		return registry.ClientConfig{}, fmt.Errorf(
			"product %q source %q has neither credentialsRef nor anonymous: true",
			p.Metadata.Name, src.Name)
	}

	return cfg, nil
}

// SourceClientFactory returns a factory that builds a client per repository.
//
// One factory per source, so every repository on that registry shares the
// credential, the CA bundle, the proxy settings and the rate-limit
// configuration - which is what makes declaring several repositories under one
// source correct rather than merely convenient.
func SourceClientFactory(
	p *product.Product, src product.Source, secrets *product.SecretResolver, log *slog.Logger,
) (ClientFactory, error) {
	base, err := clientConfigFor(p, src, secrets)
	if err != nil {
		return nil, err
	}

	// ONE connection pool, ONE rate limiter and ONE token cache for the whole
	// source, built here and shared by every repository client below.
	//
	// Without this each repository built its own, so the configured ceilings
	// were multiplied by however many repositories were being scanned at once:
	// `maxConnections: 32` across sixteen parallel repositories permitted 512
	// connections to a single host, and `requestsPerSecond: 50` permitted 800.
	// Through a corporate proxy that is not a faster scan, it is a
	// self-inflicted overload - and the configuration said the opposite of what
	// was happening.
	shared, err := transport.NewShared(transportConfigFor(base))
	if err != nil {
		return nil, fmt.Errorf("product %q source %q transport: %w",
			p.Metadata.Name, src.Name, err)
	}
	base.Shared = shared
	base.Logger = log

	return func(repositoryPath string) (registry.Source, error) {
		cfg := base
		cfg.Repository = strings.Trim(repositoryPath, "/")
		client, err := registry.New(cfg)
		if err != nil {
			return nil, fmt.Errorf("product %q source %q repository %q: %w",
				p.Metadata.Name, src.Name, repositoryPath, err)
		}
		return client, nil
	}, nil
}

// transportConfigFor extracts the parts of a client config that describe the
// SOURCE rather than one repository.
//
// Repository, and therefore the token scope, is deliberately absent: those
// belong to the per-repository client built on top of the shared parts.
func transportConfigFor(c registry.ClientConfig) transport.Config {
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
		ForceHTTP1:            true,
	}
}

// SourceCatalog builds a registry-scoped client for enumerating repositories.
//
// Returns nil when the source names its repositories - building it anyway would
// mint a second connection pool and a second token scope for a source that will
// never enumerate.
func SourceCatalog(
	p *product.Product, src product.Source, secrets *product.SecretResolver,
) (CatalogLister, error) {
	if !src.EnumeratesRepositories() {
		return nil, nil
	}

	base, err := clientConfigFor(p, src, secrets)
	if err != nil {
		return nil, err
	}

	c, err := generic.NewCatalog(generic.CatalogFromClientConfig(base))
	if err != nil {
		return nil, fmt.Errorf("product %q source %q catalog: %w",
			p.Metadata.Name, src.Name, err)
	}
	return c, nil
}

// applyNetwork merges the source's network settings over the product's.
//
// The merge itself lives in internal/product, where the types it merges are
// defined, so discovery, preflight and the worker cannot drift apart about
// what a proxy or a CA bundle means. This is the mapping onto a client config
// and nothing else.
func applyNetwork(
	cfg *registry.ClientConfig, p *product.Product, override *product.Network, secrets *product.SecretResolver,
) error {
	n, err := product.ResolveNetwork(p, override, secrets)
	if err != nil {
		return fmt.Errorf("product %q: %w", p.Metadata.Name, err)
	}
	applyResolvedNetwork(cfg, n)
	return nil
}

// applyResolvedNetwork copies a resolved network onto a client config.
func applyResolvedNetwork(cfg *registry.ClientConfig, n product.ResolvedNetwork) {
	cfg.CABundle = n.CABundle
	cfg.HTTPSProxy = n.HTTPSProxy
	cfg.NoProxy = n.NoProxy
	cfg.DirectConnect = n.DirectConnect
	cfg.InsecureSkipVerify = n.InsecureSkipVerify
	if n.ConnectTimeout > 0 {
		cfg.ConnectTimeout = n.ConnectTimeout
	}
	if n.ResponseHeaderTimeout > 0 {
		cfg.ResponseHeaderTimeout = n.ResponseHeaderTimeout
	}
}

// isPlainHTTP reports whether to talk http:// rather than https://.
//
// Restricted to loopback. A vendor registry is always TLS, and a config option
// to disable it for arbitrary hosts is a footgun that eventually gets set in
// production "temporarily".
func isPlainHTTP(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], "]") {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func userAgent() string {
	v := version.Get("coordinator")
	return "softwaregateway/" + v.Version
}

// SourceSpecs builds the polling specs for every enabled source of every
// product.
//
// A source whose client cannot be built - a missing secret, an unreachable
// proxy configuration - is reported as an error and OMITTED, while every other
// source still runs. One product's broken credential must not stop discovery
// for the rest of the fleet.
func SourceSpecs(
	products []*product.Product,
	catalog map[string]ProductRef,
	secrets *product.SecretResolver,
	layouts *vendors.Registry,
	log *slog.Logger,
) ([]SourceSpec, []error) {
	if layouts == nil {
		layouts = vendors.NewRegistry()
	}
	if log == nil {
		log = slog.Default()
	}
	var specs []SourceSpec
	var errs []error

	for _, p := range products {
		// A disabled product is loaded, validated and listed - it simply does
		// not run. Skipping here rather than at load is what lets `products
		// list` and `products describe` still show it, which is the point of
		// disabling rather than deleting.
		if !p.IsEnabled() {
			continue
		}

		ref, ok := catalog[p.Metadata.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("product %q has no catalog rows: reconciliation has not run", p.Metadata.Name))
			continue
		}

		for _, src := range p.Spec.Sources {
			// Two switches, two meanings: `enabled: false` removes the source
			// entirely, `discovery.enabled: false` keeps it usable for explicit
			// transfers but stops the polling.
			if !src.IsEnabled() || !src.Discovery.IsEnabled() {
				continue
			}

			// An unknown vendor name is fatal FOR THIS SOURCE and no other.
			// Falling back to standard behaviour would silently disable
			// signature discovery for a typo - invisible in every output, since
			// every package would simply read `unknown` forever.
			layout, err := layouts.Get(src.VendorLayout())
			if err != nil {
				errs = append(errs, fmt.Errorf("product %q source %q: %w",
					p.Metadata.Name, src.Name, err))
				continue
			}

			newClient, err := SourceClientFactory(p, src, secrets, log)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			catalogClient, err := SourceCatalog(p, src, secrets)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			interval := time.Duration(src.Discovery.Interval)
			if interval <= 0 {
				interval = product.DefaultDiscoveryInterval
			}

			specs = append(specs, SourceSpec{
				Product:     p,
				ProductID:   ref.ID,
				SourceName:  src.Name,
				RepoIDs:     ref.Repositories,
				NewClient:   newClient,
				Catalog:     catalogClient,
				Interval:    interval,
				Layout:      layout,
				InsecureTLS: product.SkipsTLSVerification(p.Spec.Network, src.Network),
			})
		}
	}
	return specs, errs
}

// ProductRef is the catalog identity discovery needs.
//
// Declared here rather than imported from internal/catalog so this package
// depends on a shape, not on the reconciler. catalog.ProductRef converts to it
// in the Coordinator wiring.
type ProductRef struct {
	ID           int64
	Repositories map[string]int64
}
