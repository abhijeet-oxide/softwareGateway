package replication

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
)

// Resolver builds management clients and desired states from configuration.
//
// This is the only place in the package that reads Secrets, for the same
// reason internal/discovery concentrates it: secrets come from projected
// volumes written by VSO, the process never talks to the Kubernetes API, and
// the one function that reads them is easier to audit than five.
type Resolver struct {
	secrets *product.SecretResolver
	log     *slog.Logger
	// now is injectable so an apply's timestamps are deterministic in tests.
	now func() time.Time

	userAgent string
}

// NewResolver builds one. secrets may be nil when no target needs credentials,
// which is the case in dry-run tooling that only reads configuration.
func NewResolver(secrets *product.SecretResolver, log *slog.Logger, userAgent string) *Resolver {
	return &Resolver{secrets: secrets, log: log, now: time.Now, userAgent: userAgent}
}

// WithClock replaces the clock. Tests only.
func (r *Resolver) WithClock(now func() time.Time) *Resolver {
	r.now = now
	return r
}

// Desired resolves a target's configured replication into the state we would
// write, including the upstream reference and any credentials Quay will use.
func (r *Resolver) Desired(p *product.Product, t product.Target) (*Desired, error) {
	if !t.Delegated() {
		return &Desired{Target: t.Name, Mode: product.ReplicationCopy}, nil
	}

	upstream, err := ResolveUpstream(p, t)
	if err != nil {
		return nil, err
	}

	creds, err := r.upstreamCredentials(p, t)
	if err != nil {
		return nil, err
	}
	return Resolve(t, upstream, creds, r.now())
}

// upstreamCredentials reads what QUAY will use to reach the upstream.
//
// Not the target's own credentialsRef, which is what WE use to reach Quay.
// Confusing the two produces a mirror that works from our pods and fails from
// Quay's, which is a long afternoon.
func (r *Resolver) upstreamCredentials(p *product.Product, t product.Target) (Credentials, error) {
	var ref *product.CredentialsRef
	switch t.ReplicationMode() {
	case product.ReplicationMirror:
		if m, ok := t.MirrorSettings(); ok {
			ref = m.SourceCredentialsRef
		}
	case product.ReplicationProxy:
		if c, ok := t.ProxySettings(); ok {
			ref = c.UpstreamCredentialsRef
		}
	}
	if ref == nil {
		return Credentials{}, nil
	}
	if r.secrets == nil {
		return Credentials{}, fmt.Errorf(
			"product %q target %q references secret %q for the upstream, but no secret resolver is configured",
			p.Metadata.Name, t.Name, ref.SecretName)
	}
	c, err := r.secrets.Credentials(*ref)
	if err != nil {
		return Credentials{}, fmt.Errorf("product %q target %q upstream credentials: %w",
			p.Metadata.Name, t.Name, err)
	}
	return Credentials{Username: c.Username, Password: c.Password.Reveal()}, nil
}

// Client builds a management client for a target's registry.
//
// The transport settings are the target's own resolved network — the same CA
// bundle, proxy and timeouts the artifact path uses, because it is the same
// host and the same network between us and it. What is NOT shared is the
// credential: the API token, not the robot.
func (r *Resolver) Client(p *product.Product, t product.Target) (*quay.Client, error) {
	if t.Type != product.RegistryQuay {
		return nil, fmt.Errorf("target %q is type %q; the management API exists only for Quay", t.Name, t.Type)
	}
	if t.Quay == nil || t.Quay.APITokenRef == nil {
		return nil, fmt.Errorf(
			"target %q has no quay.apiTokenRef; the management API takes an OAuth 2 bearer token, and the robot in credentialsRef authenticates /v2 only",
			t.Name)
	}
	if r.secrets == nil {
		return nil, fmt.Errorf("target %q needs secret %q, but no secret resolver is configured",
			t.Name, t.Quay.APITokenRef.SecretName)
	}

	token, err := r.secrets.Value(t.Quay.APITokenRef.SecretName, t.Quay.APITokenRef.Key)
	if err != nil {
		return nil, fmt.Errorf("target %q quay.apiTokenRef: %w", t.Name, err)
	}

	net, err := product.ResolveNetwork(p, t.Network, r.secrets)
	if err != nil {
		return nil, fmt.Errorf("product %q target %q network: %w", p.Metadata.Name, t.Name, err)
	}

	endpoint := t.Quay.APIEndpoint
	if endpoint == "" {
		endpoint = t.Registry
	}

	return quay.New(quay.Config{
		Endpoint:              endpoint,
		Token:                 token.Reveal(),
		CABundle:              net.CABundle,
		HTTPSProxy:            net.HTTPSProxy,
		NoProxy:               net.NoProxy,
		DirectConnect:         net.DirectConnect,
		InsecureSkipVerify:    net.InsecureSkipVerify,
		ConnectTimeout:        net.ConnectTimeout,
		ResponseHeaderTimeout: net.ResponseHeaderTimeout,
		UserAgent:             r.userAgent,
		Logger:                r.log,
	})
}

// Now returns the resolver's clock, so callers timestamp with the same one.
func (r *Resolver) Now() time.Time { return r.now() }
