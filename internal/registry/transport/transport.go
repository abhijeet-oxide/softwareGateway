// Package transport builds the HTTP client every registry implementation
// shares.
//
// See docs/design/06 §5. The layering is:
//
//	rate limiter        outermost
//	  retry
//	    auth
//	      http.Transport
//
// The RATE LIMITER IS OUTERMOST, and that ordering is the point: it means
// retries are rate-limited too. With retry outside the limiter, a burst of
// failures against a struggling registry would bypass the very limit meant to
// protect it — which is precisely how a transient error becomes an outage.
//
// Every cross-cutting concern lives here rather than in a registry
// implementation. A vendor backend that had to re-implement rate limiting
// would be a design failure.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"
)

// Config describes one repository's transport.
type Config struct {
	// Registry host, used for the token cache key and for logging.
	Registry string
	// Repository path, used to scope tokens.
	Repository string

	// Scope overrides the derived token scope.
	//
	// Needed for `/v2/_catalog`, which requires `registry:catalog:*` rather
	// than the `repository:<name>:pull` this would otherwise derive. Sending
	// the wrong scope gets a token the registry then refuses the request with,
	// which reads as a permissions problem rather than a client bug.
	Scope string

	// Credentials; zero value means anonymous.
	Username string
	Password string

	// RequestsPerSecond and Burst configure the token bucket. Zero rps means
	// unlimited.
	RequestsPerSecond int
	Burst             int

	// MaxConnections caps the connection pool.
	MaxConnections int

	// CABundle is additional trusted CA material, appended to the system pool.
	CABundle []byte

	// InsecureSkipVerify disables certificate verification: chain, expiry and
	// hostname all stop being checked.
	//
	// An earlier revision of this file said this would never exist. That was
	// too strong — a CA bundle fixes an untrusted chain and nothing else, and
	// an expired or wrong-hostname certificate on a registry being migrated is
	// a real situation. It remains the wrong answer to most problems, and
	// notably does NOT fix "x509: negative serial number", which fails during
	// PARSING before verification runs.
	InsecureSkipVerify bool

	// Proxy settings.
	HTTPSProxy string
	NoProxy    []string
	// DirectConnect bypasses proxies entirely, including any the environment
	// sets. Distinct from an empty HTTPSProxy, which falls back to
	// HTTPS_PROXY.
	DirectConnect bool

	// Timeouts. Note there is no overall request timeout: a large manifest is
	// not slow, and in M3 a multi-gigabyte blob certainly is not. Stalls are
	// caught by ResponseHeaderTimeout and, for bodies, by a stall detector.
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration

	// ForceHTTP1 disables HTTP/2.
	//
	// For M2's small JSON requests h2 would be fine. It is forced off anyway so
	// that M2 and M3 share one transport: h2 multiplexes every stream onto one
	// TCP connection, and for multi-gigabyte blob bodies its flow-control
	// windows and head-of-line blocking under-perform many parallel HTTP/1.1
	// connections. See docs/design/05 §5.
	ForceHTTP1 bool

	// UserAgent identifies us to the registry.
	UserAgent string

	// Logger records every request. Nil disables tracing entirely.
	//
	// At DEBUG this is a full trace of what the scan is doing on the wire; at
	// any level it reports failures and slow requests, which is what turns
	// "discovery is slow" into a specific host, path and duration.
	Logger *slog.Logger
	// SlowRequest is the threshold for the slow-request warning. Zero uses
	// DefaultSlowRequest.
	SlowRequest time.Duration

	// Retry policy. Zero values use the defaults from docs/design/10 §6.
	//
	// Configurable rather than hardcoded for two reasons: tests need a fast
	// policy so a persistent-failure case does not spend two minutes in
	// backoff, and a deployment behind a flaky link may legitimately want a
	// different schedule from one on a reliable network.
	MaxRetryAttempts int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration

	// RetryMaxElapsed bounds the TOTAL time one request may spend across all
	// its attempts, including backoff.
	//
	// The attempt count alone is the wrong bound. Against a server that accepts
	// the connection and then never answers, each attempt costs the full
	// ResponseHeaderTimeout, so the default eight attempts is four minutes for
	// a single request — and discovery makes two per tag. This caps the
	// pathological case without shortening the schedule for the transient one
	// retries exist for, where attempts fail fast and the budget is never
	// reached.
	RetryMaxElapsed time.Duration
}

// Defaults applied when a field is unset.
const (
	DefaultConnectTimeout        = 10 * time.Second
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultMaxConnections        = 32
	DefaultUserAgent             = "softwaregateway"

	// DefaultRetryMaxElapsed is three ResponseHeaderTimeouts' worth of budget:
	// enough for a genuine transient failure to be retried a couple of times,
	// short enough that a systematically unresponsive endpoint costs seconds
	// per request rather than minutes.
	DefaultRetryMaxElapsed = 90 * time.Second
)

// Shared is the part of the stack that belongs to a SOURCE rather than to one
// repository: the connection pool, the rate limiter and the token cache.
//
// It exists because building a whole stack per repository quietly multiplied
// every configured limit by the number of repositories being scanned. With
// `maxConnections: 32` and sixteen repositories in flight, the process was
// entitled to 512 concurrent connections to one host — and `requestsPerSecond:
// 50` became 800. Through a corporate proxy that is not a fast scan, it is a
// self-inflicted denial of service, and the configuration said the opposite.
//
// Sharing also means one token exchange for the whole source instead of one per
// repository, and warm keep-alives across repositories rather than a fresh TLS
// handshake for each.
type Shared struct {
	base    *http.Transport
	limiter *rate.Limiter
	tokens  *tokenCache
}

