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
	// Workers is the fleet. Included here because "are the workers up?" is the
	// first question of any incident, and a health check that answered
	// everything except that sent the reader somewhere else to find out.
	Workers []Worker `json:"workers,omitempty"`
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

	Type string `json:"type"`
	// Vendor is the publishing convention a SOURCE follows — `near`, or empty
	// for a conformant registry. Separate from Type, which is how to speak to
	// the registry: protocol and publishing convention vary independently.
	Vendor        string      `json:"vendor,omitempty"`
	Role          string      `json:"role"`
	Default       bool        `json:"default,omitempty"`
	PromotionOnly bool        `json:"promotionOnly,omitempty"`
	Discovery     *Discovery  `json:"discovery,omitempty"`
	Concurrency   Concurrency `json:"concurrency"`
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

// Concurrency is the RESOLVED limit in force for one registry — the product's
// override if it has one, otherwise the application-level default. Never the
// raw document value, so a client reading this sees what is actually happening
// rather than what was written down.
//
// Fleet-wide, not per-worker: a per-worker limit would silently multiply by the
// replica count and flatten a vendor registry the moment HPA scaled out.
type Concurrency struct {
	// PerRegistry is requests in flight against this registry, which is also the
	// connection pool size — they are the same limit.
	PerRegistry int `json:"perRegistry"`
	// RequestsPerSecond is an optional politeness ceiling. Zero means none.
	RequestsPerSecond int `json:"requestsPerSecond,omitempty"`
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
	//
	// OMITTED when not yet measured, which is the case for a package whose root
	// is an index: discovery records what the index lists without fetching it,
	// so the layer bytes underneath are unknown until a transfer walks the
	// tree. Absent rather than zero — a wrong size is worse than a missing one,
	// because nobody questions a number.
	TotalBytes *Int64String `json:"totalBytes,omitempty"`
	// ArtifactCount is always known: it is the root plus whatever its index
	// lists, both of which come from bytes we already hold.
	ArtifactCount int  `json:"artifactCount"`
	BlobCount     *int `json:"blobCount,omitempty"`

	State PackageState `json:"state"`
	// DiscoveredAt is when WE first saw it. An observation.
	DiscoveredAt string `json:"discoveredAt"`
	// PublishedAt is when the VENDOR says it was built, from the standard
	// org.opencontainers.image.created annotation. A claim, not an observation
	// — and omitted entirely when the publisher set none, which the OCI spec
	// permits.
	//
	// Kept separate from DiscoveredAt rather than folded into one "date",
	// because "published in March, we only noticed in July" is a fact worth
	// being able to see.
	PublishedAt string `json:"publishedAt,omitempty"`

	// SupersededBy names the package that replaced this one. Set only when the
	// SAME TAG was re-pushed with different content; different tags never
	// supersede each other.
	SupersededBy string `json:"supersededBy,omitempty"`

	// AccessoryOf names the package this row is PART OF — a signature or a
	// wrapper the vendor publishes as its own tag.
	//
	// Empty for an ordinary package, and empty for every package discovered
	// under a vendor that groups them, because such a tag never becomes a
	// package at all. Set only on rows recorded before their source declared a
	// vendor, which a later scan then grouped.
	//
	// A row with this set is hidden from listings by default: it is not a
	// release. It keeps its history and stays reachable by explicit reference.
	AccessoryOf string `json:"accessoryOf,omitempty"`

	// SourceRepository is the repository path it was discovered in, e.g.
	// "suite/core". A product may span several.
	SourceRepository string `json:"sourceRepository,omitempty"`

	// DisplayRepository is SourceRepository with the vendor's structural noise
	// removed — `cfx-5000-k8s` for NEAR's `orbs/cfx-5000-k8s`.
	//
	// Empty when no shortening applies, which is what a source with no `vendor`
	// set gets, and therefore what any conformant registry gets. Cosmetic only:
	// SourceRepository is the identity, and both spellings resolve as input.
	DisplayRepository string `json:"displayRepository,omitempty"`

	// DisplayTag is Tag with the vendor's structural noise removed, for tables.
	//
	// Empty when no shortening applies. Cosmetic only: Tag is the identity, and
	// both spellings resolve as input.
	DisplayTag string `json:"displayTag,omitempty"`

	// ExpandedAt is when this package's manifest tree was last fully walked, by
	// `packages inspect` or by a transfer. Empty means it never has been, which
	// is why TotalBytes and BlobCount are absent.
	ExpandedAt string `json:"expandedAt,omitempty"`

	// SignatureStatus is SIGNED, UNSIGNED or UNKNOWN.
	//
	// Three values, and the third is the one that matters: "we looked and found
	// none" and "nobody looked" are the same value in a boolean and completely
	// different facts when the question is whether to trust something. UNKNOWN
	// is what a source whose layout does not attempt signature discovery
	// reports — honestly, rather than claiming a confident UNSIGNED.
	SignatureStatus SignatureStatus `json:"signatureStatus,omitempty"`

	// Related are artifacts that belong to this package without living inside
	// its manifest tree — its signature, an SBOM, an attestation, or the
	// wrapper that bundles them.
	Related []RelatedArtifact `json:"related,omitempty"`

	// TransferRootTag names what a transfer actually walks, when that is not
	// this package's own tag.
	//
	// Empty in the ordinary case. Set where a vendor bundles the payload and
	// its signature under a wrapper: only the wrapper reaches both, so planning
	// from the payload alone would move the bytes and leave the signature
	// behind.
	TransferRootTag string `json:"transferRootTag,omitempty"`
}

