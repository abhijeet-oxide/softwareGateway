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
// unreachable - which is what this client used to do - sends operators to
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

// ComparePackage walks two places and reports what is different.
//
// The reference names the FIRST end's version; everything about the second is
// in the request, because the two ends are symmetric - source against target,
// target against target, and one place at two versions are the same call.
func (c *Client) ComparePackage(
	ctx context.Context, product, ref string, req CompareRequest,
) (*CompareResponse, error) {
	seg, query := splitPackageRef(ref)
	// The colon before the verb is an AIP-136 structural separator and must NOT
	// be escaped; the reference itself is escaped as one segment.
	path := "/api/v1/products/" + url.PathEscape(product) +
		"/packages/" + url.PathEscape(seg) + ":compare" + query
	var out CompareResponse
	return &out, c.post(ctx, path, req, &out)
}

// CompareProgress reads where a comparison has got to.
//
// Polled WHILE ComparePackage is still in flight, using the token that request
// carried. A 404 is a normal answer - progress lives in the memory of the
// replica running the comparison and is dropped shortly after it finishes - so
// a caller treats it as "no position available", not as a failure.
func (c *Client) CompareProgress(
	ctx context.Context, token string,
) (*CompareProgressResponse, error) {
	var out CompareProgressResponse
	return &out, c.get(ctx, "/api/v1/comparisons/"+url.PathEscape(token), &out)
}

