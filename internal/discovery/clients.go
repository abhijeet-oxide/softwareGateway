package discovery

import (
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"

	// Also registers the generic backend with the factory, via its init.
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
)

// clientConfigFor builds the backend-neutral config for a source.
//
// This is where configuration meets I/O, and the only place credentials are
// read. Secrets come from projected volume mounts written by VSO — the process
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

	limits := src.RateLimits.WithDefaults()
	cfg.RequestsPerSecond = limits.RequestsPerSecond
	cfg.Burst = limits.Burst
	cfg.MaxConnections = limits.MaxConnections

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
// configuration — which is what makes declaring several repositories under one
// source correct rather than merely convenient.
func SourceClientFactory(
	p *product.Product, src product.Source, secrets *product.SecretResolver,
) (ClientFactory, error) {
	base, err := clientConfigFor(p, src, secrets)
	if err != nil {
		return nil, err
	}

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

// SourceCatalog builds a registry-scoped client for enumerating repositories.
//
// Returns nil when the source names its repositories — building it anyway would
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
// Source-level settings win where set: a product behind one proxy can still
// have a single vendor reachable by another route, and expressing that should
// not require duplicating the whole network block.
func applyNetwork(
	cfg *registry.ClientConfig, p *product.Product, override *product.Network, secrets *product.SecretResolver,
) error {
	networks := []product.Network{p.Spec.Network}
	if override != nil {
		networks = append(networks, *override)
	}

	for _, n := range networks {
		if n.CABundleRef != nil {
			key := n.CABundleRef.Key
			if key == "" {
				key = product.DefaultCABundleKey
			}
			bundle, err := secrets.Value(n.CABundleRef.SecretName, key)
			if err != nil {
				return fmt.Errorf("product %q caBundleRef: %w", p.Metadata.Name, err)
			}
			cfg.CABundle = []byte(bundle.Reveal())
		}
		if n.Proxy != nil {
			if n.Proxy.HTTPSProxy != "" {
				cfg.HTTPSProxy = n.Proxy.HTTPSProxy
			}
			if len(n.Proxy.NoProxy) > 0 {
				cfg.NoProxy = n.Proxy.NoProxy
			}
		}
		if d := time.Duration(n.Timeouts.Connect); d > 0 {
			cfg.ConnectTimeout = d
		}
		if d := time.Duration(n.Timeouts.ResponseHeader); d > 0 {
			cfg.ResponseHeaderTimeout = d
		}
	}
	return nil
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
// A source whose client cannot be built — a missing secret, an unreachable
// proxy configuration — is reported as an error and OMITTED, while every other
// source still runs. One product's broken credential must not stop discovery
// for the rest of the fleet.
func SourceSpecs(
	products []*product.Product,
	catalog map[string]ProductRef,
	secrets *product.SecretResolver,
) ([]SourceSpec, []error) {
	var specs []SourceSpec
	var errs []error

	for _, p := range products {
		ref, ok := catalog[p.Metadata.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("product %q has no catalog rows: reconciliation has not run", p.Metadata.Name))
			continue
		}

		for _, src := range p.Spec.Sources {
			if !src.Discovery.IsEnabled() {
				continue
			}

			newClient, err := SourceClientFactory(p, src, secrets)
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
				Product:    p,
				ProductID:  ref.ID,
				SourceName: src.Name,
				RepoIDs:    ref.Repositories,
				NewClient:  newClient,
				Catalog:    catalogClient,
				Interval:   interval,
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
