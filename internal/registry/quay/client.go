// Package quay speaks Red Hat Quay's MANAGEMENT api.
//
// This is deliberately not a registry implementation. Quay's artifact path is
// plain OCI Distribution and is served by internal/registry/generic, unchanged;
// what lives here is `/api/v1`, which configures the registry rather than
// moving content through it. Two protocols, two clients, one host — see
// docs/design/18 section 7.
//
// The distinction is not academic, because the two endpoints take DIFFERENT
// credentials and confusing them produces a 403 that reads like a permissions
// problem:
//
//	/v2/...      artifacts       robot account, `org+robot` as the username
//	/api/v1/...  configuration   OAuth 2 bearer with repo:admin or org:admin
//
// Everything here is idempotent-by-read: nothing writes without the caller
// having asked explicitly, because applying a mirror configuration is
// destructive (docs/design/18 section 8).
package quay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// Config describes how to reach one Quay instance's control API.
//
// Flat and free of configuration types, for the same reason
// registry.ClientConfig is: this package must not know about ConfigMaps or
// Secrets. The translation from a product target happens in the caller.
type Config struct {
	// Endpoint is the API base, e.g. https://quay.apps.ocp.example.com.
	// A bare host is accepted and assumed to be https.
	Endpoint string

	// Token is the OAuth 2 bearer. Required: there is no anonymous access to
	// the management API worth having.
	Token string

	CABundle           []byte
	HTTPSProxy         string
	NoProxy            []string
	DirectConnect      bool
	InsecureSkipVerify bool

	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration

	UserAgent string
	Logger    *slog.Logger

	// RequestTimeout bounds one management call end to end. Unlike the blob
	// path there is no reason for a JSON request to outlive it — every
	// response here is small and a stalled control call blocks an apply.
	RequestTimeout time.Duration
}

// DefaultRequestTimeout bounds a single management call.
const DefaultRequestTimeout = 60 * time.Second

// Retry policy for the control plane. See the comment in New.
const (
	retryAttempts  = 3
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 4 * time.Second
)

// Client is a Quay management API client.
type Client struct {
	http     *http.Client
	endpoint string
	token    string
	timeout  time.Duration
}

// New builds a client.
//
// The transport stack is the same one the registry path uses — TLS trust,
// proxy, retry, rate limiting, request tracing — with NO registry credentials,
// because the OCI token exchange is the wrong auth for this endpoint. The
// bearer token is attached per request instead.
func New(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("quay: an API token is required (%w)", registry.ErrUnauthorized)
	}
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	httpClient, err := transport.New(transport.Config{
		Registry:              hostOf(endpoint),
		CABundle:              cfg.CABundle,
		InsecureSkipVerify:    cfg.InsecureSkipVerify,
		HTTPSProxy:            cfg.HTTPSProxy,
		NoProxy:               cfg.NoProxy,
		DirectConnect:         cfg.DirectConnect,
		ConnectTimeout:        cfg.ConnectTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		UserAgent:             cfg.UserAgent,
		Logger:                cfg.Logger,
		ForceHTTP1:            true,

		// A SHORTER retry schedule than the data path, deliberately.
		//
		// The default is eight attempts over ninety seconds, which is right
		// for a blob and wrong here: it is longer than a control call's own
		// timeout, so a Quay returning 502 would surface to the operator as
		// "context deadline exceeded" — a message about us, naming neither
		// Quay nor the status it actually returned. The budget is kept well
		// inside the request timeout so the real classified error is what
		// comes back.
		MaxRetryAttempts: retryAttempts,
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxDelay:    retryMaxDelay,
		RetryMaxElapsed:  timeout / 2,
	})
	if err != nil {
		return nil, fmt.Errorf("quay: build transport: %w", err)
	}
	return &Client{http: httpClient, endpoint: endpoint, token: cfg.Token, timeout: timeout}, nil
}

// Endpoint returns the API base this client talks to.
func (c *Client) Endpoint() string { return c.endpoint }

func normalizeEndpoint(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("quay: an API endpoint is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("quay: invalid API endpoint %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("quay: invalid API endpoint %q: no host", raw)
	}
	return strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.Host
}

// do performs one management request and decodes the response into out.
//
// out may be nil for calls whose body we do not need — Quay answers several
// mutations with a 201 and an empty body, and a decoder pointed at nothing is
// a needless failure mode.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("quay: encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("quay: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &registry.Error{Op: method + " " + path, Repository: hostOf(c.endpoint), Err: classifyTransport(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return c.statusError(method, path, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Bounded: every management response is small, and an unbounded decode
	// against a proxy error page is a memory footgun for no benefit.
	limited := io.LimitReader(resp.Body, 4<<20)
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return &registry.Error{
			Op: method + " " + path, Repository: hostOf(c.endpoint),
			StatusCode: resp.StatusCode, Detail: err.Error(),
			Err: registry.ErrMalformedResponse,
		}
	}
	return nil
}

func classifyTransport(err error) error {
	if cls := registry.ClassOf(err); cls != registry.ClassUnclassified {
		return err
	}
	return err
}

// statusError turns Quay's own error body into the classified form the rest of
// the system already reasons about, so retry policy and metrics key off one
// vocabulary rather than an HTTP status re-inspected here.
func (c *Client) statusError(method, path string, resp *http.Response) error {
	detail := quayDetail(resp.Body)
	e := &registry.Error{
		Op:         method + " " + path,
		Repository: hostOf(c.endpoint),
		StatusCode: resp.StatusCode,
		Detail:     detail,
		Err:        registry.ClassifyStatus(resp.StatusCode),
	}
	// A 403 on this endpoint is nearly always the same mistake, and the
	// message is worth more than the status. Quay says "Unauthorized" for a
	// robot account presented to /api/v1, which sends people to look at the
	// robot's permissions rather than at the fact that a robot cannot
	// authenticate here at all.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		if e.Detail == "" {
			e.Detail = "the management API takes an OAuth 2 bearer token with repo:admin or org:admin scope; a robot account authenticates /v2 only"
		}
	}
	return e
}

// quayDetail extracts Quay's own words. Quay uses several shapes across
// versions and endpoints, so all of them are tried rather than assuming one.
func quayDetail(r io.Reader) string {
	var body struct {
		Detail       string `json:"detail"`
		ErrorMessage string `json:"error_message"`
		Message      string `json:"message"`
		Title        string `json:"title"`
	}
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not JSON: most likely a proxy or ingress error page. The first line
		// of it is more useful than nothing and far more useful than the whole
		// page.
		return firstLine(string(raw))
	}
	for _, s := range []string{body.Detail, body.ErrorMessage, body.Message, body.Title} {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// repoPath renders `namespace/name` for Quay's route, which takes the two as
// one path segment pair rather than as query parameters.
//
// Quay's model is exactly organization/repository. A configured repository
// with more segments than that keeps them in the NAME half rather than being
// rejected here: Quay itself decides whether it supports nested names, and
// guessing on its behalf would reject a working deployment.
func repoPath(namespace, name string) string {
	return url.PathEscape(namespace) + "/" + strings.TrimPrefix(name, "/")
}

// SplitRepository divides a configured repository path into the Quay
// organization and the repository name.
func SplitRepository(repository string) (namespace, name string, ok bool) {
	repository = strings.Trim(repository, "/")
	i := strings.Index(repository, "/")
	if i <= 0 || i == len(repository)-1 {
		return "", "", false
	}
	return repository[:i], repository[i+1:], true
}
