// Package generic implements OCI Distribution v2 over stdlib net/http.
//
// This is the default and expected path for all four supported registries;
// vendor packages exist only for genuine deviations (docs/design/06 §6).
//
// It uses NO OCI client library. ADR-001 is deliberately open, and tag listing
// plus manifest resolution need only HTTP — so M2 does not force the decision
// that M3 will close with measurements.
package generic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// Repository is a single repository on a Distribution v2 registry.
//
// Safe for concurrent use: one instance is shared across all work against that
// repository, so the connection pool and token cache are shared too.
//
// It implements registry.Source — identity, tag listing, manifest reading and
// probing. It deliberately does NOT implement registry.Repository: blob and
// write operations land in M3, and stubbing them to return "not implemented"
// would be a lie the type system currently tells the truth about.
type Repository struct {
	registryHost string
	repoPath     string
	scheme       string
	client       *http.Client

	capsOnce sync.Once
	caps     registry.Capabilities
}

// init registers this backend under its configured name.
//
// Registration rather than a switch in the abstraction: internal/registry does
// not import this package, so adding a vendor backend never means editing the
// interface it implements.
func init() {
	registry.Register(registry.DefaultType, func(c registry.ClientConfig) (registry.Source, error) {
		return New(FromClientConfig(c))
	})
}

// FromClientConfig translates the backend-neutral config into ours.
func FromClientConfig(c registry.ClientConfig) Config {
	shared, _ := c.Shared.(*transport.Shared)
	log, _ := c.Logger.(*slog.Logger)

	return Config{
		Registry:   c.Registry,
		Repository: c.Repository,
		PlainHTTP:  c.PlainHTTP,
		Shared:     shared,
		Logger:     log,
		Transport: transport.Config{
			Username:              c.Username,
			Password:              c.Password,
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
			UserAgent:             c.UserAgent,
			MaxRetryAttempts:      c.MaxRetryAttempts,
			RetryBaseDelay:        c.RetryBaseDelay,
			RetryMaxDelay:         c.RetryMaxDelay,
			// HTTP/1.1 is forced so M2 and M3 share one transport. See the note
			// on transport.Config.ForceHTTP1.
			ForceHTTP1: true,
		},
	}
}

// Config describes one repository.
type Config struct {
	Registry   string
	Repository string
	Transport  transport.Config
	// PlainHTTP talks to the registry over http://. For local development
	// registries only; never for a vendor.
	PlainHTTP bool

	// Shared is the source's connection pool, rate limiter and token cache. Nil
	// builds a standalone stack, which is right for a one-off client and wrong
	// for one of forty repositories on the same host.
	Shared *transport.Shared

	// Logger records every request at DEBUG and slow ones at WARN. Nil disables
	// both.
	Logger *slog.Logger
}

// New builds a repository client.
func New(cfg Config) (*Repository, error) {
	if cfg.Registry == "" {
		return nil, fmt.Errorf("registry host is required")
	}
	if cfg.Repository == "" {
		return nil, fmt.Errorf("repository path is required")
	}

	tcfg := cfg.Transport
	tcfg.Registry = cfg.Registry
	tcfg.Repository = cfg.Repository
	tcfg.Logger = cfg.Logger

	var client *http.Client
	if cfg.Shared != nil {
		client = cfg.Shared.Client(tcfg)
	} else {
		var err error
		client, err = transport.New(tcfg)
		if err != nil {
			return nil, fmt.Errorf("build transport for %s: %w", cfg.Registry, err)
		}
	}

	scheme := "https"
	if cfg.PlainHTTP {
		scheme = "http"
	}

	return &Repository{
		registryHost: cfg.Registry,
		repoPath:     strings.Trim(cfg.Repository, "/"),
		scheme:       scheme,
		client:       client,
	}, nil
}

// ---- Identity ----

func (r *Repository) Name() string     { return r.registryHost + "/" + r.repoPath }
func (r *Repository) Registry() string { return r.registryHost }
func (r *Repository) Path() string     { return r.repoPath }

