package product

import (
	"fmt"
	"time"
)

// ResolvedNetwork is a product's network settings merged with one
// repository's overrides.
//
// It is deliberately free of any registry type. This package describes what
// configuration SAYS; turning that into a client is the caller's job, and
// keeping the boundary there is what lets discovery, preflight and the worker
// share the merge without any of them importing the others.
type ResolvedNetwork struct {
	CABundle   []byte
	HTTPSProxy string
	NoProxy    []string
	// DirectConnect bypasses proxies entirely, including any the ENVIRONMENT
	// sets. Distinct from an empty HTTPSProxy, which falls back to
	// HTTPS_PROXY - the fallback is right by default and wrong when a
	// repository has explicitly asked to go direct.
	DirectConnect bool

	InsecureSkipVerify bool

	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
}

// ResolveNetwork merges a repository's network settings over its product's.
//
// Source- and target-level settings win where set: a product behind one proxy
// can still have a single vendor reachable by another route, and expressing
// that should not require duplicating the whole network block.
//
// # Why this is one function
//
// It was three, in discovery, preflight and the worker, and they were
// byte-identical copies of subtle merge rules - a proxy that clears rather
// than inherits, an explicit `skipVerify: false` that must beat a product-wide
// true, a CA bundle read from a projected secret. Three copies of a merge is
// three chances for a config to mean different things depending on which code
// path reached the registry, and the failure would look like "discovery works
// but transfers get TLS errors", which is a bad afternoon.
func ResolveNetwork(p *Product, override *Network, secrets *SecretResolver) (ResolvedNetwork, error) {
	var out ResolvedNetwork

	networks := []Network{p.Spec.Network}
	if override != nil {
		networks = append(networks, *override)
	}

	for _, n := range networks {
		if n.CABundleRef != nil {
			key := n.CABundleRef.Key
			if key == "" {
				key = DefaultCABundleKey
			}
			bundle, err := secrets.Value(n.CABundleRef.SecretName, key)
			if err != nil {
				return out, fmt.Errorf("caBundleRef: %w", err)
			}
			out.CABundle = []byte(bundle.Reveal())
		}

		if n.Proxy != nil {
			switch {
			case n.Proxy.Direct:
				// Clears any inherited proxy. Without this the only way to say
				// "everything through the corporate proxy except this one
				// registry" is to repeat the host in noProxy at every level.
				out.HTTPSProxy = ""
				out.NoProxy = nil
				out.DirectConnect = true
			case n.Proxy.HTTPSProxy != "":
				out.HTTPSProxy = n.Proxy.HTTPSProxy
				out.DirectConnect = false
			}
			if len(n.Proxy.NoProxy) > 0 && !n.Proxy.Direct {
				out.NoProxy = n.Proxy.NoProxy
			}
		}

		if n.TLS.SetsSkipVerify() {
			// Explicit at either level wins, INCLUDING an explicit false - a
			// product that disables verification globally must still be able
			// to keep it on for the one registry with a good certificate.
			out.InsecureSkipVerify = n.TLS.SkipsVerify()
		}
		if d := time.Duration(n.Timeouts.Connect); d > 0 {
			out.ConnectTimeout = d
		}
		if d := time.Duration(n.Timeouts.ResponseHeader); d > 0 {
			out.ResponseHeaderTimeout = d
		}
	}
	return out, nil
}