// NewShared builds the per-source parts. The repository-specific fields of cfg
// are ignored here; pass them to Client.
func NewShared(cfg Config) (*Shared, error) {
	base, err := baseTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &Shared{
		base:    base,
		limiter: newLimiter(cfg.RequestsPerSecond, cfg.Burst),
		tokens:  newTokenCache(),
	}, nil
}

// Client builds a client for ONE repository on top of the shared parts.
//
// The retry and auth layers stay per repository: retry carries no state worth
// sharing, and auth derives its token scope from cfg.Repository. Both sit
// between the shared limiter and the shared pool, so the layering in the
// package comment is preserved exactly.
func (s *Shared) Client(cfg Config) *http.Client {
	// Innermost first, so the outermost wrapper is applied last.
	var rt http.RoundTripper = s.base
	rt = newAuthTransportWithCache(rt, cfg, s.tokens)
	rt = newRetryTransport(rt, cfg)
	rt = newRateLimitTransportWith(rt, s.limiter)
	rt = newUserAgentTransport(rt, cfg.UserAgent)
	// Outermost, so the duration reported is the cost the CALLER paid —
	// including any wait for a rate-limit token and any retry backoff.
	rt = newTraceTransport(rt, cfg.Logger, cfg.SlowRequest)

	return &http.Client{
		Transport: rt,
		// No client-level Timeout: it would apply to the whole exchange
		// including the body, capping large reads. Per-phase timeouts on the
		// transport are the right granularity.
	}
}

// New builds a standalone client, sharing nothing.
//
// For one-off users — preflight probes, the catalog client — where there is no
// second repository to share with.
func New(cfg Config) (*http.Client, error) {
	shared, err := NewShared(cfg)
	if err != nil {
		return nil, err
	}
	return shared.Client(cfg), nil
}

func baseTransport(cfg Config) (*http.Transport, error) {
	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = DefaultMaxConnections
	}
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	headerTimeout := cfg.ResponseHeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultResponseHeaderTimeout
	}

	tlsConfig, err := tlsConfigFor(cfg)
	if err != nil {
		return nil, err
	}

	proxy, err := proxyFor(cfg)
	if err != nil {
		return nil, err
	}

	t := &http.Transport{
		Proxy:           proxy,
		TLSClientConfig: tlsConfig,

		DialContext: (&net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: 1 * time.Second,

		// Go defaults to 2 idle connections per host. With concurrent work
		// against one registry that serializes almost everything behind
		// connection churn.
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     90 * time.Second,

		// Blobs are already gzip or zstd compressed. Attempting again burns
		// CPU, and transparent decompression would change the bytes and break
		// the digest. Manifests are small enough that compression is noise.
		DisableCompression: true,

		WriteBufferSize: 64 * 1024,
		ReadBufferSize:  64 * 1024,
	}

	if cfg.ForceHTTP1 {
		// Setting a non-nil empty TLSNextProto is how Go is told not to
		// negotiate h2.
		t.ForceAttemptHTTP2 = false
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	} else {
		t.ForceAttemptHTTP2 = true
	}

	return t, nil
}

func tlsConfigFor(cfg Config) (*tls.Config, error) {
	//nolint:gosec // G402: deliberate, opt-in per repository, and logged loudly
	// by the caller. See the field's documentation.
	base := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	if len(cfg.CABundle) == 0 {
		return base, nil
	}

	// Append to the system pool rather than replacing it: a product that adds
	// a private CA still needs to reach public registries and Sigstore.
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(cfg.CABundle) {
		return nil, fmt.Errorf("caBundle for %s contains no usable PEM certificates", cfg.Registry)
	}
	base.RootCAs = pool
	return base, nil
}

func proxyFor(cfg Config) (func(*http.Request) (*url.URL, error), error) {
	if cfg.DirectConnect {
		// Explicitly direct: not even the environment's proxy applies. A
		// repository that asked to bypass the proxy means it, and silently
		// honouring HTTPS_PROXY here would make the setting a no-op in exactly
		// the deployment that needs it — one with a cluster-wide proxy.
		return func(*http.Request) (*url.URL, error) { return nil, nil }, nil
	}
	if cfg.HTTPSProxy == "" {
		// Fall back to the environment so a cluster-wide proxy still applies.
		return http.ProxyFromEnvironment, nil
	}

	proxyURL, err := url.Parse(cfg.HTTPSProxy)
	if err != nil {
		return nil, fmt.Errorf("parse httpsProxy %q: %w", cfg.HTTPSProxy, err)
	}

	noProxy := make(map[string]bool, len(cfg.NoProxy))
	for _, h := range cfg.NoProxy {
		noProxy[h] = true
	}

	return func(r *http.Request) (*url.URL, error) {
		host := r.URL.Hostname()
		if noProxy[host] {
			return nil, nil
		}
		// Suffix entries such as ".svc.cluster.local".
		for entry := range noProxy {
			if len(entry) > 0 && entry[0] == '.' && len(host) > len(entry) &&
				host[len(host)-len(entry):] == entry {
				return nil, nil
			}
		}
		return proxyURL, nil
	}, nil
}

// userAgentTransport stamps a User-Agent on every request.
//
// Outermost because it must apply to auth-token fetches too — a registry
// operator diagnosing load wants to see who is calling, including for the
// token endpoint.
type userAgentTransport struct {
	next http.RoundTripper
	ua   string
}

func newUserAgentTransport(next http.RoundTripper, ua string) http.RoundTripper {
	if ua == "" {
		ua = DefaultUserAgent
	}
	return &userAgentTransport{next: next, ua: ua}
}

func (t *userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("User-Agent") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("User-Agent", t.ua)
	}
	return t.next.RoundTrip(r)
}
