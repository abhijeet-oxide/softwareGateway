// Package v1 is the public API surface of softwareGateway.
//
// This is the ONLY package outside internal/. It is what transferctl uses and
// what a third-party integration would import — a compile-time commitment to
// the contract in docs/design/09-api.md, rather than a convention.
//
// Conventions (docs/design/09-api.md section 1):
//   - lowerCamelCase JSON field names (AIP-140)
//   - SCREAMING_SNAKE_CASE enum values (AIP-126)
//   - int64 serialized as STRING (AIP-141) — see the note on Int64String
//   - RFC 3339 UTC timestamps
package v1

// APIVersion is the served API version.
const APIVersion = "v1"

// Int64String carries a 64-bit quantity over JSON as a string.
//
// JSON numbers are IEEE-754 doubles and lose precision above 2^53. Byte counts
// here already reach 10^11, so a plain int64 would be silently rounded — rare
// enough to survive testing, routine enough to corrupt production reporting.
// AIP-141 requires the string form for exactly this reason.
type Int64String string

// ---------------------------------------------------------------------------
// System
// ---------------------------------------------------------------------------

// VersionResponse is returned by GET /api/v1/system/version.
// These four identifiers are what is needed to interpret a bug report.
type VersionResponse struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	BuildDate  string `json:"buildDate"`
	GoVersion  string `json:"goVersion"`
	APIVersion string `json:"apiVersion"`
	Component  string `json:"component"`
	// SchemaVersion is the highest applied migration, so a client can tell
	// whether the binary and the database agree.
	SchemaVersion int64 `json:"schemaVersion"`
}

// HealthStatus is a dependency outcome.
type HealthStatus string

const (
	HealthHealthy  HealthStatus = "HEALTHY"
	HealthDegraded HealthStatus = "DEGRADED"
	HealthDown     HealthStatus = "DOWN"
)

// HealthCheckResponse is returned by GET /api/v1/system:healthCheck.
//
// This is the DEEP check — it validates connectivity to every configured
// dependency and may be slow. It is deliberately not what Kubernetes polls:
// see docs/design/09-api.md section 9.1.
type HealthCheckResponse struct {
	Status    HealthStatus  `json:"status"`
	Component string        `json:"component"`
	Version   string        `json:"version"`
	Leader    bool          `json:"leader"`
	Checks    []HealthCheck `json:"checks"`
}

// HealthCheck is one dependency's outcome.
type HealthCheck struct {
	Name      string       `json:"name"`
	Status    HealthStatus `json:"status"`
	LatencyMs float64      `json:"latencyMs"`
	Detail    string       `json:"detail,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// ReadyResponse is returned by GET /readyz.
type ReadyResponse struct {
	Status HealthStatus  `json:"status"`
	Checks []HealthCheck `json:"checks"`
}

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

// Product is the API view of a configured product.
//
// Deliberately not the internal document: configuration is GitOps-managed and
// read-only over the API, and the internal type carries fields (resolved
// credentials, source file paths) that must never leave the process.
type Product struct {
	// Name is the AIP-122 resource name: "products/{product}".
	Name        string            `json:"name"`
	ProductID   string            `json:"productId"`
	DisplayName string            `json:"displayName,omitempty"`
	Description string            `json:"description,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`

	// Enabled reports whether the product runs. A disabled product is still
	// loaded, validated and listed — it simply does nothing.
	Enabled bool `json:"enabled"`

	Sources []Repository `json:"sources"`
	Targets []Repository `json:"targets"`

	AutoDownload AutoDownloadSummary `json:"autoDownload"`
	Verification VerificationSummary `json:"verification"`

	// ConfigHash identifies the exact document this view was built from, so an
	// operator can confirm which revision the Coordinator actually loaded.
	ConfigHash string `json:"configHash"`
}

