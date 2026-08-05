package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrUnreachable means the Coordinator could not be contacted at all, as
// distinct from the Coordinator answering with an error. transferctl maps it
// to exit code 3 so a script can tell "the service is down" from "you asked
// for something invalid".
var ErrUnreachable = errors.New("coordinator unreachable")

// ErrTimeout means the Coordinator was reached but did not answer within the
// client's timeout.
//
// Separate from ErrUnreachable because they call for opposite actions. "The
// Coordinator is down" means go and look at the Coordinator; "it is still
// working on your request" means give it longer. Reporting both as
// unreachable — which is what this client used to do — sends operators to
// investigate a healthy service, and is exactly the confusion `products check`
// and `packages discover` produced against their 30-second default.
var ErrTimeout = errors.New("the request timed out")

// Client talks to the Coordinator API.
//
// transferctl uses this rather than hand-rolling HTTP, so the CLI is bound at
// compile time to the same contract a third-party integration would see.
type Client struct {
	endpoint string
	http     *http.Client
	token    string
	ua       string
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient overrides the underlying client, chiefly for tests.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.http = h }
}

// WithToken sets a bearer token.
//
// Accepted and sent today even though the Coordinator ignores it: scripts that
// already set a token keep working unchanged when authentication is switched
// on. See docs/design/09-api.md section 10.
func WithToken(t string) ClientOption {
	return func(c *Client) { c.token = t }
}

// WithUserAgent sets the User-Agent.
func WithUserAgent(ua string) ClientOption {
	return func(c *Client) { c.ua = ua }
}

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.http.Timeout = d }
}

// NewClient builds a client for a Coordinator endpoint.
func NewClient(endpoint string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		http:     &http.Client{Timeout: 30 * time.Second},
		ua:       "transferctl/" + APIVersion,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Endpoint returns the configured base URL.
func (c *Client) Endpoint() string { return c.endpoint }

// Version fetches build information.
func (c *Client) Version(ctx context.Context) (*VersionResponse, error) {
	var out VersionResponse
	return &out, c.get(ctx, "/api/v1/system/version", &out)
}

// HealthCheck runs the deep dependency check.
func (c *Client) HealthCheck(ctx context.Context) (*HealthCheckResponse, error) {
	var out HealthCheckResponse
	// The colon is part of the AIP-136 custom method name, not a path
	// separator, so it must not be escaped.
	return &out, c.get(ctx, "/api/v1/system:healthCheck", &out)
}

// ListProducts returns configured products.
func (c *Client) ListProducts(ctx context.Context) (*ListProductsResponse, error) {
	var out ListProductsResponse
	return &out, c.get(ctx, "/api/v1/products", &out)
}

// GetProduct returns one product.
func (c *Client) GetProduct(ctx context.Context, name string) (*Product, error) {
	var out Product
	return &out, c.get(ctx, "/api/v1/products/"+url.PathEscape(name), &out)
}

// ListPackages returns a product's discovered packages.
func (c *Client) ListPackages(ctx context.Context, product string, opts ListPackagesOptions) (*ListPackagesResponse, error) {
	path := "/api/v1/products/" + url.PathEscape(product) + "/packages"
	if q := opts.query(); q != "" {
		path += "?" + q
	}
	var out ListPackagesResponse
	return &out, c.get(ctx, path, &out)
}

// ListPackagesOptions filters a package listing.
type ListPackagesOptions struct {
	// Repository narrows to one repository path. A product may span several.
	Repository string
	Tag        string
	// State is the SCREAMING_SNAKE wire form, e.g. "DISCOVERED".
	State     string
	PageSize  int
	PageToken string
}

func (o ListPackagesOptions) query() string {
	q := url.Values{}
	if o.Repository != "" {
		q.Set("repository", o.Repository)
	}
	if o.Tag != "" {
		q.Set("tag", o.Tag)
	}
	if o.State != "" {
		q.Set("state", o.State)
	}
	if o.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(o.PageSize))
	}
	if o.PageToken != "" {
		q.Set("pageToken", o.PageToken)
	}
	return q.Encode()
}