// SignatureStatus is AIP-126 SCREAMING_SNAKE on the wire.
type SignatureStatus string

const (
	SignatureUnknown  SignatureStatus = "UNKNOWN"
	SignatureSigned   SignatureStatus = "SIGNED"
	SignatureUnsigned SignatureStatus = "UNSIGNED"
)

// RelatedArtifact is one artifact attached to a package.
//
// Role is vendor-neutral: SIGNATURE, never NOKIA_SIGNATURE. Which mechanism
// found it — the referrers API, a cosign tag, a wrapper index — is an
// implementation detail of the source's layout and deliberately not on the
// wire, so a vendor changing convention changes no client.
type RelatedArtifact struct {
	Role      string      `json:"role"`
	Digest    string      `json:"digest"`
	Tag       string      `json:"tag,omitempty"`
	MediaType string      `json:"mediaType,omitempty"`
	SizeBytes Int64String `json:"sizeBytes,omitempty"`

	// The MATERIAL, for a signature: Digest above names the manifest that
	// carries it, and these name what is inside — the blob a verifier reads.
	// For NEAR that is one layer of `application/pkcs7-signature`.
	//
	// Absent until the package has been inspected: the manifest has to be
	// fetched before its layers are known, and discovery deliberately does not
	// fetch it.
	BlobDigest    string      `json:"blobDigest,omitempty"`
	BlobMediaType string      `json:"blobMediaType,omitempty"`
	BlobSize      Int64String `json:"blobSize,omitempty"`
	// Annotations is the signature manifest's own annotation map, verbatim, so
	// a client reads vendor keys this API has never heard of.
	Annotations map[string]string `json:"annotations,omitempty"`
	// ResolvedAt is when the material was last confirmed. Empty means nobody has
	// inspected this package — which is a different fact from a signature that
	// carries no blob.
	ResolvedAt string `json:"resolvedAt,omitempty"`
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
	// Annotations is the artifact's annotation map, verbatim.
	//
	// Kept whole so a vendor's own keys — com.nokia.ncd.orb.type, say — reach
	// a caller without this API knowing they exist. The standard
	// org.opencontainers.* keys are in here too; only `created` is also
	// promoted to a column, because only it is worth sorting by.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Fetched reports whether this manifest was ever pulled and verified.
	//
	// Discovery fetches the tag's own manifest and records the children its
	// index lists, without a request each. A fetched manifest was verified
	// against its digest; a listed one has the vendor's word for it, and the
	// difference is worth being able to see.
	Fetched bool `json:"fetched"`
	// Cached reports whether the manifest's BYTES are still held locally.
	//
	// A separate axis from Fetched, and only false-with-Fetched-true is
	// interesting: it means the body was reclaimed by the manifest-cache
	// sweeper. Nothing is lost — the artifact, its blobs and its size are all
	// still recorded — but pushing it would re-read it from the source. See
	// store.SweepManifestCache.
	Cached bool `json:"cached"`
}