// Repository is the API view of a source or target.
// Credentials are never included, in any form.
type Repository struct {
	Name string `json:"name"`
	// Enabled reports whether this source or target participates.
	Enabled  bool   `json:"enabled"`
	Registry string `json:"registry"`
	// Repository is the single repository path, for a target or a
	// single-repository source. Empty when a source covers several.
	Repository string `json:"repository,omitempty"`
	// Repositories is every path this covers. A source may span many; a target
	// is always exactly one.
	Repositories []string `json:"repositories,omitempty"`
	// RepositoryDiscovery reports that this source names no repositories and
	// therefore finds them from the registry catalog.
	RepositoryDiscovery bool `json:"repositoryDiscovery,omitempty"`
	// RepositoryFilters narrows the set. Its main use is the enumerated case.
	RepositoryFilters *Filters `json:"repositoryFilters,omitempty"`

	Type          string     `json:"type"`
	Role          string     `json:"role"`
	Default       bool       `json:"default,omitempty"`
	PromotionOnly bool       `json:"promotionOnly,omitempty"`
	Discovery     *Discovery `json:"discovery,omitempty"`
	RateLimits    RateLimits `json:"rateLimits"`
}

// Filters is an include/exclude pair of RE2 patterns.
type Filters struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

type Discovery struct {
	Enabled         bool     `json:"enabled"`
	IntervalSeconds int      `json:"intervalSeconds"`
	IncludePatterns []string `json:"includePatterns,omitempty"`
	ExcludePatterns []string `json:"excludePatterns,omitempty"`
}

// RateLimits are fleet-wide ceilings, not per-worker. The Coordinator divides
// them across active workers.
type RateLimits struct {
	MaxConcurrentDownloads int `json:"maxConcurrentDownloads"`
	MaxConcurrentUploads   int `json:"maxConcurrentUploads"`
	MaxConnections         int `json:"maxConnections"`
	RequestsPerSecond      int `json:"requestsPerSecond,omitempty"`
	Burst                  int `json:"burst,omitempty"`
}

type AutoDownloadSummary struct {
	Enabled bool               `json:"enabled"`
	Rules   []AutoDownloadRule `json:"rules,omitempty"`
}

type AutoDownloadRule struct {
	Name       string   `json:"name"`
	TagPattern string   `json:"tagPattern"`
	Targets    []string `json:"targets,omitempty"`
	Priority   int      `json:"priority"`
}

type VerificationSummary struct {
	Enabled       bool   `json:"enabled"`
	Policy        string `json:"policy,omitempty"`
	Mode          string `json:"mode,omitempty"`
	AtSource      bool   `json:"atSource"`
	AtDestination bool   `json:"atDestination"`
}

