package discovery

import (
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"

	// Registers the generic backend. Imported for the side effect: the factory
	// does not import backends, so something must.
	_ "github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
)

// SourceClient builds a registry client for a configured source.
//
// This is where configuration meets I/O, and the only place credentials are
// read. Secrets come from projected volume mounts written by VSO — the process
// never talks to the Kubernetes API and never needs Secret read permission
// (docs/design/02 §3).
func SourceClient(p *product.Product, src product.Source, secrets *product.SecretResolver) (registry.Source, error) {
	cfg := registry.ClientConfig{
		Type:       string(src.Type),
		Registry:   src.Registry,
		Repository: strings.Trim(src.Repository, "/"),
		UserAgent:  userAgent(),
		PlainHTTP:  isPlainHTTP(src.Registry),
	}

	limits := src.RateLimits.WithDefaults()
	cfg.RequestsPerSecond = limits.RequestsPerSecond
	cfg.Burst = limits.Burst
	cfg.MaxConnections = limits.MaxConnections

	if err := applyNetwork(&cfg, p, src.Network, secrets); err != nil {
		return nil, err
	}

	// Anonymous is explicit, not inferred from a missing credentialsRef. A
	// typo'd secret name should fail loudly rather than silently downgrade to
	// anonymous access and then fail later as a confusing 401.
	switch {
	case src.Anonymous:
	case src.CredentialsRef != nil:
		creds, err := secrets.Credentials(*src.CredentialsRef)
		if err != nil {
			return nil, fmt.Errorf("product %q source %q credentials: %w",
				p.Metadata.Name, src.Name, err)
		}
		cfg.Username = creds.Username
		cfg.Password = creds.Password.Reveal()
	default:
		return nil, fmt.Errorf(
			"product %q source %q has neither credentialsRef nor anonymous: true",
			p.Metadata.Name, src.Name)
	}

	client, err := registry.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("product %q source %q: %w", p.Metadata.Name, src.Name, err)
	}
	return client, nil
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
				key = "ca.crt"
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

			repoID, ok := ref.Repositories[src.Name]
			if !ok {
				errs = append(errs, fmt.Errorf("product %q source %q has no catalog row", p.Metadata.Name, src.Name))
				continue
			}

			client, err := SourceClient(p, src, secrets)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			interval := time.Duration(src.Discovery.Interval)
			if interval <= 0 {
				interval = product.DefaultDiscoveryInterval
			}

			specs = append(specs, SourceSpec{
				Product:      p,
				ProductID:    ref.ID,
				SourceName:   src.Name,
				SourceRepoID: repoID,
				RepoIDs:      ref.Repositories,
				Client:       client,
				Interval:     interval,
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