// ListUnavailable returns what a product's sources would not serve.
func (c *Client) ListUnavailable(
	ctx context.Context, product string,
) (*ListUnavailableResponse, error) {
	var out ListUnavailableResponse
	return &out, c.get(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/unavailable", &out)
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
// So the user-facing spelling stays `orbs/core:v1` - it is what a person has
// in their hand - and the wire form is `/packages/v1?repository=orbs/core`.
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
// The colon in the path is an AIP-136 custom method and must NOT be escaped -
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
// Slow by nature: it reads from the source registry. Idempotent - the tree
// under a digest cannot change - so a second call is cheap and honest about it.
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

// ControlTransfer applies pause, resume, stop or delete to one transfer.
//
// One method for the four because they differ only in the verb: the states
// each admits belong to the server, which owns the state machine, and
// duplicating that knowledge here would be a second place for it to be wrong.
func (c *Client) ControlTransfer(
	ctx context.Context, id, verb string,
) (*TransferControlResponse, error) {
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	path := "/api/v1/transfers/" + url.PathEscape(id) + ":" + verb
	var out TransferControlResponse
	return &out, c.post(ctx, path, struct{}{}, &out)
}

// SetTransferPriority reorders what a transfer has left to do.
//
// Its own method rather than a verb on ControlTransfer, because it is the one
// control verb that carries a value - and a signature that took `verb, body`
// would let any of the others be called with a body they do not read.
func (c *Client) SetTransferPriority(
	ctx context.Context, id string, priority int,
) (*TransferControlResponse, error) {
	// The colon is an AIP-136 structural separator and must NOT be escaped.
	path := "/api/v1/transfers/" + url.PathEscape(id) + ":setPriority"
	var out TransferControlResponse
	return &out, c.post(ctx, path, SetPriorityRequest{Priority: &priority}, &out)
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
// their timeout to match - the sweep alone is roughly budget × (levels + 2) per
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
// opposite directions, and because Go's own message for a client timeout -
// "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
// - names neither the timeout that was in force nor the flag that changes it.
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
// yields a usable Problem rather than a decode failure - otherwise the user
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
	case http.StatusGatewayTimeout:
		return CodeDeadlineExceeded
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

// ListTransferFailures summarises why a transfer is failing.
//
// Distinct from ListTransferJobs on purpose: that returns rows, this returns
// causes. A bundle whose manifests are all rejected produces one cause and
// hundreds of rows, and only one of those two is a diagnosis.
func (c *Client) ListTransferFailures(ctx context.Context, id string) (*ListFailuresResponse, error) {
	var out ListFailuresResponse
	if err := c.get(ctx, "/api/v1/transfers/"+url.PathEscape(id)+"/failures", &out); err != nil {
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

// ---------------------------------------------------------------------------
// Target replication (docs/design/18)
// ---------------------------------------------------------------------------

// ListReplication returns every target's replication state for one product.
func (c *Client) ListReplication(ctx context.Context, product string) (*ListReplicationResponse, error) {
	var out ListReplicationResponse
	return &out, c.get(ctx, "/api/v1/products/"+url.PathEscape(product)+"/replication", &out)
}

// GetReplication returns one target's replication state.
func (c *Client) GetReplication(ctx context.Context, product, target string) (*ReplicationView, error) {
	var out ReplicationView
	return &out, c.get(ctx, replicationPath(product, target), &out)
}

// ApplyReplication writes a target's configuration to its registry.
//
// A destructive plan comes back as 409 with NeedsConfirmation set and the plan
// in the body, so the caller can show what will happen before asking again
// with Confirm. That is why the 409 is decoded rather than returned as an
// error: the body is the whole point of the response.
func (c *Client) ApplyReplication(ctx context.Context, product, target string, req ApplyReplicationRequest) (*ApplyReplicationResponse, error) {
	var out ApplyReplicationResponse
	err := c.post(ctx, replicationPath(product, target)+":apply", req, &out)
	if err != nil && out.NeedsConfirmation {
		return &out, nil
	}
	return &out, err
}

// SyncReplication asks the registry to sync a mirror target now.
func (c *Client) SyncReplication(ctx context.Context, product, target string) (*SyncReplicationResponse, error) {
	var out SyncReplicationResponse
	return &out, c.post(ctx, replicationPath(product, target)+":sync", nil, &out)
}

// CancelSyncReplication stops an in-progress sync.
func (c *Client) CancelSyncReplication(ctx context.Context, product, target string) (*SyncReplicationResponse, error) {
	var out SyncReplicationResponse
	return &out, c.post(ctx, replicationPath(product, target)+":cancelSync", nil, &out)
}

// ListSyncs returns a target's observed sync history.
func (c *Client) ListSyncs(ctx context.Context, product, target string, pageSize int) (*ListSyncsResponse, error) {
	path := "/api/v1/products/" + url.PathEscape(product) +
		"/targets/" + url.PathEscape(target) + "/syncs"
	if pageSize > 0 {
		path += "?pageSize=" + strconv.Itoa(pageSize)
	}
	var out ListSyncsResponse
	return &out, c.get(ctx, path, &out)
}

func replicationPath(product, target string) string {
	return "/api/v1/products/" + url.PathEscape(product) +
		"/targets/" + url.PathEscape(target) + "/replication"
}

// ---------------------------------------------------------------------------
// Downloads and auto-download (docs/design/20)
// ---------------------------------------------------------------------------

// ListDownloads returns a product's downloads with their derived chains.
func (c *Client) ListDownloads(ctx context.Context, product string) (*ListDownloadsResponse, error) {
	var out ListDownloadsResponse
	return &out, c.get(ctx, "/api/v1/products/"+url.PathEscape(product)+"/downloads", &out)
}

// RunDownload downloads named software by hand.
func (c *Client) RunDownload(ctx context.Context, product string, req RunDownloadRequest) (*RunDownloadResponse, error) {
	var out RunDownloadResponse
	return &out, c.post(ctx, "/api/v1/products/"+url.PathEscape(product)+"/downloads:run", req, &out)
}

// ListAutoDownloadRules returns a product's auto-download rules.
func (c *Client) ListAutoDownloadRules(ctx context.Context, product string) (*ListAutoDownloadRulesResponse, error) {
	var out ListAutoDownloadRulesResponse
	return &out, c.get(ctx, "/api/v1/products/"+url.PathEscape(product)+"/autoDownloadRules", &out)
}

// RuleMatches reports which discovered packages a rule would pick up.
func (c *Client) RuleMatches(ctx context.Context, product, rule string) (*MatchesResponse, error) {
	path := "/api/v1/products/" + url.PathEscape(product) +
		"/autoDownloadRules/" + url.PathEscape(rule) + "/matches"
	var out MatchesResponse
	return &out, c.get(ctx, path, &out)
}

// ---------------------------------------------------------------------------
// Audit, reports and identity
// ---------------------------------------------------------------------------

// AuditQuery narrows the audit trail. Every field is optional.
type AuditQuery struct {
	Product     string
	EventType   string
	Actor       string
	Outcome     string
	SubjectKind string
	SubjectID   string
	// Since and Until are RFC 3339. An unparseable bound is rejected by the
	// server rather than ignored, so a filter never silently widens.
	Since string
	Until string

	PageSize  int
	PageToken string
}

func (q AuditQuery) values() url.Values {
	v := url.Values{}
	for key, val := range map[string]string{
		"product":     q.Product,
		"eventType":   q.EventType,
		"actor":       q.Actor,
		"outcome":     q.Outcome,
		"subjectKind": q.SubjectKind,
		"subjectId":   q.SubjectID,
		"since":       q.Since,
		"until":       q.Until,
		"pageToken":   q.PageToken,
	} {
		if val != "" {
			v.Set(key, val)
		}
	}
	if q.PageSize > 0 {
		v.Set("pageSize", strconv.Itoa(q.PageSize))
	}
	return v
}

// ListAuditEvents queries the audit trail, newest first.
func (c *Client) ListAuditEvents(ctx context.Context, q AuditQuery) (*ListAuditEventsResponse, error) {
	path := "/api/v1/auditEvents"
	if v := q.values(); len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out ListAuditEventsResponse
	return &out, c.get(ctx, path, &out)
}

// ReportQuery bounds a report.
//
// Period and Since/Until are two spellings of one window; setting both is
// rejected rather than resolved by precedence, so a caller is never shown a
// period they did not mean.
type ReportQuery struct {
	// Period is a day count such as "7d" or "30d". Defaults to 30d.
	Period string
	Since  string
	Until  string
	// Product narrows to one product. Empty reports every product the caller
	// may see.
	Product string
}

// ReportSummary fetches the operational rollup for a period.
func (c *Client) ReportSummary(ctx context.Context, q ReportQuery) (*ReportSummary, error) {
	v := url.Values{}
	for key, val := range map[string]string{
		"period":  q.Period,
		"since":   q.Since,
		"until":   q.Until,
		"product": q.Product,
	} {
		if val != "" {
			v.Set(key, val)
		}
	}
	path := "/api/v1/reports/summary"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out ReportSummary
	return &out, c.get(ctx, path, &out)
}

// WhoAmI reports the calling identity and what it may do.
//
// On a Coordinator without authentication this answers `anonymous` with
// `authenticated: false`, which is a fact worth being able to confirm rather
// than infer.
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmIResponse, error) {
	var out WhoAmIResponse
	return &out, c.get(ctx, "/api/v1/whoami", &out)
}

// ---------------------------------------------------------------------------
// Security
// ---------------------------------------------------------------------------

// PackageSecurityOptions modulates a posture read.
type PackageSecurityOptions struct {
	// Detail asks for every finding. Without it the response carries counts
	// alone, which is what a listing needs and what a listing must not pay a
	// megabyte per release to get.
	Detail bool
	// Refresh bypasses every cache and asks the scanner. Only a person pressing
	// refresh should set it.
	Refresh bool
	// ProgressToken is a caller-minted id it can poll at SecurityProgress while
	// this request is open. Optional and free to omit.
	ProgressToken string
}

// PackageSecurity returns one release's security posture.
//
// A 404 means this deployment has no scanner configured. That is an honest
// absence rather than a failure - the route is not registered when the
// dependency is missing - and a caller should render it as "not configured"
// rather than as an error.
func (c *Client) PackageSecurity(
	ctx context.Context, product, ref string, opts PackageSecurityOptions,
) (*PackageSecurityResponse, error) {
	seg, query := splitPackageRef(ref)
	q := url.Values{}
	if opts.Detail {
		q.Set("detail", "true")
	}
	if opts.Refresh {
		q.Set("refresh", "true")
	}
	if opts.ProgressToken != "" {
		q.Set("progressToken", opts.ProgressToken)
	}

	path := "/api/v1/products/" + url.PathEscape(product) +
		"/packages/" + url.PathEscape(seg) + "/security" + mergeQuery(query, q)
	var out PackageSecurityResponse
	return &out, c.get(ctx, path, &out)
}

// CompareSecurity classifies the security difference between two releases.
//
// The package in the path is the OLD release and req.Against names the new one,
// so every word in the answer - resolved, introduced - is written from the new
// release's point of view.
func (c *Client) CompareSecurity(
	ctx context.Context, product, ref string, req SecurityCompareRequest,
) (*SecurityComparisonResponse, error) {
	seg, query := splitPackageRef(ref)
	// The colon before the verb is an AIP-136 structural separator and must NOT
	// be escaped; the reference itself is escaped as one segment.
	path := "/api/v1/products/" + url.PathEscape(product) +
		"/packages/" + url.PathEscape(seg) + ":compareSecurity" + query
	var out SecurityComparisonResponse
	return &out, c.post(ctx, path, req, &out)
}

// SecuritySearchOptions filters a security search.
type SecuritySearchOptions struct {
	// Kind is cve, package or image. Empty lets the server infer it from the
	// query, which reads a pasted CVE identifier for what it is.
	Kind string
	// Exact requires a whole-value match rather than a contained one.
	Exact bool
	Limit int
}

// SearchSecurity finds a CVE, a package or an image across what the platform
// has already retrieved.
//
// It cannot answer "is this CVE anywhere in my estate", only "is it anywhere I
// have looked", and the response says so in Searched.Note. Reporting the first
// would mean scanning every release in the catalogue on every search.
func (c *Client) SearchSecurity(
	ctx context.Context, product, query string, opts SecuritySearchOptions,
) (*SecuritySearchResponse, error) {
	q := url.Values{}
	q.Set("q", query)
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	if opts.Exact {
		q.Set("exact", "true")
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	var out SecuritySearchResponse
	return &out, c.get(ctx,
		"/api/v1/products/"+url.PathEscape(product)+"/security/search?"+q.Encode(), &out)
}

// SecurityProgress reads where a security retrieval has got to.
//
// Polled WHILE the request carrying the token is still in flight. A 404 is a
// normal answer - progress lives in one replica's memory and is dropped shortly
// after the work finishes - so a caller treats it as "no position available".
func (c *Client) SecurityProgress(
	ctx context.Context, token string,
) (*SecurityProgressResponse, error) {
	var out SecurityProgressResponse
	return &out, c.get(ctx, "/api/v1/security/progress/"+url.PathEscape(token), &out)
}

// mergeQuery joins a reference's own query string with additional parameters.
//
// A package reference carries `?repository=` when its repository could not
// survive the path, and the security calls add their own parameters on top.
// Emitting two `?` produces a URL the server reads as one parameter with a very
// strange value, which is a bug that looks like a server problem.
func mergeQuery(existing string, extra url.Values) string {
	encoded := extra.Encode()
	switch {
	case existing == "" && encoded == "":
		return ""
	case existing == "":
		return "?" + encoded
	case encoded == "":
		return existing
	default:
		return existing + "&" + encoded
	}
}