// InspectPackageResponse reports what expanding a package found.
//
// Discovery stops at the tag's own manifest, so a package's transfer size is
// unknown until something walks the tree. This is that something — and it is
// the same walk a transfer performs, so the numbers here are the numbers a
// transfer will move.
type InspectPackageResponse struct {
	Package Package `json:"package"`
	// Fetched is manifests fetched by THIS call. Zero means the package was
	// already expanded and the registry was not troubled again.
	Fetched int `json:"fetched"`
	// AlreadyExpanded reports exactly that, so a caller need not infer it from
	// a zero.
	AlreadyExpanded bool `json:"alreadyExpanded"`

	Artifacts  int         `json:"artifacts"`
	Blobs      int         `json:"blobs"`
	TotalBytes Int64String `json:"totalBytes"`

	// CachedManifests and CachedBytes describe how much of this package's
	// manifest bodies are still held locally, out of Artifacts.
	//
	// The bodies are an evictable cache with a budget, not part of the record —
	// what a package IS gets kept forever, what it was SERVED AS gets reclaimed
	// when the cache fills. Reported so the difference is visible rather than
	// discovered when a transfer takes longer than expected. Neither number
	// affects any of the ones above.
	CachedManifests int         `json:"cachedManifests"`
	CachedBytes     Int64String `json:"cachedBytes,omitempty"`
	// SignatureResolved is how many signature relations had their material
	// recorded — the blob a verifier reads, captured while the tree was in hand.
	SignatureResolved int `json:"signatureResolved,omitempty"`
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

	// Wait holds the request open until the scan finishes. Absent means true,
	// which is the historical behaviour and the useful default for a person at
	// a terminal.
	//
	// Set false to start the scan and return immediately. That matters against a
	// registry that is slow rather than broken: a scan there can run for
	// minutes, and holding an HTTP request open for the whole of it makes every
	// intermediary's idle timeout part of your control plane. Progress is then
	// read from GET .../discovery.
	Wait *bool `json:"wait,omitempty"`
}

// ShouldWait reports the effective value of Wait. Absent means wait.
func (r DiscoverPackagesRequest) ShouldWait() bool { return r.Wait == nil || *r.Wait }

// DiscoverPackagesResponse reports what a triggered scan did.
//
// Returned synchronously by default because a scan is usually bounded work —
// one HEAD per tag — and an operator triggering it after a vendor announcement
// wants the answer, not a job ID to poll. `wait: false` opts out when the
// registry is slow enough to make that a bad trade.
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
	PackagesDiscovered int `json:"packagesDiscovered"`
	Superseded         int `json:"superseded"`
	RequestsCreated    int `json:"requestsCreated"`
	// Renamed counts EXISTING packages whose display name was corrected because
	// the source's `vendor` changed. Zero on every steady-state scan; non-zero
	// exactly once after that field is edited.
	Renamed int `json:"renamed,omitempty"`
	// Regrouped counts EXISTING packages re-grouped under a newly declared
	// vendor — gaining their signature status, their related artifacts and their
	// transfer root. Same shape as Renamed: zero always, except on the one scan
	// that follows the edit.
	Regrouped  int   `json:"regrouped,omitempty"`
	DurationMs int64 `json:"durationMs"`
	// TagErrors are per-tag failures that did not stop the scan.
	TagErrors []string `json:"tagErrors,omitempty"`
	// RepositoryErrors are per-repository failures that did not stop the scan.
	RepositoryErrors []string `json:"repositoryErrors,omitempty"`

	// Collapsed reports that a scan was ALREADY RUNNING when this request
	// arrived, so these numbers come from that scan rather than one this call
	// started. The data is real either way — the request waited for it — but the
	// two are different facts and the caller is told which one it got.
	Collapsed bool `json:"collapsed,omitempty"`

	// Started is set instead of the counters when the request asked not to
	// wait: the scan is running, and there are no results yet to report.
	Started *DiscoverStarted `json:"started,omitempty"`
}

// DiscoverAllResponse reports a fleet-wide scan.
type DiscoverAllResponse struct {
	// Started and AlreadyRunning are totals across every product.
	Started        int `json:"started"`
	AlreadyRunning int `json:"alreadyRunning,omitempty"`

	Products []DiscoverAllProduct `json:"products"`
}

// DiscoverAllProduct is one product's outcome in a fleet-wide scan.
type DiscoverAllProduct struct {
	Product string `json:"product"`
	// Sources is how many began a new scan.
	Sources        int `json:"sources"`
	AlreadyRunning int `json:"alreadyRunning,omitempty"`
	// Error is set when this product could not be started. Reported per product
	// rather than failing the whole call: one broken source must not stop the
	// other thirty being scanned.
	Error string `json:"error,omitempty"`
}

// DiscoverStarted reports a scan that was launched without waiting.
type DiscoverStarted struct {
	// Sources is how many sources began a new scan.
	Sources int `json:"sources"`
	// AlreadyRunning is how many were already scanning and so were left alone.
	// Reported rather than folded into Sources: "I started four scans" and "one
	// started, three were already going" are different answers.
	AlreadyRunning int `json:"alreadyRunning,omitempty"`
}