// url builds an absolute URL for a v2 API path.
//
// The repository path is escaped per segment: it legitimately contains
// slashes ("vendor-a/platform"), which must survive, while any other unsafe
// character must not.
func (r *Repository) url(format string, args ...any) string {
	segments := strings.Split(r.repoPath, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	name := strings.Join(segments, "/")

	suffix := fmt.Sprintf(format, args...)
	return r.scheme + "://" + r.registryHost + "/v2/" + name + suffix
}

// ---- Prober ----

// Ping verifies the registry answers and that credentials work.
//
// The /v2/ endpoint is the specified discovery point: a 200 (or a 401 that the
// auth transport then satisfies) means Distribution v2 is served here.
func (r *Repository) Ping(ctx context.Context) error {
	endpoint := r.scheme + "://" + r.registryHost + "/v2/"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return r.wrap("ping", "", 0, nil, transportError(err))
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return r.wrap("ping", "", resp.StatusCode, resp, registry.ClassifyStatus(resp.StatusCode))
}

// Capabilities probes what this registry supports, once.
func (r *Repository) Capabilities(ctx context.Context) registry.Capabilities {
	r.capsOnce.Do(func() {
		r.caps = r.probe(ctx)
	})
	return r.caps
}

// probe determines capabilities empirically rather than from a vendor table.
func (r *Repository) probe(ctx context.Context) registry.Capabilities {
	caps := registry.DefaultCapabilities()

	// Referrers API: a 200 means supported; 404 means fall back to the cosign
	// tag schema. Any other answer is inconclusive, so we assume absence —
	// the safe direction, since a wrongly-assumed capability produces a failed
	// operation while a wrongly-assumed absence only costs a slower path.
	probeDigest := registry.NewDigestFromBytes([]byte("softwaregateway-capability-probe"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url("/referrers/%s", probeDigest), nil)
	if err == nil {
		if resp, err := r.client.Do(req); err == nil {
			caps.SupportsReferrersAPI = resp.StatusCode == http.StatusOK
			closeBody(resp)
		}
	}

	// _catalog is reported for diagnostics only. Discovery takes repositories
	// from configuration, not from enumeration (docs/design/07 §2).
	creq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.scheme+"://"+r.registryHost+"/v2/_catalog?n=1", nil)
	if err == nil {
		if resp, err := r.client.Do(creq); err == nil {
			caps.SupportsCatalog = resp.StatusCode == http.StatusOK
			closeBody(resp)
		}
	}

	// Mount and chunked upload cannot be probed without writing, so they keep
	// the spec-default assumption until M3 exercises them for real.
	caps.SupportsMount = true
	caps.SupportsChunkedUpload = true

	return caps
}

// ---- error helpers ----

// wrap builds a classified registry error.
func (r *Repository) wrap(op, ref string, status int, resp *http.Response, err error) error {
	e := &registry.Error{
		Op:         op,
		Repository: r.Name(),
		Reference:  ref,
		StatusCode: status,
		Err:        err,
	}
	if resp != nil {
		e.RetryAfter = retryAfter(resp)
		e.Detail = registryErrorDetail(resp)
	}
	return e
}

// registryErrorDetail extracts the registry's own error message.
//
// Distribution returns {"errors":[{"code":..,"message":..}]}. Surfacing that
// message is the difference between "HTTP 403" and "requested access to the
// resource is denied", which is what an operator actually needs.
func registryErrorDetail(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil || len(body) == 0 {
		return ""
	}

	var payload struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Errors) == 0 {
		return ""
	}

	parts := make([]string, 0, len(payload.Errors))
	for _, e := range payload.Errors {
		switch {
		case e.Code != "" && e.Message != "":
			parts = append(parts, e.Code+": "+e.Message)
		case e.Message != "":
			parts = append(parts, e.Message)
		case e.Code != "":
			parts = append(parts, e.Code)
		}
	}
	return strings.Join(parts, "; ")
}

func closeBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Drain before closing so the connection returns to the pool rather than
	// being torn down.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	_ = resp.Body.Close()
}

// Compile-time proof that this type satisfies exactly what M2 claims.
var _ registry.Source = (*Repository)(nil)
