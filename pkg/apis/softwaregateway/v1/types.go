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
	Name          string     `json:"name"`
	Registry      string     `json:"registry"`
	Repository    string     `json:"repository"`
	Type          string     `json:"type"`
	Role          string     `json:"role"`
	Default       bool       `json:"default,omitempty"`
	PromotionOnly bool       `json:"promotionOnly,omitempty"`
	Discovery     *Discovery `json:"discovery,omitempty"`
	RateLimits    RateLimits `json:"rateLimits"`
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