// ---------------------------------------------------------------------------
// Discovery status
// ---------------------------------------------------------------------------

// DiscoveryStatusResponse is returned by GET
// /api/v1/products/{product}/discovery.
//
// It answers "what is discovery doing right now", which a synchronous scan
// cannot: a request that blocks for two minutes and then reports a timeout
// tells you nothing while it is blocked, and a slow registry is
// indistinguishable from a hung one until it finally answers.
type DiscoveryStatusResponse struct {
	// Running reports whether the discovery loop is active on this replica. It
	// runs on the leader only.
	Running bool                   `json:"running"`
	Sources []DiscoverySourceState `json:"sources"`
}

// DiscoverySourceState is one source's live and last-completed state.
type DiscoverySourceState struct {
	Product string `json:"product"`
	Source  string `json:"source"`

	// Scanning reports whether a scan is in flight right now.
	Scanning bool `json:"scanning"`
	// Phase is the stage of the running scan: ENUMERATING_REPOSITORIES,
	// LISTING_TAGS or RESOLVING_TAGS. Empty when idle.
	Phase string `json:"phase,omitempty"`
	// ElapsedMs is how long the running scan has been going.
	ElapsedMs int64 `json:"elapsedMs,omitempty"`

	// RepositoriesTotal is zero until enumeration finishes, which itself says
	// the scan is still waiting on /v2/_catalog.
	RepositoriesTotal int `json:"repositoriesTotal,omitempty"`
	RepositoriesDone  int `json:"repositoriesDone,omitempty"`
	// RepositoriesInFlight is how many are being scanned right now. Without it
	// a concurrent scan looks stalled for its first minute.
	RepositoriesInFlight int    `json:"repositoriesInFlight,omitempty"`
	CurrentRepository    string `json:"currentRepository,omitempty"`
	TagsTotal            int    `json:"tagsTotal,omitempty"`
	TagsResolved         int    `json:"tagsResolved,omitempty"`
	// Artifacts is manifests fetched so far. A single tag with a large artifact
	// tree takes minutes, during which this is the only counter that moves.
	Artifacts   int `json:"artifacts,omitempty"`
	NewPackages int `json:"newPackages,omitempty"`
	Errors      int `json:"errors,omitempty"`

	// LastRunAt and the fields below describe the last COMPLETED scan.
	LastRunAt        string `json:"lastRunAt,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	LastRepositories int    `json:"lastRepositories,omitempty"`
	LastTagsListed   int    `json:"lastTagsListed,omitempty"`
	LastNewPackages  int    `json:"lastNewPackages,omitempty"`
	LastDurationMs   int64  `json:"lastDurationMs,omitempty"`
	IntervalSeconds  int    `json:"intervalSeconds,omitempty"`
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

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// Worker is one member of the fleet, as the Coordinator last heard from it.
//
// Every field here already crossed the wire on a lease or a heartbeat and was
// read for one decision and dropped. Recording it is what lets `health` answer
// "are the workers up, and what are they doing" — a question that previously
// had no route at all.
type Worker struct {
	WorkerID string `json:"workerId"`
	Version  string `json:"version,omitempty"`
	// MaxConcurrency is the worker's configured ceiling: what it asked for plus
	// what it already held. The number in the operator's config file, not a
	// remainder that shrinks as work starts.
	MaxConcurrency int `json:"maxConcurrency"`
	// ActiveJobs is what the WORKER says it is running.
	ActiveJobs int `json:"activeJobs"`
	// LeasedJobs is what the COORDINATOR has leased to it. The two disagreeing
	// is worth seeing: a worker holding jobs the Coordinator has already reaped
	// is about to report completions nobody will accept.
	LeasedJobs int `json:"leasedJobs"`
	// State is ACTIVE, DRAINING or STALE. Stale is derived from the heartbeat
	// rather than announced — a worker that was killed never got to say so.
	State         string `json:"state"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
}

