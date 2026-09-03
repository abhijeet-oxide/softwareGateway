package anchore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// Config is everything needed to reach one Anchore Enterprise.
//
// # Why the transport fields are here and not derived from a repository
//
// Because Anchore is not a repository. Xray's config is a copy of the JFrog
// repository's, because Xray is a second endpoint on the same host reached with
// the same credential; there is no such host here. What replaces it is one
// stanza in the system configuration, resolved once for the deployment, so a
// product document still says one thing about Anchore - whether it is on.
type Config struct {
	// Endpoint is the Anchore API base URL as an operator has it in a browser:
	// "https://anchore.example.com". The `/v2` prefix is appended here.
	Endpoint string

	Username string
	Password string
	// Account scopes every request to one Anchore account, via the
	// `x-anchore-account` header. Empty uses the credential's own account,
	// which is what almost every deployment wants.
	Account string

	CABundle           []byte
	HTTPSProxy         string
	NoProxy            []string
	DirectConnect      bool
	InsecureSkipVerify bool

	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	RequestTimeout        time.Duration

	UserAgent string
	Logger    *slog.Logger
}

// Client speaks Anchore Enterprise's v2 API.
type Client struct {
	http     *http.Client
	endpoint string
	service  string
	username string
	password string
	account  string
	timeout  time.Duration
	logger   *slog.Logger
}

// NewClient builds a client against one Anchore.
//
// Basic authentication, because that is what Anchore Enterprise takes and what
// every deployment has. An API key presented in the password field works
// unchanged, which is the form a service account should be using.
func NewClient(cfg Config) (*Client, error) {
	endpoint, err := resolveEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.Username == "" && cfg.Password == "" {
		return nil, fmt.Errorf("anchore: a username and password (or API key) are required (%w)",
			registry.ErrUnauthorized)
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

		MaxRetryAttempts: retryAttempts,
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxDelay:    retryMaxDelay,
		RetryMaxElapsed:  timeout / 2,
	})
	if err != nil {
		return nil, fmt.Errorf("anchore: build transport: %w", err)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		http:     httpClient,
		endpoint: endpoint,
		service:  strings.TrimSuffix(endpoint, apiPrefix) + "/service",
		username: cfg.Username,
		password: cfg.Password,
		account:  strings.TrimSpace(cfg.Account),
		timeout:  timeout,
		logger:   log,
	}, nil
}

// Endpoint returns the API base URL, including the version prefix.
func (c *Client) Endpoint() string { return c.endpoint }

// Ping checks Anchore answers and the credential is accepted.
//
// GET /account rather than GET /: the unauthenticated root answers on an
// Anchore whose credential is wrong, so a health check built on it would report
// green for a deployment that cannot read a single image.
func (c *Client) Ping(ctx context.Context) error {
	var out struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	return c.do(ctx, http.MethodGet, "/account", nil, &out)
}

// resolveEndpoint turns a configured base URL into the API root.
//
// Tolerant of the three things operators actually paste: a bare host, a URL
// with a trailing slash, and a URL that already ends in `/v2` because they
// copied it out of the API documentation. Appending a second `/v2` to the
// third produces 404s on every call, which reads like a missing feature rather
// than a doubled path.
func resolveEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("anchore: an endpoint is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("anchore: invalid endpoint %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("anchore: invalid endpoint %q: no host", raw)
	}
	base := strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/")
	if strings.HasSuffix(base, apiPrefix) {
		return base, nil
	}
	return base + apiPrefix, nil
}

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.Host
}

// do performs one request with a JSON body and decodes a JSON answer.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var encoded []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("anchore: encode %s %s: %w", method, path, err)
		}
		encoded = raw
	}
	_, err := c.send(ctx, method, path, encoded, out, false)
	return err
}

// doService calls Anchore's service API, used only for image submission. This
// deployment accepts submissions there with a flat tag request, while the v2
// image endpoint rejects the same registry image by digest.
func (c *Client) doService(ctx context.Context, method, path string, body, out any) error {
	service := *c
	service.endpoint = c.service
	return service.do(ctx, method, path, body, out)
}

// raw performs one request and returns the response body untouched.
//
// # Why the bytes and not a decoded value
//
// Because these bodies are the product, not an input. An SBOM or a
// vulnerability response is what somebody hands to a vendor, and a copy this
// platform re-encoded from its own model would be a different document with
// the same facts in it - which is worse than handing them nothing, because it
// looks authoritative.
func (c *Client) raw(ctx context.Context, path string) ([]byte, error) {
	return c.send(ctx, http.MethodGet, path, nil, nil, true)
}

func (c *Client) send(
	ctx context.Context, method, path string, body []byte, out any, wantRaw bool,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return nil, fmt.Errorf("anchore: build %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.account != "" {
		// Admin-only, and only sent when configured: an ordinary credential
		// sending this header for its own account is refused by some builds.
		req.Header.Set("x-anchore-account", c.account)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &registry.Error{
			Op: method + " " + path, Repository: hostOf(c.endpoint),
			Err: classifyTransport(ctx, err),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, c.statusError(method, path, resp)
	}
	// Bounded. An image's vulnerability list is large and not unbounded, and an
	// unbounded read against a proxy's error page is a memory footgun.
	limited := io.LimitReader(resp.Body, 128<<20)
	if wantRaw {
		raw, err := io.ReadAll(limited)
		if err != nil {
			return nil, c.bodyError(ctx, method, path, resp.StatusCode, err)
		}
		return raw, nil
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, limited)
		return nil, nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return nil, c.bodyError(ctx, method, path, resp.StatusCode, err)
	}
	return nil, nil
}