// GetPackage returns one package. ref is a tag or a digest.
func (c *Client) GetPackage(ctx context.Context, product, ref string) (*Package, error) {
	var out Package
	return &out, c.get(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages/"+url.PathEscape(ref), &out)
}

// ListArtifacts returns a package's artifact tree.
func (c *Client) ListArtifacts(ctx context.Context, product, ref string) (*ListArtifactsResponse, error) {
	var out ListArtifactsResponse
	return &out, c.get(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages/"+url.PathEscape(ref)+"/artifacts", &out)
}

// DiscoverPackages triggers an immediate scan.
//
// The colon in the path is an AIP-136 custom method and must NOT be escaped —
// it is a structural separator, not data.
func (c *Client) DiscoverPackages(ctx context.Context, product, source string) (*DiscoverPackagesResponse, error) {
	return c.discover(ctx, product, DiscoverPackagesRequest{Source: source})
}

// StartDiscovery triggers a scan without waiting for it to finish.
//
// For a registry slow enough that holding an HTTP request open for the whole
// scan is a bad trade. Progress then comes from DiscoveryStatus.
func (c *Client) StartDiscovery(ctx context.Context, product, source string) (*DiscoverPackagesResponse, error) {
	no := false
	return c.discover(ctx, product, DiscoverPackagesRequest{Source: source, Wait: &no})
}

func (c *Client) discover(ctx context.Context, product string, req DiscoverPackagesRequest) (*DiscoverPackagesResponse, error) {
	var out DiscoverPackagesResponse
	err := c.post(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages:discover", req, &out)
	return &out, err
}

// InspectPackage expands one package's manifest tree and returns its size.
//
// Slow by nature: it reads from the source registry. Idempotent — the tree
// under a digest cannot change — so a second call is cheap and honest about it.
func (c *Client) InspectPackage(ctx context.Context, product, ref string) (*InspectPackageResponse, error) {
	var out InspectPackageResponse
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	err := c.post(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages/"+url.PathEscape(ref)+":inspect",
		struct{}{}, &out)
	return &out, err
}

// DiscoveryStatus reports what discovery is doing for one product right now.
//
// Safe to poll: it is a read of in-memory counters, not a scan.
func (c *Client) DiscoveryStatus(ctx context.Context, product string) (*DiscoveryStatusResponse, error) {
	var out DiscoveryStatusResponse
	return &out, c.get(ctx, "/api/v1/products/"+url.PathEscape(product)+"/discovery", &out)
}

// CheckConnectivity probes a product's registries, or every product's when
// product is empty.
//
// Slow by nature: it makes real calls to third-party registries. Deliberately
// separate from HealthCheck, which must not depend on them.
func (c *Client) CheckConnectivity(ctx context.Context, product string) (*CheckConnectivityResponse, error) {
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	path := "/api/v1/products:checkConnectivity"
	if product != "" {
		path = "/api/v1/products/" + url.PathEscape(product) + ":checkConnectivity"
	}
	var out CheckConnectivityResponse
	return &out, c.post(ctx, path, struct{}{}, &out)
}

// transportError classifies a failure to get any response at all.
//
// The distinction is worth the code because the two cases are diagnosed in
// opposite directions, and because Go's own message for a client timeout —
// "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
// — names neither the timeout that was in force nor the flag that changes it.
func (c *Client) transportError(err error) error {
	var nerr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &nerr) && nerr.Timeout())

	if !timedOut {
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, c.endpoint, err)
	}
	if c.http.Timeout > 0 {
		return fmt.Errorf("%w after %s: %s did not answer in time: %w",
			ErrTimeout, c.http.Timeout, c.endpoint, err)
	}
	return fmt.Errorf("%w: %s did not answer in time: %w", ErrTimeout, c.endpoint, err)
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return decodeProblem(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(err)
	}
	defer func() {
		// Drain before closing so the connection can be reused rather than
		// torn down; at CLI volumes this is hygiene rather than performance,
		// but the same client is used by long-lived callers.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return decodeProblem(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// decodeProblem turns an error response into a *Problem.
//
// A server that returns a non-problem body (a proxy error page, say) still
// yields a usable Problem rather than a decode failure — otherwise the user
// sees "invalid character '<'" instead of the actual status.
func decodeProblem(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var p Problem
	if err := json.Unmarshal(body, &p); err == nil && p.Code != "" {
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return &p
	}

	detail := strings.TrimSpace(string(body))
	if len(detail) > 200 {
		detail = detail[:200] + "…"
	}
	if detail == "" {
		detail = resp.Status
	}
	return &Problem{
		Status:    resp.StatusCode,
		Code:      codeForStatus(resp.StatusCode),
		Title:     http.StatusText(resp.StatusCode),
		Detail:    detail,
		RequestID: resp.Header.Get("X-Request-Id"),
	}
}

func codeForStatus(status int) Code {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeFailedPrecondition
	case http.StatusPreconditionFailed:
		return CodeAborted
	case http.StatusTooManyRequests:
		return CodeResourceExhausted
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	default:
		return CodeInternal
	}
}