// ListWorkersResponse is returned by GET /api/v1/workers.
type ListWorkersResponse struct {
	Workers []Worker `json:"workers"`
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

// RetryTransferResponse is returned by `transfers/{transfer}:retry` and by the
// fleet-wide `transfers:retry`.
//
// One shape for both, so a client renders the single and the bulk case with the
// same code. The single form returns a list of one.
type RetryTransferResponse struct {
	Transfers []TransferRetry `json:"transfers"`
	// Requeued is the total across every transfer acted on.
	Requeued int `json:"requeued"`
}

// TransferRetry is what a retry did to one transfer.
type TransferRetry struct {
	TransferID string `json:"transferId"`
	Requeued   int    `json:"requeued"`
	State      string `json:"state,omitempty"`
	// Error explains why this one could not be retried, when it could not.
	//
	// Carried per transfer rather than failing the whole request: a fleet-wide
	// retry after an outage should restart everything restartable and then say
	// which ones it could not, not stop at the first exception.
	Error string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Calibration
// ---------------------------------------------------------------------------
//
// A sibling of the connectivity check and a different question: not "can we
// reach it" but "how fast is it, and what setting would make it faster". It
// moves real data in both directions and takes minutes, which is why it is its
// own custom method rather than a mode of the check.

// CalibrateRequest is POST /api/v1/products/{product}:calibrate.
type CalibrateRequest struct {
	// Source and Target name configured entries. Empty picks the product's
	// only source, and its default target.
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
	// SourceRepository overrides which repository the read probe reads.
	SourceRepository string `json:"sourceRepository,omitempty"`

	// Levels are the concurrency levels to sweep. Empty uses the default.
	Levels []int `json:"levels,omitempty"`
	// BudgetSeconds is how long ONE level runs.
	BudgetSeconds float64 `json:"budgetSeconds,omitempty"`

	// Write enables the target-side probe, which opens upload sessions and
	// cancels them. Nil means the server's default, which is on.
	Write *bool `json:"write,omitempty"`

	// BundleBytes projects the measured ceiling onto a transfer size.
	BundleBytes Int64String `json:"bundleBytes,omitempty"`
}

// CalibrateResponse is one calibration run.
type CalibrateResponse struct {
	Product string `json:"product"`
	// MeasuredFrom is the host that ran the probes, and it is load-bearing: a
	// measurement of the Coordinator's network describes the workers' network
	// only when they share one.
	MeasuredFrom string  `json:"measuredFrom"`
	StartedAt    string  `json:"startedAt"`
	DurationSec  float64 `json:"durationSeconds"`

	Source CalibrationSide `json:"source"`
	Target CalibrationSide `json:"target"`

	Suggestions []CalibrationSuggestion `json:"suggestions"`
	Notes       []string                `json:"notes,omitempty"`
}

// CalibrationSide is everything measured about one end of the path.
type CalibrationSide struct {
	Role       string `json:"role"`
	Name       string `json:"name"`
	Registry   string `json:"registry"`
	Repository string `json:"repository,omitempty"`

	Route CalibrationRoute `json:"route"`
	RTTMs float64          `json:"rttMs,omitempty"`

	// Samples and LargestSampleBytes say what the read probe opened, so a
	// throughput measured over signature blobs cannot be mistaken for one
	// measured over layers.
	Samples            int         `json:"samples,omitempty"`
	LargestSampleBytes Int64String `json:"largestSampleBytes,omitempty"`

	Levels []CalibrationLevel `json:"levels,omitempty"`
	// Knee is the smallest concurrency within a tenth of the best measured —
	// the level worth configuring.
	Knee int `json:"knee,omitempty"`
	// StillClimbing means the sweep ended before the path did.
	StillClimbing bool `json:"stillClimbing,omitempty"`
	// Skipped explains why there are no measurements, when there are none.
	Skipped string `json:"skipped,omitempty"`
}

// CalibrationRoute is what the traffic goes through, and what it would do the
// other way.
type CalibrationRoute struct {
	Configured      string  `json:"configured"`
	ProxyInUse      bool    `json:"proxyInUse"`
	DirectTested    bool    `json:"directTested,omitempty"`
	DirectReachable bool    `json:"directReachable,omitempty"`
	DirectDetail    string  `json:"directDetail,omitempty"`
	ProxiedRate     float64 `json:"proxiedRateBytesPerSecond,omitempty"`
	DirectRate      float64 `json:"directRateBytesPerSecond,omitempty"`
}

// CalibrationLevel is one concurrency level's measurement.
type CalibrationLevel struct {
	Concurrency int         `json:"concurrency"`
	Bytes       Int64String `json:"bytes"`
	Seconds     float64     `json:"seconds"`
	Rate        float64     `json:"rateBytesPerSecond"`
	PerStream   float64     `json:"perStreamBytesPerSecond"`
	Requests    int         `json:"requests"`
	Errors      int         `json:"errors,omitempty"`
	Throttled   int         `json:"throttled,omitempty"`
	TTFBMs      float64     `json:"ttfbMs,omitempty"`
	FirstError  string      `json:"firstError,omitempty"`
}

// CalibrationSuggestion is one thing to change, or one reason not to.
type CalibrationSuggestion struct {
	Severity string `json:"severity"`
	// Setting is the configuration key, in the spelling the file uses. Empty
	// for a finding with no knob behind it.
	Setting   string `json:"setting,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Current   string `json:"current,omitempty"`
	Suggested string `json:"suggested,omitempty"`
	// Evidence is the measurement the suggestion rests on. Never empty:
	// advice without a number is the guesswork calibration replaces.
	Evidence string `json:"evidence"`
}

// ---------------------------------------------------------------------------
// The worker plane
// ---------------------------------------------------------------------------
//
// Workers speak only to these types. They hold no database credentials, so
// everything needed to execute a job travels in the lease response — except
// the credential itself, which travels as a NAME the worker resolves against
// its own projected secret volume. No secret is ever serialized here.
//
// See docs/design/09-api.md §7.

// LeaseRequest is POST /api/v1/jobs:lease.
type LeaseRequest struct {
	WorkerID string `json:"workerId"`
	// Capacity is how many more jobs this worker can take right now, already
	// reduced by what it is holding.
	Capacity int `json:"capacity"`
	// ActiveJobs is how many it currently holds, for telemetry.
	ActiveJobs int `json:"activeJobs,omitempty"`
	// Version is the worker's build, recorded on the workers row.
	Version string `json:"version,omitempty"`
}

// JobEndpoint is one end of a transfer.
type JobEndpoint struct {
	Product    string `json:"product"`
	Name       string `json:"name"`
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Type       string `json:"type,omitempty"`
	// Role is source or target, so the worker knows which half of the
	// product's configuration to resolve the credential from.
	Role string `json:"role"`
}

// LeasedJob is one unit of work.
type LeasedJob struct {
	JobID      string      `json:"jobId"`
	TransferID string      `json:"transferId"`
	Kind       string      `json:"kind"`
	Digest     string      `json:"digest"`
	SizeBytes  Int64String `json:"sizeBytes"`
	MediaType  string      `json:"mediaType,omitempty"`

	Source JobEndpoint `json:"source"`
	Target JobEndpoint `json:"target"`

	// KnownPlacement is the placement fast path, resolved for this batch so
	// the worker makes no extra call to decide it.
	KnownPlacement bool `json:"knownPlacement,omitempty"`
	// MountFromRepository is a repository on the TARGET registry already known
	// to hold this digest, so the worker can ask the registry to relocate it
	// internally instead of streaming it across the network again.
	//
	// This is what stops a bundle's components costing twice their size: they
	// are published both inside the bundle and under their own name, and
	// without this the second copy re-fetched every byte from the vendor.
	// Advisory — a registry that declines the mount just streams.
	MountFromRepository string `json:"mountFromRepository,omitempty"`
	// Tags are what this manifest must be called at the destination, resolved
	// at planning time from the source's own reference annotations. Empty for
	// a blob, and for any manifest the source did not name.
	Tags []string `json:"tags,omitempty"`
	// TargetRepository is the destination path within the target registry.
	// A bundle's components each land in their own, reproduced from the
	// source's structure.
	TargetRepository string `json:"targetRepository,omitempty"`

	Attempt int `json:"attempt"`
	Wave    int `json:"wave"`
}

// LeaseResponse answers a lease request.
type LeaseResponse struct {
	Jobs []LeasedJob `json:"jobs"`
	// LeaseDurationSeconds is how long these are held without renewal.
	LeaseDurationSeconds int `json:"leaseDurationSeconds"`
	// NextPollAfterSeconds is server-directed backoff, so an empty queue with
	// forty workers does not become a poll storm.
	NextPollAfterSeconds int `json:"nextPollAfterSeconds"`
}

// ProgressRequest is POST /api/v1/jobs/{job}:reportProgress.
//
// Lossy by design: dropping one costs nothing. `complete` is not lossy.
type ProgressRequest struct {
	WorkerID         string      `json:"workerId"`
	BytesTransferred Int64String `json:"bytesTransferred"`
}

// CompleteRequest is POST /api/v1/jobs/{job}:complete.
type CompleteRequest struct {
	WorkerID string `json:"workerId"`
	// Outcome is SUCCEEDED, SKIPPED, FAILED or CANCELLED.
	Outcome          JobState    `json:"outcome"`
	BytesTransferred Int64String `json:"bytesTransferred,omitempty"`
	SkipReason       SkipReason  `json:"skipReason,omitempty"`
	ErrorClass       string      `json:"errorClass,omitempty"`
	ErrorMessage     string      `json:"errorMessage,omitempty"`
	DurationMs       int64       `json:"durationMs,omitempty"`
	Attempt          int         `json:"attempt,omitempty"`
	// Placed reports that the destination now holds this content.
	Placed bool `json:"placed,omitempty"`
}

// CompleteResponse tells the worker what its report did.
type CompleteResponse struct {
	// Applied is false when the lease had already expired and the result was
	// discarded. Not an error — the expected outcome of finishing late.
	Applied       bool          `json:"applied"`
	TransferState TransferState `json:"transferState,omitempty"`
	WaveAdvanced  bool          `json:"waveAdvanced,omitempty"`
	CurrentWave   int           `json:"currentWave,omitempty"`
}

// HeartbeatRequest is POST /api/v1/workers/{worker}:heartbeat.
type HeartbeatRequest struct {
	ActiveJobIDs []string    `json:"activeJobIds"`
	CPUPercent   float64     `json:"cpuPercent,omitempty"`
	MemoryBytes  Int64String `json:"memoryBytes,omitempty"`
}

// HeartbeatResponse carries lease renewal and cancellation in one call.
type HeartbeatResponse struct {
	// LeasesRenewed is what the worker still holds. A job MISSING from this
	// list has been lost — reaped and possibly redone elsewhere — and must be
	// abandoned rather than completed.
	LeasesRenewed []string `json:"leasesRenewed"`
	// CancelledJobIDs is how cancellation reaches a worker: there is no push
	// channel, so it rides the heartbeat and takes effect within one interval.
	CancelledJobIDs []string `json:"cancelledJobIds,omitempty"`
	DrainRequested  bool     `json:"drainRequested,omitempty"`
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

// Transfer is one package moving to one destination.
type Transfer struct {
	ID        string `json:"id"`
	RequestID string `json:"requestId"`
	Product   string `json:"product"`
	Tag       string `json:"tag"`
	// DisplayTag is Tag with the vendor's structural noise removed — `25.7.2131`
	// for NEAR's `orb_25.7.2131`. Empty where no shortening applies, which is
	// every source declaring no `vendor`. Cosmetic: Tag is the identity, and
	// both spellings resolve as input.
	DisplayTag string `json:"displayTag,omitempty"`
	Source     string `json:"source"`
	Target     string `json:"target"`

	State    TransferState `json:"state"`
	Priority int           `json:"priority"`

	CurrentWave int `json:"currentWave"`
	MaxWave     int `json:"maxWave"`

	// Progress is always a ROLLUP over jobs, never a maintained counter
	// (invariant I6). A counter would be a second source of truth for the same
	// fact and would drift; this cannot.
	Progress TransferProgress `json:"progress"`

	FailureReason string `json:"failureReason,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
	// StartedAt is when the first job was leased, not when the transfer was
	// asked for. Elapsed time and throughput measured from the request would
	// count however long it waited for a worker as transfer time.
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// TransferProgress is what has happened so far, and what was planned.
type TransferProgress struct {
	JobsPlanned     int `json:"jobsPlanned"`
	JobsDone        int `json:"jobsDone"`
	JobsFailed      int `json:"jobsFailed"`
	JobsOutstanding int `json:"jobsOutstanding"`

	// JobsInFlight is how many are being worked on RIGHT NOW, across Workers
	// distinct workers. Concurrency is otherwise invisible — a page of jobs
	// shows whichever sort to the top, and sixteen-way parallelism looks
	// exactly like one-at-a-time.
	JobsInFlight int `json:"jobsInFlight"`
	Workers      int `json:"workers"`
	// JobsWaiting is how many are sitting out a retry backoff rather than
	// being runnable. "The queue is saturated" and "most of this is waiting"
	// are indistinguishable from a progress count alone.
	JobsWaiting int `json:"jobsWaiting"`

	// JobsBlocked are gated behind a later wave: a manifest cannot be pushed
	// until every blob beneath it has landed (invariant I1), so they are
	// outstanding and deliberately not leasable.
	//
	// It is the number that explains an idle-looking fleet. Five hundred
	// outstanding jobs with one running reads as a broken worker and is
	// usually four hundred and ninety-nine manifests waiting for the last
	// blob of wave 0.
	JobsBlocked int `json:"jobsBlocked"`

	PlannedBytes     Int64String `json:"plannedBytes"`
	BytesTransferred Int64String `json:"bytesTransferred"`
	// DedupeSkippedBytes is what this transfer will NOT move because the
	// destination already had it. Reported rather than buried: it is the
	// number that makes the second transfer of a product line nearly free.
	DedupeSkippedBytes Int64String `json:"dedupeSkippedBytes"`
}

// ListTransfersResponse is GET /api/v1/transfers.
type ListTransfersResponse struct {
	Transfers     []Transfer `json:"transfers"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

// Job is layer-level progress.
type Job struct {
	ID         string      `json:"id"`
	Kind       string      `json:"kind"`
	Digest     string      `json:"digest"`
	SizeBytes  Int64String `json:"sizeBytes"`
	State      JobState    `json:"state"`
	SkipReason SkipReason  `json:"skipReason,omitempty"`
	Wave       int         `json:"wave"`

	Attempts         int         `json:"attempts"`
	MaxAttempts      int         `json:"maxAttempts"`
	BytesTransferred Int64String `json:"bytesTransferred"`
	LeaseOwner       string      `json:"leaseOwner,omitempty"`
	LastError        string      `json:"lastError,omitempty"`
	LastErrorClass   string      `json:"lastErrorClass,omitempty"`

	// Where this job reads from and writes to.
	SourceRepository string   `json:"sourceRepository,omitempty"`
	TargetRepository string   `json:"targetRepository,omitempty"`
	TargetTags       []string `json:"targetTags,omitempty"`

	// Parent is the artifact this job belongs to — what makes a digest
	// legible. A blob on its own is not something anybody can recognise; the
	// image or chart that references it is.
	Parent *JobParent `json:"parent,omitempty"`
}

// JobParent identifies the artifact a job belongs to.
type JobParent struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType,omitempty"`
	// Ref is the vendor's own name for it, from
	// org.opencontainers.image.ref.name — `orbs/CFX-5000-k8s/nginx:1.2.3`.
	Ref string `json:"ref,omitempty"`
	// Shared reports that several artifacts reference this blob, so the
	// attribution is an example rather than the whole truth. A base layer
	// shared by five images belongs to all of them.
	Shared bool `json:"shared,omitempty"`
}

// ListJobsResponse is GET /api/v1/transfers/{transfer}/jobs.
type ListJobsResponse struct {
	TransferID string `json:"transferId"`
	Jobs       []Job  `json:"jobs"`
}

// CreateTransferRequest is POST /api/v1/transfers.
//
// One request, one origin, several destinations. The OPERATION is not a field:
// it is derived from what `from` resolves to — a configured source means
// replicate, a configured target means promote — because a field that can
// disagree with `from` is a field that eventually will.
type CreateTransferRequest struct {
	Product string `json:"product"`
	// Package is a tag or a digest.
	Package string `json:"package"`

	// From names the origin. Empty means the repository the package was
	// discovered in, or — when Promote is set — the product's promotion
	// source environment.
	From string `json:"from,omitempty"`
	// To names destinations explicitly.
	To []string `json:"to,omitempty"`
	// ToEnvironment fans out to every target in one environment: the
	// deliberate form of "all of them", said rather than inferred.
	ToEnvironment string `json:"toEnvironment,omitempty"`

	// Promote requires the origin to be a target and resolves omitted ends
	// through the product's promotion path.
	Promote bool `json:"promote,omitempty"`

	Priority int `json:"priority,omitempty"`
	// ValidateOnly checks and resolves everything, writes nothing, and returns
	// the plan (AIP-163). This is how dry run works.
	ValidateOnly bool `json:"validateOnly,omitempty"`
}

// TransferEndpoint is one resolved end of a request.
type TransferEndpoint struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Environment string `json:"environment,omitempty"`
	Registry    string `json:"registry"`
	Repository  string `json:"repository,omitempty"`
}

// CreateTransferResponse reports what a request produced.
type CreateTransferResponse struct {
	RequestID string `json:"requestId,omitempty"`
	// Created is false when an identical request already existed — a replay,
	// not an error.
	Created bool `json:"created"`

	// Operation is REPLICATE or PROMOTE, as derived.
	Operation string             `json:"operation"`
	From      TransferEndpoint   `json:"from"`
	To        []TransferEndpoint `json:"to"`

	// TransferIDs are the transfers opened, one per destination, in the same
	// order as `to`. Empty on a dry run.
	TransferIDs []string `json:"transferIds,omitempty"`
}