// ListProductsResponse is returned by GET /api/v1/products.
type ListProductsResponse struct {
	Products []Product `json:"products"`
	// NextPageToken is an opaque cursor. Empty means the last page.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// ---------------------------------------------------------------------------
// Enums used across future resources (docs/design/10-state-machines.md)
// ---------------------------------------------------------------------------

// PackageState mirrors packages.state, upper-cased for the wire.
type PackageState string

const (
	PackageDiscovered         PackageState = "DISCOVERED"
	PackageQueued             PackageState = "QUEUED"
	PackageTransferring       PackageState = "TRANSFERRING"
	PackageTransferred        PackageState = "TRANSFERRED"
	PackageVerifying          PackageState = "VERIFYING"
	PackageVerified           PackageState = "VERIFIED"
	PackageVerificationFailed PackageState = "VERIFICATION_FAILED"
	PackageFailed             PackageState = "FAILED"
	PackageSuperseded         PackageState = "SUPERSEDED"
)

// TransferState mirrors transfers.state.
type TransferState string

const (
	TransferPending    TransferState = "PENDING"
	TransferPlanning   TransferState = "PLANNING"
	TransferReady      TransferState = "READY"
	TransferRunning    TransferState = "RUNNING"
	TransferPaused     TransferState = "PAUSED"
	TransferVerifying  TransferState = "VERIFYING"
	TransferSucceeded  TransferState = "SUCCEEDED"
	TransferFailed     TransferState = "FAILED"
	TransferCancelling TransferState = "CANCELLING"
	TransferCancelled  TransferState = "CANCELLED"
)

// JobState mirrors jobs.state.
type JobState string

const (
	JobBlocked   JobState = "BLOCKED"
	JobPending   JobState = "PENDING"
	JobLeased    JobState = "LEASED"
	JobSucceeded JobState = "SUCCEEDED"
	// JobSkipped means the content was already present or was mounted — a
	// first-class success carrying zero bytes, not an exception.
	JobSkipped   JobState = "SKIPPED"
	JobFailed    JobState = "FAILED"
	JobCancelled JobState = "CANCELLED"
)

// SkipReason explains a SKIPPED job. Feeds the dedupe metrics.
type SkipReason string

const (
	SkipPlacementHit   SkipReason = "PLACEMENT_HIT"
	SkipExistsAtTarget SkipReason = "EXISTS_AT_TARGET"
	SkipMounted        SkipReason = "MOUNTED"
)

// Operation is the kind of work a transfer request asks for.
type Operation string

const (
	OperationReplicate Operation = "REPLICATE"
	OperationPromote   Operation = "PROMOTE"
	OperationVerify    Operation = "VERIFY"
)

// ---------------------------------------------------------------------------
// Packages
// ---------------------------------------------------------------------------

// Package is the API view of a discovered software package.
//
// Identity is (source repository, tag, manifest digest) — the digest is part of
// it, which is why a re-pushed tag produces a second Package rather than
// mutating the first (docs/design/01 §2.2).
type Package struct {
	// Name is the AIP-122 resource name: "products/{product}/packages/{package}".
	Name      string `json:"name"`
	PackageID string `json:"packageId"`
	Product   string `json:"product"`

	Tag            string `json:"tag"`
	ManifestDigest string `json:"manifestDigest"`
	MediaType      string `json:"mediaType"`

	// TotalBytes counts each distinct digest ONCE. A fat index whose platforms
	// share a base layer transfers that layer once, so summing naively would
	// overstate the cost — sometimes several-fold.
	TotalBytes    Int64String `json:"totalBytes"`
	ArtifactCount int         `json:"artifactCount"`
	BlobCount     int         `json:"blobCount"`

	State        PackageState `json:"state"`
	DiscoveredAt string       `json:"discoveredAt"`

	// SupersededBy names the package that replaced this one. Set only when the
	// SAME TAG was re-pushed with different content; different tags never
	// supersede each other.
	SupersededBy string `json:"supersededBy,omitempty"`

	// SourceRepository is the repository path it was discovered in, e.g.
	// "suite/core". A product may span several.
	SourceRepository string `json:"sourceRepository,omitempty"`
}

// ListPackagesResponse is returned by GET /api/v1/products/{product}/packages.
type ListPackagesResponse struct {
	Packages      []Package `json:"packages"`
	NextPageToken string    `json:"nextPageToken,omitempty"`
}

// Artifact is one manifest in a package's tree.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	ParentID   string `json:"parentId,omitempty"`

	Digest       string      `json:"digest"`
	MediaType    string      `json:"mediaType"`
	ArtifactType string      `json:"artifactType,omitempty"`
	SizeBytes    Int64String `json:"sizeBytes"`
	// Platform is "linux/amd64". Empty for non-image artifacts such as Helm
	// charts and configuration bundles.
	Platform string `json:"platform,omitempty"`
	// Depth is 0 for the root; an index's children are 1.
	Depth int `json:"depth"`
}

// ListArtifactsResponse is returned by
// GET /api/v1/products/{product}/packages/{package}/artifacts.
type ListArtifactsResponse struct {
	Artifacts []Artifact `json:"artifacts"`
}

// TransferRequest is the API view of requested work.
type TransferRequest struct {
	RequestID string    `json:"requestId"`
	Operation Operation `json:"operation"`
	Priority  int       `json:"priority"`
	State     string    `json:"state"`
	// Origin is API, CLI, AUTO_DOWNLOAD or SCHEDULE.
	Origin       string `json:"origin"`
	RequestedBy  string `json:"requestedBy"`
	AutoRuleName string `json:"autoRuleName,omitempty"`
}

