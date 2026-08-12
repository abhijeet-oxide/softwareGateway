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
	State string
	// IncludeAccessories lists the signature and wrapper rows a vendor
	// publishes as their own tags, which are hidden by default because they are
	// not releases.
	IncludeAccessories bool
	PageSize           int
	PageToken          string
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
	if o.IncludeAccessories {
		q.Set("includeAccessories", "true")
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
	seg, query := splitPackageRef(ref)
	return &out, c.get(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages/"+url.PathEscape(seg)+query, &out)
}

// ListArtifacts returns a package's artifact tree.
func (c *Client) ListArtifacts(ctx context.Context, product, ref string) (*ListArtifactsResponse, error) {
	var out ListArtifactsResponse
	seg, query := splitPackageRef(ref)
	return &out, c.get(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages/"+url.PathEscape(seg)+"/artifacts"+query, &out)
}

// splitPackageRef moves the repository of a scoped reference into the query
// string, leaving a single path segment behind.
//
// A repository path contains slashes, and a slash cannot survive a URL path
// segment: %2F is decoded before routing, so `orbs/core:v1` arrives at the
// router as two segments and matches nothing. Percent-encoding it twice
// "works" and is the kind of thing that breaks the first time a proxy
// normalises the path.
//
// So the user-facing spelling stays `orbs/core:v1` — it is what a person has
// in their hand — and the wire form is `/packages/v1?repository=orbs/core`.
// A reference with no slash needs no rewriting and gets none, which keeps the
// common single-repository URL exactly as it was.
func splitPackageRef(ref string) (segment, query string) {
	i := strings.LastIndex(ref, ":")
	if i <= 0 || i == len(ref)-1 {
		return ref, ""
	}
	repo, tag := ref[:i], ref[i+1:]
	// A digest is `algorithm:hex`, not a repository and a tag.
	if !strings.Contains(repo, "/") {
		return ref, ""
	}
	return tag, "?repository=" + url.QueryEscape(strings.Trim(repo, "/"))
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

// DiscoverAll scans every product discovery is polling.
//
// Never blocks: a fleet-wide scan is minutes to hours of work. Progress comes
// from DiscoveryStatus, per product.
func (c *Client) DiscoverAll(ctx context.Context) (*DiscoverAllResponse, error) {
	var out DiscoverAllResponse
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	return &out, c.post(ctx, "/api/v1/products:discover", struct{}{}, &out)
}

// InspectPackage expands one package's manifest tree and returns its size.
//
// Slow by nature: it reads from the source registry. Idempotent — the tree
// under a digest cannot change — so a second call is cheap and honest about it.
func (c *Client) InspectPackage(ctx context.Context, product, ref string) (*InspectPackageResponse, error) {
	var out InspectPackageResponse
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	seg, query := splitPackageRef(ref)
	err := c.post(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/packages/"+url.PathEscape(seg)+":inspect"+query,
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

// ListWorkers reports the fleet.
func (c *Client) ListWorkers(ctx context.Context) (*ListWorkersResponse, error) {
	var out ListWorkersResponse
	return &out, c.get(ctx, "/api/v1/workers", &out)
}

// RetryTransfer requeues one transfer's failed jobs.
//
// Idempotent: a transfer with nothing failed reports zero requeued rather than
// failing, so a client that retried after a timeout is not punished for it.
func (c *Client) RetryTransfer(ctx context.Context, id string) (*RetryTransferResponse, error) {
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	path := "/api/v1/transfers/" + url.PathEscape(id) + ":retry"
	var out RetryTransferResponse
	return &out, c.post(ctx, path, struct{}{}, &out)
}

// RetryTransfers requeues the failed jobs of every transfer that has any.
//
// The shape an outage calls for: it does not fail one transfer, it fails every
// transfer that was running.
func (c *Client) RetryTransfers(ctx context.Context) (*RetryTransferResponse, error) {
	var out RetryTransferResponse
	return &out, c.post(ctx, "/api/v1/transfers:retry", struct{}{}, &out)
}

// Calibrate measures one source-to-target path and returns what to configure.
//
// The slowest call in this client by a wide margin: it moves real data in both
// directions for as long as the requested budget allows. Callers must raise
// their timeout to match — the sweep alone is roughly budget × (levels + 2) per
// side, and a client timeout that fires mid-run cancels the probes and reports
// the Coordinator as unreachable.
func (c *Client) Calibrate(
	ctx context.Context, product string, req CalibrateRequest,
) (*CalibrateResponse, error) {
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	path := "/api/v1/products/" + url.PathEscape(product) + ":calibrate"
	var out CalibrateResponse
	return &out, c.post(ctx, path, req, &out)
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

// ---------------------------------------------------------------------------
// The worker plane
// ---------------------------------------------------------------------------
//
// These four calls are the whole of a worker's conversation with the
// Coordinator. See docs/design/09-api.md §7.

// LeaseJobs asks for work.
//
// An empty Jobs slice is the normal answer, not an error: it means the queue
// has nothing this worker can take right now, and NextPollAfterSeconds says
// when to ask again.
func (c *Client) LeaseJobs(ctx context.Context, req LeaseRequest) (*LeaseResponse, error) {
	var out LeaseResponse
	if err := c.post(ctx, "/api/v1/jobs:lease", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportProgress records how far a blob has got.
//
// Best-effort: the caller should log a failure and carry on transferring
// rather than abandon a job over a dropped UI signal.
func (c *Client) ReportProgress(ctx context.Context, jobID string, req ProgressRequest) error {
	return c.post(ctx, "/api/v1/jobs/"+jobID+":reportProgress", req, nil)
}

// CompleteJob reports a finished job. This one is not lossy.
func (c *Client) CompleteJob(ctx context.Context, jobID string, req CompleteRequest) (*CompleteResponse, error) {
	var out CompleteResponse
	if err := c.post(ctx, "/api/v1/jobs/"+jobID+":complete", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat renews leases and learns which are gone.
func (c *Client) Heartbeat(ctx context.Context, workerID string, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var out HeartbeatResponse
	if err := c.post(ctx, "/api/v1/workers/"+workerID+":heartbeat", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

// ListTransfersOptions filters a transfer listing.
type ListTransfersOptions struct {
	Product   string
	State     string
	PageSize  int
	PageToken string
}

func (o ListTransfersOptions) query() string {
	v := url.Values{}
	if o.Product != "" {
		v.Set("product", o.Product)
	}
	if o.State != "" {
		v.Set("state", o.State)
	}
	if o.PageSize > 0 {
		v.Set("pageSize", strconv.Itoa(o.PageSize))
	}
	if o.PageToken != "" {
		v.Set("pageToken", o.PageToken)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// ListTransfers returns transfers, newest first.
func (c *Client) ListTransfers(ctx context.Context, opts ListTransfersOptions) (*ListTransfersResponse, error) {
	var out ListTransfersResponse
	if err := c.get(ctx, "/api/v1/transfers"+opts.query(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTransfer returns one transfer with its progress.
func (c *Client) GetTransfer(ctx context.Context, id string) (*Transfer, error) {
	var out Transfer
	if err := c.get(ctx, "/api/v1/transfers/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTransferJobs returns layer-level progress, optionally for one state.
func (c *Client) ListTransferJobs(ctx context.Context, id, state string) (*ListJobsResponse, error) {
	path := "/api/v1/transfers/" + url.PathEscape(id) + "/jobs"
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}

	var out ListJobsResponse
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTransfer asks for a package to be copied.
//
// One call serves both `transfers create` and `transfers promote`: the
// operation is derived server-side from what `from` resolves to, so the client
// sends intent rather than a classification it would have to keep in step with
// configuration it cannot see.
func (c *Client) CreateTransfer(ctx context.Context, req CreateTransferRequest) (*CreateTransferResponse, error) {
	var out CreateTransferResponse
	if err := c.post(ctx, "/api/v1/transfers", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
