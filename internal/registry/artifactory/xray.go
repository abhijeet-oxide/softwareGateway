package artifactory

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
	"strings"
	"sync/atomic"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// XrayConfig is everything needed to reach one JFrog Xray.
//
// Every field except the four at the top is a COPY of the repository's own
// configuration (see XrayConfigFor). Nothing here is separately configurable by
// an operator, and that is the point: JFrog credentials already exist because
// JFrog is already a supported repository type, and a second credential model
// for the same host is a rotation hazard rather than a feature.
type XrayConfig struct {
	// Endpoint is the JFrog platform base URL. Empty derives it from Registry.
	Endpoint string
	// Registry is the docker registry host, used to derive Endpoint and to
	// label errors.
	Registry string
	// RepositoryKey is the Artifactory repository key, for path lookups.
	RepositoryKey string
	// Watches scopes results to named Xray watches.
	Watches []string

	Username string
	Password string

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

	Concurrency int
	BatchSize   int

	// PlainHTTP talks http:// to a development Xray. Never for a real one.
	PlainHTTP bool
}

// Defaults for the Xray path.
const (
	// DefaultRequestTimeout bounds one summary call. Generous compared with the
	// Quay control plane because an Xray summary over fifty checksums is a real
	// query against a vulnerability database rather than a configuration read.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultBatchSize is how many artifacts one summary call asks about.
	//
	// Xray accepts more. Fifty is chosen so that one slow or failed batch costs
	// fifty artifacts' results rather than a whole release's, and so that
	// progress advances visibly on a 157-artifact release instead of jumping
	// from nothing to everything.
	DefaultBatchSize = 50
	// DefaultConcurrency is how many batches may be in flight per repository.
	//
	// Six, not sixty. Xray's summary endpoint is expensive server-side and
	// rate-limited on hosted JFrog; the difference between six and sixty is not
	// ten times faster, it is a 429 storm and a slower answer.
	DefaultConcurrency = 6

	// retryAttempts and friends keep the Xray retry budget well inside the
	// request timeout, so a failing Xray surfaces as Xray's own error rather
	// than as our deadline. Same reasoning as the Quay control client.
	retryAttempts  = 3
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 4 * time.Second
)

// providerName is what every finding from this package is stamped with. It
// reaches cache rows, exports and the interface, so it does not change.
const providerName = "jfrog-xray"

// The artifact summary endpoints, newest first.
//
// v2 is the one worth having: v1 answers with the prose and the severity and
// omits `components` entirely, so every finding it produces has no package and
// no fixed version. v1 remains as a fallback for a platform that predates v2,
// where a finding with no package still beats no finding at all.
const (
	summaryPathV2 = "/xray/api/v2/summary/artifact"
	summaryPathV1 = "/xray/api/v1/summary/artifact"
)

// XrayClient speaks JFrog Xray's REST API.
//
// Deliberately not a registry implementation, for the same reason the Quay
// management client is not: this is `/xray/api`, which describes artifacts
// rather than serving them, and forcing it through registry.Source would mean
// stub methods that lie.
type XrayClient struct {
	http     *http.Client
	endpoint string
	username string
	password string
	watches  []string
	repoKey  string
	timeout  time.Duration
	batch    int
	logger   *slog.Logger

	// legacySummary is set once a platform has answered 404 for the v2 path.
	legacySummary atomic.Bool
}

// NewXrayClient builds a client against one JFrog platform.
//
// The transport is the same stack the artifact path uses - TLS trust, proxy,
// retry, rate limiting, tracing - with NO registry credentials attached,
// because the OCI Distribution token exchange is the wrong authentication for
// this endpoint and would answer a 401 here by asking a token realm that does
// not exist. The JFrog credential is attached per request as Basic instead,
// which is what Xray takes, and an access token presented in the password
// field works unchanged.
func NewXrayClient(cfg XrayConfig) (*XrayClient, error) {
	endpoint, err := resolveEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Username == "" && cfg.Password == "" {
		return nil, fmt.Errorf("xray: JFrog credentials are required (%w)", registry.ErrUnauthorized)
	}

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
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
		return nil, fmt.Errorf("xray: build transport: %w", err)
	}

	return &XrayClient{
		http:     httpClient,
		endpoint: endpoint,
		username: cfg.Username,
		password: cfg.Password,
		watches:  cfg.Watches,
		repoKey:  cfg.RepositoryKey,
		timeout:  timeout,
		batch:    batch,
		logger:   cfg.Logger,
	}, nil
}

// Endpoint returns the platform base URL this client talks to.
func (c *XrayClient) Endpoint() string { return c.endpoint }