// DiscoverPackagesRequest is the body of the packages:discover custom method.
type DiscoverPackagesRequest struct {
	// Source limits the scan to one source. Empty scans every source of the
	// product.
	Source string `json:"source,omitempty"`
}

// DiscoverPackagesResponse reports what a triggered scan did.
//
// Returned synchronously because a scan is bounded work — one HEAD per tag —
// and an operator triggering it after a vendor announcement wants the answer,
// not a job ID to poll.
type DiscoverPackagesResponse struct {
	// Repositories is how many were scanned. A source may cover several.
	Repositories int `json:"repositories"`
	// RepositoriesFromCatalog is how many came from `/v2/_catalog` rather than
	// from configuration.
	RepositoriesFromCatalog int `json:"repositoriesFromCatalog,omitempty"`
	// RepositoriesFiltered is how many candidates repositoryFilters rejected.
	RepositoriesFiltered int `json:"repositoriesFiltered,omitempty"`

	TagsListed   int `json:"tagsListed"`
	TagsAdmitted int `json:"tagsAdmitted"`
	// PackagesDiscovered counts genuinely new packages. Zero on a re-scan is
	// the expected, correct result.
	PackagesDiscovered int   `json:"packagesDiscovered"`
	Superseded         int   `json:"superseded"`
	RequestsCreated    int   `json:"requestsCreated"`
	DurationMs         int64 `json:"durationMs"`
	// TagErrors are per-tag failures that did not stop the scan.
	TagErrors []string `json:"tagErrors,omitempty"`
	// RepositoryErrors are per-repository failures that did not stop the scan.
	RepositoryErrors []string `json:"repositoryErrors,omitempty"`

	// Collapsed reports that a scan was ALREADY RUNNING when this request
	// arrived, so these numbers come from that scan rather than one this call
	// started. The data is real either way — the request waited for it — but the
	// two are different facts and the caller is told which one it got.
	Collapsed bool `json:"collapsed,omitempty"`
}

// ---------------------------------------------------------------------------
// Connectivity checks
// ---------------------------------------------------------------------------

// CheckStatus is one connectivity probe's outcome.
type CheckStatus string

const (
	CheckOK      CheckStatus = "OK"
	CheckFailed  CheckStatus = "FAILED"
	CheckWarning CheckStatus = "WARNING"
	// CheckSkipped means the probe did not apply, or an earlier one failed and
	// made it meaningless. Reported rather than omitted: "we did not check"
	// and "it passed" must not look the same.
	CheckSkipped CheckStatus = "SKIPPED"
)

// CheckStep is one probe against one repository.
type CheckStep struct {
	Name      string      `json:"name"`
	Status    CheckStatus `json:"status"`
	Detail    string      `json:"detail,omitempty"`
	Hint      string      `json:"hint,omitempty"`
	LatencyMs float64     `json:"latencyMs,omitempty"`
}

// RepositoryCheck is every probe for one configured repository.
type RepositoryCheck struct {
	Name       string      `json:"name"`
	Role       string      `json:"role"`
	Registry   string      `json:"registry"`
	Repository string      `json:"repository,omitempty"`
	Status     CheckStatus `json:"status"`
	Steps      []CheckStep `json:"steps"`
}

// ProductCheck is every repository of one product.
type ProductCheck struct {
	Product      string            `json:"product"`
	Status       CheckStatus       `json:"status"`
	Repositories []RepositoryCheck `json:"repositories"`
}

// CheckConnectivityResponse is returned by the products:checkConnectivity
// custom method.
//
// Deliberately NOT part of the health check. Health answers "is the service
// working?" and backs readiness; if a vendor's outage made it fail, a vendor's
// bad afternoon would pull our pods out of service, and an operator could not
// tell whose fault an unhealthy reading was. This answers a different
// question — "is my configuration correct?" — makes real calls to third
// parties, and is run on demand.
type CheckConnectivityResponse struct {
	Status   CheckStatus    `json:"status"`
	Products []ProductCheck `json:"products"`
}