// bodyError classifies a failure that happened while reading a response that
// had already started arriving.
//
// The distinction matters for the same reason it does on the Xray path: a body
// that stopped mid-stream is a scanner under load, which a smaller or later
// request may succeed at, and calling it malformed classifies the most common
// real failure as one that cannot be retried.
func (c *Client) bodyError(ctx context.Context, method, path string, status int, err error) error {
	e := &registry.Error{
		Op: method + " " + path, Repository: hostOf(c.endpoint), StatusCode: status,
	}
	switch {
	case isDeadline(ctx, err):
		e.Detail = fmt.Sprintf(
			"Anchore began answering but the response did not finish within %s.", c.timeout)
		e.Err = fmt.Errorf("%w: %w", registry.ErrTimeout, err)
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		e.Detail = "Anchore closed the connection before the response finished."
		e.Err = fmt.Errorf("%w: %w", registry.ErrUnavailable, err)
	default:
		e.Detail = err.Error()
		e.Err = registry.ErrMalformedResponse
	}
	return e
}

func classifyTransport(ctx context.Context, err error) error {
	if isDeadline(ctx, err) {
		return fmt.Errorf("%w: %w", registry.ErrTimeout, err)
	}
	return err
}

func isDeadline(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// statusError turns Anchore's error body into the classified vocabulary the
// rest of the system already reasons about.
func (c *Client) statusError(method, path string, resp *http.Response) error {
	e := &registry.Error{
		Op:         method + " " + path,
		Repository: hostOf(c.endpoint),
		StatusCode: resp.StatusCode,
		Detail:     errorDetail(resp.Body),
		Err:        registry.ClassifyStatus(resp.StatusCode),
	}
	// The three failures whose HTTP status sends people to look in the wrong
	// place, each named for the thing that actually has to change.
	switch resp.StatusCode {
	case http.StatusNotFound:
		if e.Detail == "" {
			e.Detail = "Anchore does not know this resource; the endpoint may be the UI host rather than the API host"
		}
	case http.StatusForbidden:
		if e.Detail == "" {
			e.Detail = "the Anchore credential is valid but the account has no permission for this call"
		}
	case http.StatusTooManyRequests:
		if e.Detail == "" {
			e.Detail = "Anchore is rate limiting these requests; lower coordinator.security.anchore.concurrency"
		}
	}
	return e
}

// NotFound reports whether a failure is Anchore saying it has no such record.
//
// A first-class question rather than a status comparison at the call sites,
// because "Anchore has never heard of this image" is the ORDINARY state before
// a submission and must not be reported as a failure. Everything in this
// package that looks something up has to be able to tell that apart from a
// scanner that is down.
func NotFound(err error) bool {
	var re *registry.Error
	if errors.As(err, &re) {
		return re.StatusCode == http.StatusNotFound
	}
	return false
}

// Conflict reports whether a failure is Anchore saying the thing already
// exists.
//
// Which is a SUCCESS for every create in this package: an application created
// by a sync that ran a minute ago on another Coordinator is the application
// this sync wanted. See findOrCreateApplication.
func Conflict(err error) bool {
	var re *registry.Error
	if errors.As(err, &re) {
		return re.StatusCode == http.StatusConflict
	}
	return false
}

func errorDetail(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body apiError
	if err := json.Unmarshal(raw, &body); err != nil {
		return firstLine(string(raw))
	}
	if body.Message != "" {
		return body.Message
	}
	if s, ok := body.Detail.(string); ok && s != "" {
		return s
	}
	return firstLine(string(raw))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strings.TrimSpace(s)
}

// jsonTime is an RFC 3339 timestamp that tolerates the shapes Anchore sends.
//
// Anchore returns times with and without a zone, and occasionally an empty
// string where the schema says date-time. A strict time.Time would fail the
// whole decode of a vulnerability response over a missing scan timestamp,
// which is a release's findings lost to a field nothing reads.
type jsonTime struct{ t time.Time }

// Time returns the parsed time, zero when it could not be read.
func (j *jsonTime) Time() time.Time {
	if j == nil {
		return time.Time{}
	}
	return j.t
}

// Ptr returns the time as a pointer, nil when unset - the shape the platform's
// own model uses for "the scanner did not say".
func (j *jsonTime) Ptr() *time.Time {
	if j == nil || j.t.IsZero() {
		return nil
	}
	t := j.t
	return &t
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

func (j *jsonTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			j.t = t.UTC()
			return nil
		}
	}
	// An unparseable timestamp costs a date on a page, never a release's
	// findings. Nothing in this integration branches on one.
	return nil
}

// query builds a query string, omitting empty values.
func query(pairs ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			v.Set(pairs[i], pairs[i+1])
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func boolParam(b bool) string {
	if !b {
		return ""
	}
	return strconv.FormatBool(b)
}