// BatchSize is how many artifacts one summary call asks about.
func (c *XrayClient) BatchSize() int { return c.batch }

// resolveEndpoint decides where Xray lives.
//
// # Why this is not simply "the registry host"
//
// JFrog serves Docker two ways. A repository-path deployment puts everything on
// one hostname - `acme.jfrog.io/docker-local/app` - and there the platform base
// URL IS the registry host. A subdomain deployment gives each repository its
// own name - `acme-docker.jfrog.io/app` - and there it is not: Xray lives at
// `acme.jfrog.io` and asking the docker subdomain gets a 404 that reads like a
// missing artifact rather than a wrong base URL.
//
// There is no reliable way to tell the two apart from the hostname, so the
// derivation covers the common case and configuration covers the other. Getting
// this wrong is cheap to diagnose because the error names the URL it asked.
func resolveEndpoint(cfg XrayConfig) (string, error) {
	raw := strings.TrimSpace(cfg.Endpoint)
	if raw == "" {
		if cfg.Registry == "" {
			return "", fmt.Errorf("xray: an endpoint or a registry host is required")
		}
		scheme := "https://"
		if cfg.PlainHTTP {
			scheme = "http://"
		}
		raw = scheme + cfg.Registry
	}
	if !strings.Contains(raw, "://") {
		if cfg.PlainHTTP {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("xray: invalid endpoint %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("xray: invalid endpoint %q: no host", raw)
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

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// artifactSummaryRequest is POST /xray/api/v1/summary/artifact.
//
// Checksums and paths, both accepted, because Xray answers on either and the
// two cover different gaps: a checksum is exact and needs no knowledge of where
// the artifact is stored, and a path finds artifacts indexed before Xray
// recorded a manifest checksum we would recognise.
type artifactSummaryRequest struct {
	Checksums []string `json:"checksums,omitempty"`
	Paths     []string `json:"paths,omitempty"`
}

type artifactSummaryResponse struct {
	Artifacts []xrayArtifact   `json:"artifacts"`
	Errors    []xraySummaryErr `json:"errors"`
}

type xraySummaryErr struct {
	Identifier string `json:"identifier"`
	Error      string `json:"error"`
}

type xrayArtifact struct {
	General  xrayGeneral `json:"general"`
	Issues   []xrayIssue `json:"issues"`
	Licenses []struct {
		Name       string   `json:"name"`
		Components []string `json:"components"`
	} `json:"licenses"`
}

type xrayGeneral struct {
	ComponentID string `json:"component_id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	PkgType     string `json:"pkg_type"`
	SHA256      string `json:"sha256"`
	// ScanTime is not part of the documented summary payload on every Xray
	// version, so it is read opportunistically and its absence is normal.
	ScanTime string `json:"scan_time"`
}

type xrayIssue struct {
	IssueID     string   `json:"issue_id"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	IssueType   string   `json:"issue_type"`
	Severity    string   `json:"severity"`
	Provider    string   `json:"provider"`
	Created     string   `json:"created"`
	References  []string `json:"references"`
	// WatchName is present when the deployment scopes findings to watches.
	WatchName string `json:"watch_name"`

	CVEs []xrayCVE `json:"cves"`

	// Components is an ARRAY, and every field in it matters.
	//
	// It was declared as a map keyed by component id, which is the shape a
	// much older Xray used. Against a current one the key is simply absent from
	// the v1 payload and present as an array in v2 - so every finding was
	// emitted with no component and no fix, which is what put a dash in the
	// Package column of every row and 0% in the Fixable card.
	Components []xrayComponent `json:"components"`

	// ImpactPath locates the package inside the image, as
	// ".../sha256__<layer>.tar.gz/3.23:libssl3:3.5.5-r0". It is the only
	// component information a v1 payload carries, and the fallback when an
	// issue arrives without the array above.
	ImpactPath []string `json:"impact_path"`
}

// xrayComponent is the vulnerable package, as Xray v2 reports it.
//
// The id does NOT carry the version - `3.23:libcrypto3` with `3.5.5-r0`
// alongside - which is the opposite of the `deb://openssl:1.1.1n` form the
// older payload used, and the reason the id is not parsed on its own here.
type xrayComponent struct {
	ComponentID   string     `json:"component_id"`
	Version       string     `json:"version"`
	PkgType       string     `json:"pkg_type"`
	FixedVersions []string   `json:"fixed_versions"`
	ImpactPaths   [][]string `json:"impact_paths"`
}

type xrayCVE struct {
	CVE string `json:"cve"`
	// The v2 form: one string carrying score and vector,
	// "7.5/CVSS:3.1/AV:N/AC:L/...". The *Score and *Vector fields below are the
	// v1 form. Both are read, because which one arrives is a fact about the
	// Xray version rather than about the finding.
	CVSSV2 string `json:"cvss_v2"`
	CVSSV3 string `json:"cvss_v3"`
	CVSSV4 string `json:"cvss_v4"`

	CVSSV2Score  any    `json:"cvss_v2_score"`
	CVSSV3Score  any    `json:"cvss_v3_score"`
	CVSSV2Vector string `json:"cvss_v2_vector"`
	CVSSV3Vector string `json:"cvss_v3_vector"`

	CWE []string `json:"cwe"`
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

// Ping verifies Xray answers and the JFrog credential is accepted here.
//
// Its own call rather than inferring health from a summary, because "Xray is
// unreachable" and "this artifact is not indexed" are the two states most often
// confused with each other, and confusing them is how an unscanned release
// reads as a clean one.
func (c *XrayClient) Ping(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/xray/api/v1/system/ping", nil, &out); err != nil {
		return err
	}
	if out.Status != "" && !strings.EqualFold(out.Status, "pong") && !strings.EqualFold(out.Status, "ok") {
		return &registry.Error{
			Op: "GET /xray/api/v1/system/ping", Repository: hostOf(c.endpoint),
			Detail: "unexpected status " + out.Status, Err: registry.ErrUnavailable,
		}
	}
	return nil
}

// ArtifactSummary asks Xray about a batch of artifacts, by checksum.
//
// Returns results keyed by the checksum asked about, plus the identifiers Xray
// declined to answer for and what it said. The second return value is what
// becomes StatusNotScanned - it is an ANSWER, not a failure, and losing it
// would make an unindexed artifact indistinguishable from a clean one.
func (c *XrayClient) ArtifactSummary(ctx context.Context, checksums []string) (map[string]xrayArtifact, map[string]string, error) {
	if len(checksums) == 0 {
		return map[string]xrayArtifact{}, map[string]string{}, nil
	}

	req := artifactSummaryRequest{Checksums: normalizeChecksums(checksums)}
	var resp artifactSummaryResponse
	if err := c.do(ctx, http.MethodPost, c.summaryPath(), req, &resp); err != nil {
		// A platform without v2 answers 404 or 405 for it. Fall back once,
		// remember, and carry on: the alternative is a deployment that reports
		// its whole estate unscannable because of an API version.
		if c.fallBackToV1(err) {
			if err := c.do(ctx, http.MethodPost, summaryPathV1, req, &resp); err != nil {
				return nil, nil, err
			}
		} else {
			return nil, nil, err
		}
	}

	byChecksum := make(map[string]xrayArtifact, len(resp.Artifacts))
	for _, a := range resp.Artifacts {
		key := strings.ToLower(strings.TrimSpace(a.General.SHA256))
		if key == "" {
			continue
		}
		byChecksum[key] = a
	}

	notIndexed := make(map[string]string, len(resp.Errors))
	for _, e := range resp.Errors {
		notIndexed[strings.ToLower(strings.TrimSpace(e.Identifier))] = e.Error
	}
	return byChecksum, notIndexed, nil
}

// summaryPath is v2 until a platform says it does not have it.
func (c *XrayClient) summaryPath() string {
	if c.legacySummary.Load() {
		return summaryPathV1
	}
	return summaryPathV2
}

// StoresChecksum reports whether Artifactory holds this content in the
// repository the scanner answers for.
//
// # Why this question is worth a request of its own
//
// Xray says one sentence for two situations: "Artifact doesn't exist or not
// indexed/cached in Xray". Those have nothing in common. An image that IS in
// the repository and merely un-indexed is a scanner job somebody can trigger;
// an image that was never replicated there is a TRANSFER that has not happened,
// and no amount of scanning will produce a result for it.
//
// Reported as one line - "142 images have no scan result" - the second kind
// quietly becomes an accusation against the scanner. This turns it back into a
// fact about the repository, which is where the fix is.
func (c *XrayClient) StoresChecksum(ctx context.Context, digest string) (bool, error) {
	hex := checksumOf(digest)
	if hex == "" || c.repoKey == "" {
		// With no repository key the question cannot be scoped, and a global
		// answer would report an image as present because some other
		// repository on the platform happens to hold it.
		return false, errNoRepositoryKey
	}

	var out struct {
		Results []struct {
			URI string `json:"uri"`
		} `json:"results"`
	}
	path := "/artifactory/api/search/checksum?sha256=" + url.QueryEscape(hex) +
		"&repos=" + url.QueryEscape(c.repoKey)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return false, err
	}
	return len(out.Results) > 0, nil
}

var errNoRepositoryKey = errors.New("xray: no Artifactory repository key is configured")

// fallBackToV1 records that this platform has no v2 summary endpoint.
func (c *XrayClient) fallBackToV1(err error) bool {
	if c.legacySummary.Load() {
		return false
	}
	var re *registry.Error
	if !errors.As(err, &re) {
		return false
	}
	switch re.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		c.legacySummary.Store(true)
		if c.logger != nil {
			c.logger.Info("xray: no v2 artifact summary on this platform, using v1",
				"endpoint", c.endpoint,
				"cost", "v1 carries no component or fix versions, so findings will have neither")
		}
		return true
	}
	return false
}

// normalizeChecksums strips the "sha256:" prefix Xray does not use and lowers
// the hex, so a caller can hand over an OCI digest unchanged.
func normalizeChecksums(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		h := checksumOf(s)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// checksumOf turns an OCI digest into the hex Xray indexes by.
func checksumOf(digest string) string {
	d := strings.TrimSpace(strings.ToLower(digest))
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[i+1:]
	}
	return d
}

// do performs one Xray request.
func (c *XrayClient) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("xray: encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("xray: build %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &registry.Error{
			Op: method + " " + path, Repository: hostOf(c.endpoint),
			Err: c.classifyTransport(ctx, err),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return c.statusError(method, path, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Bounded. An Xray summary over a large image is big but not unbounded, and
	// an unbounded decode against a proxy error page is a memory footgun.
	limited := io.LimitReader(resp.Body, 64<<20)
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		// A body that stopped arriving is a SLOW Xray, not an unreadable one.
		// Xray sends the headers for a fifty-checksum summary quickly and then
		// streams megabytes of issues, so a deadline lands HERE rather than on
		// Do - and calling that malformed classified the single most common
		// real failure as one that cannot be retried, which is why the batch
		// splitting written for it never once ran.
		if isDeadline(ctx, err) {
			return &registry.Error{
				Op: method + " " + path, Repository: hostOf(c.endpoint),
				StatusCode: resp.StatusCode,
				Detail: fmt.Sprintf(
					"Xray began answering but the response did not finish within %s.", c.timeout),
				Err: fmt.Errorf("%w: %w", registry.ErrTimeout, err),
			}
		}
		// Same argument, different symptom: the connection went away mid-body.
		// A truncated answer is a scanner under load, not one whose response
		// shape we do not know, and it is retryable on a smaller batch.
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return &registry.Error{
				Op: method + " " + path, Repository: hostOf(c.endpoint),
				StatusCode: resp.StatusCode,
				Detail:     "Xray closed the connection before the response finished.",
				Err:        fmt.Errorf("%w: %w", registry.ErrUnavailable, err),
			}
		}
		return &registry.Error{
			Op: method + " " + path, Repository: hostOf(c.endpoint),
			StatusCode: resp.StatusCode, Detail: err.Error(),
			Err: registry.ErrMalformedResponse,
		}
	}
	return nil
}

// classifyTransport keeps a deadline from being reported as "unreachable".
//
// The two send a reader to different places: one is a firewall or a wrong host,
// the other is an Xray that is answering and simply slower than the bound.
func (c *XrayClient) classifyTransport(ctx context.Context, err error) error {
	if isDeadline(ctx, err) {
		return fmt.Errorf("%w: %w", registry.ErrTimeout, err)
	}
	return err
}

// isDeadline reports whether a failure is this request running out of time,
// however it is wrapped.
func isDeadline(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// statusError turns Xray's error body into the classified vocabulary the rest
// of the system already reasons about.
func (c *XrayClient) statusError(method, path string, resp *http.Response) error {
	e := &registry.Error{
		Op:         method + " " + path,
		Repository: hostOf(c.endpoint),
		StatusCode: resp.StatusCode,
		Detail:     xrayDetail(resp.Body),
		Err:        registry.ClassifyStatus(resp.StatusCode),
	}
	// The two failures that are always the same mistake, and whose HTTP status
	// sends people to look in the wrong place.
	switch resp.StatusCode {
	case http.StatusNotFound:
		if e.Detail == "" {
			e.Detail = "Xray is not installed on this JFrog platform, or the endpoint is the docker subdomain rather than the platform base URL"
		}
	case http.StatusForbidden:
		if e.Detail == "" {
			e.Detail = "the JFrog credential is valid but has no Xray permission; a repository credential does not automatically grant one"
		}
	}
	return e
}

func xrayDetail(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Errors []struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"errors"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return firstLine(string(raw))
	}
	for _, e := range body.Errors {
		if e.Message != "" {
			return e.Message
		}
	}
	if body.Error != "" {
		return body.Error
	}
	return body.Message
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
