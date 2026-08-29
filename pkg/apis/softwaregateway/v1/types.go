// Package v1 is the public API surface of softwareGateway.
//
// This is the ONLY package outside internal/. It is what transferctl uses and
// what a third-party integration would import - a compile-time commitment to
// the contract in docs/design/09-api.md, rather than a convention.
//
// Conventions (docs/design/09-api.md section 1):
//   - lowerCamelCase JSON field names (AIP-140)
//   - SCREAMING_SNAKE_CASE enum values (AIP-126)
//   - int64 serialized as STRING (AIP-141) - see the note on Int64String
//   - RFC 3339 UTC timestamps
package v1

import "encoding/json"

// APIVersion is the served API version.
const APIVersion = "v1"

// Int64String carries a 64-bit quantity over JSON as a string.
//
// JSON numbers are IEEE-754 doubles and lose precision above 2^53. Byte counts
// here already reach 10^11, so a plain int64 would be silently rounded - rare
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
// This is the DEEP check - it validates connectivity to every configured
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
	// loaded, validated and listed - it simply does nothing.
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
	// Environment is the stage a TARGET represents: `lab`, `production`.
	// Empty for a source, and for a target that declares none.
	//
	// On the wire because it is the only thing that distinguishes production
	// from anywhere else, and a client that cannot tell them apart cannot say
	// whether a release has shipped.
	Environment string `json:"environment,omitempty"`
	// Vendor is the publishing convention a SOURCE follows - `near`, or empty
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

// Concurrency is the RESOLVED limit in force for one registry - the product's
// override if it has one, otherwise the application-level default. Never the
// raw document value, so a client reading this sees what is actually happening
// rather than what was written down.
//
// Fleet-wide, not per-worker: a per-worker limit would silently multiply by the
// replica count and flatten a vendor registry the moment HPA scaled out.
type Concurrency struct {
	// PerRegistry is requests in flight against this registry, which is also the
	// connection pool size - they are the same limit.
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
	TransferPending  TransferState = "PENDING"
	TransferPlanning TransferState = "PLANNING"
	TransferReady    TransferState = "READY"
	TransferRunning  TransferState = "RUNNING"
	TransferPaused   TransferState = "PAUSED"
	// TransferSyncing is a delegated transfer waiting on the registry. It has
	// no progress and never will.
	TransferSyncing TransferState = "SYNCING"
	// TransferPromoting is a native promotion waiting on the registry. It has
	// no BYTE progress and never will - what it moves is names, and Promotion
	// below is where that count lives.
	TransferPromoting TransferState = "PROMOTING"
	TransferVerifying TransferState = "VERIFYING"
	TransferSucceeded TransferState = "SUCCEEDED"
	// TransferDiverged is terminal and is neither success nor failure: the
	// sync completed and the destination holds a different digest than the one
	// requested, because the upstream tag moved.
	TransferDiverged   TransferState = "DIVERGED"
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
	// JobSkipped means the content was already present or was mounted - a
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
// Identity is (source repository, tag, manifest digest) - the digest is part of
// it, which is why a re-pushed tag produces a second Package rather than
// mutating the first (docs/design/01 §2.2).
type Package struct {
	// Name is the AIP-122 resource name: "products/{product}/packages/{package}".
	Name      string `json:"name"`
	PackageID string `json:"packageId"`
	Product   string `json:"product"`

	// Security is what a vulnerability sync recorded for this release, or nil
	// where none has run. Nil is NOT zero vulnerabilities, and a client that
	// renders it as such has written the bug the whole feature exists to
	// prevent.
	Security *PackageSecuritySummary `json:"security,omitempty"`

	Tag            string `json:"tag"`
	ManifestDigest string `json:"manifestDigest"`
	MediaType      string `json:"mediaType"`

	// TotalBytes counts each distinct digest ONCE. A fat index whose platforms
	// share a base layer transfers that layer once, so summing naively would
	// overstate the cost - sometimes several-fold.
	//
	// OMITTED when not yet measured, which is the case for a package whose root
	// is an index: discovery records what the index lists without fetching it,
	// so the layer bytes underneath are unknown until a transfer walks the
	// tree. Absent rather than zero - a wrong size is worse than a missing one,
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
	// - and omitted entirely when the publisher set none, which the OCI spec
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

	// AccessoryOf names the package this row is PART OF - a signature or a
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
	// removed - `cfx-5000-k8s` for NEAR's `orbs/cfx-5000-k8s`.
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
	// AnalysisState is "analyzing" while a walk is in flight and "failed" when
	// the last one gave up. Empty otherwise - `expandedAt` is what says a walk
	// succeeded.
	//
	// Three states because `expandedAt` has two, and a release being walked
	// right now must not read as one nobody has touched: that is what offered
	// "Analyze package" on a release the system was already analysing.
	AnalysisState string `json:"analysisState,omitempty"`
	// AnalysisError is why the last walk gave up. "It failed" that cannot say
	// why is a dead end for whoever reads it a week later.
	AnalysisError string `json:"analysisError,omitempty"`

	// SignatureStatus is SIGNED, UNSIGNED or UNKNOWN.
	//
	// Three values, and the third is the one that matters: "we looked and found
	// none" and "nobody looked" are the same value in a boolean and completely
	// different facts when the question is whether to trust something. UNKNOWN
	// is what a source whose layout does not attempt signature discovery
	// reports - honestly, rather than claiming a confident UNSIGNED.
	SignatureStatus SignatureStatus `json:"signatureStatus,omitempty"`

	// Related are artifacts that belong to this package without living inside
	// its manifest tree - its signature, an SBOM, an attestation, or the
	// wrapper that bundles them.
	Related []RelatedArtifact `json:"related,omitempty"`

	// Transfers is what has been attempted with this package, per destination.
	//
	// On the SINGLE-package read only, for the reason Related is: a page of
	// fifty packages would be fifty extra queries to render a column.
	//
	// It is where a package's transfer HISTORY lives, and deliberately not a
	// state on the package itself. "Transferred" is a fact about a package and
	// ONE target - with four targets there are four answers, and a single
	// column could hold at most one of them and would be wrong the moment a
	// fifth target was configured.
	Transfers []PackageTransfer `json:"transfers,omitempty"`

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
// found it - the referrers API, a cosign tag, a wrapper index - is an
// implementation detail of the source's layout and deliberately not on the
// wire, so a vendor changing convention changes no client.
type RelatedArtifact struct {
	Role      string      `json:"role"`
	Digest    string      `json:"digest"`
	Tag       string      `json:"tag,omitempty"`
	MediaType string      `json:"mediaType,omitempty"`
	SizeBytes Int64String `json:"sizeBytes,omitempty"`

	// The MATERIAL, for a signature: Digest above names the manifest that
	// carries it, and these name what is inside - the blob a verifier reads.
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
	// inspected this package - which is a different fact from a signature that
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

	Digest       string `json:"digest"`
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType,omitempty"`
	// SizeBytes is what the referencing descriptor says this MANIFEST weighs -
	// a few kilobytes of JSON. It is the right number for planning a manifest
	// push and the wrong one for "how big is this image".
	SizeBytes Int64String `json:"sizeBytes"`
	// ContentBytes is what the artifact weighs: manifest, config and layers.
	//
	// Omitted for an artifact nobody has walked, because until then its blobs
	// are unknown - which is not the same as its weighing nothing, and Fetched
	// is what tells the two apart. Summing SizeBytes instead reported a
	// nine-hundred-megabyte image as two kilobytes.
	ContentBytes Int64String `json:"contentBytes,omitempty"`
	// Kind is what this artifact IS, in the words somebody uses: index, image,
	// chart, file, signature, artifact.
	//
	// Derived rather than stored, from the OCI fields and - where the source
	// declares a vendor layout - from the vendor's own annotations. Both are
	// needed: an index's children are recorded from what the index LISTED
	// without fetching each one, so the config media type that normally
	// separates a Helm chart from an image is not available, and a vendor whose
	// parts are all `image.manifest.v1+json` would otherwise report every chart
	// as an image.
	//
	// A client groups on this and needs to know nothing about any vendor.
	Kind string `json:"kind,omitempty"`
	// Platform is "linux/amd64". Empty for non-image artifacts such as Helm
	// charts and configuration bundles.
	Platform string `json:"platform,omitempty"`
	// Depth is 0 for the root; an index's children are 1.
	Depth int `json:"depth"`
	// Annotations is the artifact's annotation map, verbatim.
	//
	// Kept whole so a vendor's own keys - com.nokia.ncd.orb.type, say - reach
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
	// sweeper. Nothing is lost - the artifact, its blobs and its size are all
	// still recorded - but pushing it would re-read it from the source. See
	// store.SweepManifestCache.
	Cached bool `json:"cached"`
}

// InspectPackageRequest asks for a release to be walked.
//
// Wait decides whether the CALLER waits. Walking a real release is hundreds of
// round trips against a vendor registry and minutes of them, and a caller that
// waits owns a problem: navigating away, or any intermediary's idle timeout,
// cancels the request - and with it the walk, leaving the release claimed by
// nobody and marked as being analysed.
//
// So an interface asks with `wait: false`: the walk is handed to the same
// background analyser discovery uses, the release is marked as being analysed,
// and the page watches that state. A script that wants the numbers back in the
// response omits it, which is the default and the old behaviour.
type InspectPackageRequest struct {
	// Wait holds the request open until the walk finishes. Absent means true.
	Wait *bool `json:"wait,omitempty"`
}

// InspectPackageResponse reports what expanding a package found.
//
// Discovery stops at the tag's own manifest, so a package's transfer size is
// unknown until something walks the tree. This is that something - and it is
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
	// The bodies are an evictable cache with a budget, not part of the record -
	// what a package IS gets kept forever, what it was SERVED AS gets reclaimed
	// when the cache fills. Reported so the difference is visible rather than
	// discovered when a transfer takes longer than expected. Neither number
	// affects any of the ones above.
	CachedManifests int         `json:"cachedManifests"`
	CachedBytes     Int64String `json:"cachedBytes,omitempty"`
	// SignatureResolved is how many signature relations had their material
	// recorded - the blob a verifier reads, captured while the tree was in hand.
	SignatureResolved int `json:"signatureResolved,omitempty"`

	// Started reports that the walk was HANDED OFF rather than performed, so
	// every count above is zero because nothing has been counted yet. The
	// package's analysisState is what to watch.
	Started bool `json:"started,omitempty"`
}

// CancelAnalysisResponse is POST
// /api/v1/products/{product}/packages/{package}:cancelAnalysis.
//
// Two booleans rather than one, because they are two different promises and a
// caller must be able to tell which it was given. See the handler for why the
// claim and the walking are separable at all.
type CancelAnalysisResponse struct {
	Product string `json:"product"`
	Package string `json:"package"`

	// Stopped means a claim was released - the release is no longer marked as
	// being analysed, and can be analysed again now.
	//
	// False is not a failure: the walk finished between the reader deciding to
	// stop it and the request arriving.
	Stopped bool `json:"stopped"`

	// StoppedHere means the walking itself has been cancelled, because the
	// Coordinator that answered this request is the one that was doing it.
	//
	// False with Stopped true means the walk is running on another replica: it
	// will carry on reading the vendor's registry until its own deadline, and
	// its result will be discarded because it no longer holds the claim.
	StoppedHere bool `json:"stoppedHere"`

	// Package_ is the release as it stands now, so a caller need not re-read it
	// to find out what state the stop left behind.
	Package_ Package `json:"packageState"`
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
// Returned synchronously by default because a scan is usually bounded work -
// one HEAD per tag - and an operator triggering it after a vendor announcement
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
	// vendor - gaining their signature status, their related artifacts and their
	// transfer root. Same shape as Renamed: zero always, except on the one scan
	// that follows the edit.
	Regrouped  int   `json:"regrouped,omitempty"`
	DurationMs int64 `json:"durationMs"`
	// TagErrors are per-tag failures that did not stop the scan.
	//
	// Structured rather than pre-rendered because they are not all the same
	// KIND of thing: a source refusing content the customer has not licensed is
	// a fact about entitlement, and a timeout is a fault. Handing the client one
	// string per failure forced it to decide which by reading English.
	TagErrors []ScanIssue `json:"tagErrors,omitempty"`
	// RepositoryErrors are per-repository failures that did not stop the scan.
	RepositoryErrors []string `json:"repositoryErrors,omitempty"`
	// Vocabulary is what this source's vendor calls a repository and a tag, so
	// a summary can be read without translating every line.
	Vocabulary *ScanVocabulary `json:"vocabulary,omitempty"`

	// Collapsed reports that a scan was ALREADY RUNNING when this request
	// arrived, so these numbers come from that scan rather than one this call
	// started. The data is real either way - the request waited for it - but the
	// two are different facts and the caller is told which one it got.
	Collapsed bool `json:"collapsed,omitempty"`

	// Started is set instead of the counters when the request asked not to
	// wait: the scan is running, and there are no results yet to report.
	Started *DiscoverStarted `json:"started,omitempty"`
}

// ScanIssue is one thing a scan could not read.
type ScanIssue struct {
	Repository string `json:"repository,omitempty"`
	Tag        string `json:"tag,omitempty"`
	// DisplayRepository and DisplayTag are the VENDOR's names for the same two
	// things - `cfx-5000-k8s` and `24.7.1186` where the paths are
	// `orbs/cfx-5000-k8s` and `orb_24.7.1186`.
	DisplayRepository string `json:"displayRepository,omitempty"`
	DisplayTag        string `json:"displayTag,omitempty"`
	// Class is what kind of issue this is, where the kind changes what should
	// be done about it. `not_entitled` means the source refused content this
	// customer has not licensed, which is the entitlement check working rather
	// than anything to fix. Empty is an ordinary failure.
	Class string `json:"class,omitempty"`
	// Message is the failure verbatim, including whatever the registry said.
	Message string `json:"message,omitempty"`
}

// ScanVocabulary is what a vendor's users call the things a scan counts.
type ScanVocabulary struct {
	Unit     string `json:"unit,omitempty"`
	Units    string `json:"units,omitempty"`
	Version  string `json:"version,omitempty"`
	Versions string `json:"versions,omitempty"`
}

// CompareRequest is POST /api/v1/products/{product}/packages/{package}:compare.
//
// The package in the path is the FIRST end. Everything here names the second,
// and every field is optional: the common case - "did this land at my default
// destination?" - is an empty body.
type CompareRequest struct {
	// From and To are configured source or target names. An empty From means
	// the repository the package was discovered in.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Against is a second package reference - a tag or a digest - making this a
	// comparison of two VERSIONS rather than of two places. Combined with a
	// From and To that name the same endpoint, it answers "what changed in this
	// release"; combined with two different ones, it answers both at once.
	Against string `json:"against,omitempty"`
	// FileBudgetBytes is accepted and ignored.
	//
	// It bounded how much layer CONTENT a comparison could download to say
	// which files changed rather than which layers. Nothing is downloaded any
	// more - an artifact's manifest already names its files and states their
	// digests - so there is no cost to bound. Kept on the wire so an older
	// client's request is still a valid request.
	FileBudgetBytes int64 `json:"fileBudgetBytes,omitempty"`
	// ProgressToken is a caller-minted id it can poll for progress while this
	// request is still open, at GET /api/v1/comparisons/{token}.
	//
	// The caller mints it because the report is the response: there is nothing
	// to hand back an id in. Omitting it costs nothing and reports nothing.
	ProgressToken string `json:"progressToken,omitempty"`
}

// PackageFile is one named file inside a release.
type PackageFile struct {
	// Path is the publisher's own name for it - `CONFIGURATION/nodes.json`.
	Path string `json:"path"`
	// Component is the artifact it came from, by the name the release gives
	// that artifact.
	Component string      `json:"component,omitempty"`
	SizeBytes Int64String `json:"sizeBytes"`
	Digest    string      `json:"digest"`
	MediaType string      `json:"mediaType,omitempty"`
}

// ListPackageFilesResponse is
// GET /api/v1/products/{product}/packages/{package}/files.
//
// What is INSIDE a release, as files rather than as layers.
type ListPackageFilesResponse struct {
	Files []PackageFile `json:"files"`
	// OpaqueLayers is how many layers carry no name of their own - image
	// layers, which are archives of an unknown number of paths. They are not
	// listed, and saying how many there are is what stops the list reading as
	// the whole of a release's content.
	OpaqueLayers int `json:"opaqueLayers,omitempty"`
	// Analysed reports whether this release has been walked. Before that, the
	// only thing known is what its index listed, and an empty file list means
	// "nobody has looked" rather than "there are none".
	Analysed bool `json:"analysed"`
}

// PackageFileContentResponse is
// GET /api/v1/products/{product}/packages/{package}/files/content?digest=…
//
// One file, read out of the registry that publishes it.
//
// The digest names WHICH file, and it is a lookup key rather than an address:
// the server serves it only if it is already recorded as a named layer of this
// release. A handler that fetched whatever digest a caller asked for would be a
// request forgery with the vendor credential attached.
type PackageFileContentResponse struct {
	Path      string      `json:"path"`
	Component string      `json:"component,omitempty"`
	Digest    string      `json:"digest"`
	MediaType string      `json:"mediaType,omitempty"`
	SizeBytes Int64String `json:"sizeBytes"`
	// Content is the file, as text. Empty when Binary or TooLarge is set.
	Content string `json:"content,omitempty"`
	// Binary says the bytes are not text, so there is nothing to show. Stated
	// rather than rendered as mojibake: a reader who asked to look at a file
	// deserves to be told what it is instead of a screen of replacement
	// characters. Use the download endpoint for the actual bytes.
	Binary bool `json:"binary,omitempty"`
	// TooLarge says the file is past what this endpoint will read into memory,
	// and Limit is that bound. A view is for looking at configuration, not for
	// downloading a release through the API.
	TooLarge bool  `json:"tooLarge,omitempty"`
	Limit    int64 `json:"limit,omitempty"`
}

// CompareProgressSide is one end's position in a comparison.
type CompareProgressSide struct {
	// Key is which end this is - "a" or "b". The label is not an identity: the
	// two sides of a version comparison are the same place and share it.
	Key   string `json:"key"`
	Side  string `json:"side"`
	Phase string `json:"phase"`
	Done  int    `json:"done"`
	// Total is what is KNOWN so far. A manifest tree is discovered by walking
	// it, so during that phase the denominator grows and Estimated is true.
	Total     int  `json:"total"`
	Estimated bool `json:"estimated,omitempty"`
	// Concurrency is how many requests this side may have in flight at once.
	// "Is it going as fast as it can" is the second question anybody watching
	// a four-minute bar asks.
	Concurrency int `json:"concurrency,omitempty"`
}

// CompareProgressResponse is GET /api/v1/comparisons/{comparison}.
//
// The side channel a comparison reports through while its own request is open.
// A 404 is a normal answer: progress lives in the memory of the replica running
// the comparison and is dropped shortly after it finishes.
type CompareProgressResponse struct {
	Sides     []CompareProgressSide `json:"sides"`
	Done      bool                  `json:"done"`
	StartedAt string                `json:"startedAt,omitempty"`
	UpdatedAt string                `json:"updatedAt,omitempty"`
}

// CompareResponse is what two places hold, aligned component by component.
type CompareResponse struct {
	Product string `json:"product"`

	A CompareEnd `json:"a"`
	B CompareEnd `json:"b"`

	Rows []CompareRow `json:"rows"`

	// Same, Changed, OnlyA and OnlyB partition Rows.
	Same    int `json:"same"`
	Changed int `json:"changed"`
	OnlyA   int `json:"onlyA"`
	OnlyB   int `json:"onlyB"`

	// ExtraTagsA and ExtraTagsB are tags in each side's bundle repository that
	// the bundle does not account for - content nobody in this comparison put
	// there.
	ExtraTagsA []string `json:"extraTagsA,omitempty"`
	ExtraTagsB []string `json:"extraTagsB,omitempty"`
	// ExtraTruncatedA and ExtraTruncatedB say the repository listed more tags
	// than the comparison would resolve, so the lists above are a partial
	// account of what is unexplained rather than the whole one.
	ExtraTruncatedA bool `json:"extraTruncatedA,omitempty"`
	ExtraTruncatedB bool `json:"extraTruncatedB,omitempty"`
}

// CompareEnd identifies one side of a comparison.
type CompareEnd struct {
	// Label is the configured endpoint, plus the version where the two sides
	// differ in version.
	Label string `json:"label"`
	// Reference is what was actually walked, as a pullable reference.
	Reference string `json:"reference"`
}

// CompareRow is one component, on both sides.
type CompareRow struct {
	// Type is what the component is: index, image, chart, file, signature.
	Type string `json:"type"`
	// Name is the vendor's name for it, from org.opencontainers.image.ref.name.
	Name string `json:"name"`
	// Verdict is same | changed | only-a | only-b.
	Verdict string       `json:"verdict"`
	A       *CompareSide `json:"a,omitempty"`
	B       *CompareSide `json:"b,omitempty"`
	// Differences states each disagreement as a fact. Empty when the two sides
	// agree.
	Differences []string `json:"differences,omitempty"`
	// Files is the account of the NAMED FILES inside this component - the
	// answer to "which configuration changed", which "two layers changed"
	// cannot give.
	//
	// Read from the manifests, not from the archives: an OCI artifact names one
	// file per layer and states its content digest, so aligning two of those
	// lists by path answers it exactly and costs nothing.
	//
	// Every file of a component that differs, unchanged ones included. Empty
	// for a component that agrees, where nothing inside it can differ.
	Files []CompareFile `json:"files,omitempty"`
}

// CompareFile is one named file inside a component, and what became of it.
//
// Both digests and both sizes, because "changed" prompts the next question: a
// reader looking at a changed file wants what it was and what it is.
type CompareFile struct {
	Path string `json:"path"`
	// Verdict is same | changed | only-a | only-b - the same vocabulary the
	// component rows use.
	Verdict string      `json:"verdict"`
	SizeA   Int64String `json:"sizeA,omitempty"`
	SizeB   Int64String `json:"sizeB,omitempty"`
	DigestA string      `json:"digestA,omitempty"`
	DigestB string      `json:"digestB,omitempty"`
}

// CompareSide is one end's account of one component.
type CompareSide struct {
	Digest     string      `json:"digest"`
	Tag        string      `json:"tag,omitempty"`
	Size       Int64String `json:"size,omitempty"`
	Repository string      `json:"repository,omitempty"`
	// NamedRepository is where this component should be pullable AS ITSELF on
	// this side, and NamedPresent whether it is. The site a consumer uses, and
	// the one that silently fails to appear.
	NamedRepository string `json:"namedRepository,omitempty"`
	NamedPresent    bool   `json:"namedPresent,omitempty"`
	// NamedTagDigest is what the component's own tag resolves to there, empty
	// when it resolves to nothing.
	NamedTagDigest string `json:"namedTagDigest,omitempty"`
}

// ListUnavailableResponse is GET /api/v1/products/{product}/unavailable.
type ListUnavailableResponse struct {
	Packages []UnavailablePackage `json:"packages"`
}

// UnavailablePackage is content a source would not serve.
//
// Kept and reported rather than raised as an error on every scan: a vendor
// registry serves a catalogue spanning every customer, and refusing the
// products this one has not bought is correct behaviour that recurs forever.
type UnavailablePackage struct {
	Repository        string `json:"repository"`
	Tag               string `json:"tag"`
	DisplayRepository string `json:"displayRepository,omitempty"`
	DisplayTag        string `json:"displayTag,omitempty"`
	Reason            string `json:"reason"`
	// Detail is what the registry itself said - the sentence naming the
	// customer and the product, which is what somebody takes to their account
	// manager.
	Detail      string `json:"detail,omitempty"`
	FirstSeenAt string `json:"firstSeenAt,omitempty"`
	LastSeenAt  string `json:"lastSeenAt,omitempty"`
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
	CurrentTag           string `json:"currentTag,omitempty"`

	TagsTotal    int `json:"tagsTotal,omitempty"`
	TagsResolved int `json:"tagsResolved,omitempty"`
	// TagsChecked is how many tags have been resolved to a digest - one HEAD
	// each, and the bulk of a scan. It moves continuously; TagsResolved does
	// not, and a bar built on the wrong one sits still through the longest
	// part of every scan.
	TagsChecked int `json:"tagsChecked,omitempty"`
	// TagsToFetch is how many turned out to be new, and TagsFetched how many of
	// those have been read. The second phase's real denominator, known only
	// once the first has decided it.
	TagsToFetch int `json:"tagsToFetch,omitempty"`
	TagsFetched int `json:"tagsFetched,omitempty"`
	// TagsInFlight is how many tags are being read RIGHT NOW - the configured
	// concurrency actually being used, and when it sits at one, that it is not.
	TagsInFlight int `json:"tagsInFlight,omitempty"`

	// Progress is the whole scan's progress, from 0 to 1.
	//
	// ONE number for the whole scan, not one per phase. The counters above are
	// in three different units and a caller that drew a bar from whichever one
	// was live would draw a bar that filled, reset and filled again - which is
	// what this replaced. The server puts the phases on one scale and keeps the
	// result monotonic, so the bar only ever moves forward.
	//
	// The phase name is served alongside and says what is happening; this says
	// how much of it is left.
	Progress float64 `json:"progress,omitempty"`

	// Artifacts is manifests fetched so far. A single tag with a large artifact
	// tree takes minutes, during which this is the only counter that moves.
	Artifacts int `json:"artifacts,omitempty"`
	// Packages is releases recorded by this scan so far, and NewPackages the
	// subset nobody had seen. Both counted as they are written rather than at
	// the end - "is it finding anything?" is asked while it is still looking.
	Packages    int `json:"packages,omitempty"`
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
// question - "is my configuration correct?" - makes real calls to third
// parties, and is run on demand.
type CheckConnectivityResponse struct {
	Status   CheckStatus    `json:"status"`
	Products []ProductCheck `json:"products"`
}

// ContentGroup is one kind of component and how the transfer's components of
// that kind went.
//
// # Counted per COMPONENT
//
// A component published under two names is one component, not two, so the
// counts here are artifacts rather than jobs.
//
// # The bytes are per JOB, and they are a different question
//
// There is deliberately no "how big is this kind" here: a base layer shared by
// four images belongs to all four, so any such total either counts it four
// times or picks an owner. The transfer's own byte totals answer that.
//
// SavedBytes and CopiedBytes answer something else - which JOBS were skipped
// and which ran. A blob is one job however many components reference it, so
// these partition the transfer's bytes exactly: every byte counted once, and
// the parts add up to the whole. The only softness is which component a shared
// blob is filed under, and within a kind that is nearly always the same answer.
type ContentGroup struct {
	// Kind is what these are, in the words somebody uses: index, image, chart,
	// file, signature, artifact.
	Kind  string `json:"kind"`
	Total int    `json:"total"`
	// Copied is what this transfer actually pushed. Present is what the
	// destination already held - the pair that makes a delta transfer legible.
	Copied  int `json:"copied"`
	Present int `json:"present"`
	// Failed is components with a job that has given up; Outstanding is
	// components with work still to do.
	Failed      int `json:"failed,omitempty"`
	Outstanding int `json:"outstanding,omitempty"`

	// SavedBytes is what this transfer did not have to move for this kind, and
	// CopiedBytes what it did. See the type comment: these are per JOB and they
	// add up to the transfer's own totals.
	SavedBytes  Int64String `json:"savedBytes,omitempty"`
	CopiedBytes Int64String `json:"copiedBytes,omitempty"`

	// Units is the number of separate pushes beneath these components - every
	// layer, config and manifest that is a job of its own - and the four
	// counts under it are how those pushes went.
	//
	// # Why the counts above are not enough
	//
	// A component is `copied` only when its last layer and its manifest have
	// both landed: half an image is not an image, and reporting one as copied
	// because most of it arrived would be a lie about what the destination
	// holds. That makes the component counts the right answer to "what is
	// there" and a hopeless answer to "how far along is this" - they sit at
	// zero for the whole download and then all move at once, while tens of
	// thousands of layers are visibly streaming underneath.
	//
	// These are that second answer. UnitsCopied + UnitsPresent over Units is
	// how much of this kind the destination now holds, and it moves with every
	// layer rather than with every finished component.
	Units            int `json:"units,omitempty"`
	UnitsCopied      int `json:"unitsCopied,omitempty"`
	UnitsPresent     int `json:"unitsPresent,omitempty"`
	UnitsFailed      int `json:"unitsFailed,omitempty"`
	UnitsOutstanding int `json:"unitsOutstanding,omitempty"`

	// Files is how many FILES this kind holds, where that is a different
	// number from Total - which is to say, on the `file` kind and nowhere
	// else.
	//
	// A vendor ships its configuration as one `generic` component carrying a
	// hundred and twelve named layers. Two such bundles are two components and
	// a hundred and twelve files, and a release page that has been counting
	// files as files since it learnt to list them reported `Files 112` beside a
	// download page reporting `Files 2`. Both were true of different
	// populations, and neither said so.
	//
	// Counted the same way the file listing counts: a layer the publisher gave
	// a name. An unnamed layer is a tar of an unknown number of paths and is
	// not a file anybody can point at, so it is not one here either.
	Files int `json:"files,omitempty"`
}

// PackageTransfer is one attempt to move a package to one destination.
type PackageTransfer struct {
	ID     string        `json:"id"`
	Target string        `json:"target"`
	State  TransferState `json:"state"`
	// Operation is REPLICATE (downloaded from a vendor) or PROMOTE (moved
	// between two of our targets).
	//
	// It is what stops a release's history reading as one long download. A
	// promotion is a different event in a release's life, reached by a
	// different decision, and folding the two together made a promoted release
	// look like one that had been downloaded twice.
	Operation string `json:"operation,omitempty"`
	// FailureReason is why it failed, verbatim, INCLUDING the digest of
	// whatever the source would not serve.
	//
	// The reason a vendor refuses one component of a release is the sentence
	// that names the customer and the sales item, and the digest is what turns
	// "an entitlement is missing" into "this component is the one". Both are
	// wanted weeks later, which is why this is read from the transfer that
	// recorded it rather than summarised into a flag.
	FailureReason string `json:"failureReason,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
	CompletedAt   string `json:"completedAt,omitempty"`
}

// TransferWave is one wave's population, by state.
//
// Present on a single transfer, absent from a listing: forty transfers times
// four waves is a table nobody reads, and the question it answers is always
// asked about one transfer.
//
// It exists because the outstanding count mixes three populations that behave
// completely differently - runnable, waiting out a backoff, and GATED behind a
// wave that has not drained - and only the last one explains an idle fleet.
type TransferWave struct {
	Wave int `json:"wave"`
	// Kind is blob, manifest, or mixed. Blobs are wave 0 and manifests are the
	// rest; seeing that stated is half of understanding the ordering.
	Kind string `json:"kind"`
	// Current marks the wave the transfer is working on.
	Current bool `json:"current,omitempty"`

	Total   int `json:"total"`
	Done    int `json:"done"`
	Running int `json:"running"`
	// Pending is leasable NOW. Waiting is pending behind a retry backoff - the
	// same state column, an entirely different situation.
	Pending int `json:"pending"`
	Waiting int `json:"waiting"`
	Blocked int `json:"blocked"`
	Failed  int `json:"failed"`

	PlannedBytes     Int64String `json:"plannedBytes"`
	TransferredBytes Int64String `json:"transferredBytes"`
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// Worker is one member of the fleet, as the Coordinator last heard from it.
//
// Every field here already crossed the wire on a lease or a heartbeat and was
// read for one decision and dropped. Recording it is what lets `health` answer
// "are the workers up, and what are they doing" - a question that previously
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
	// rather than announced - a worker that was killed never got to say so.
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
	// Reblocked is how many jobs failed only because something they depend on
	// failed. Those go back to WAITING rather than to running - their
	// dependency is being requeued, not satisfied - and they are counted apart
	// because "forty jobs requeued" when thirty-eight were consequences of two
	// overstates what actually broke.
	Reblocked int    `json:"reblocked,omitempty"`
	State     string `json:"state,omitempty"`
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
	// Knee is the smallest concurrency within a tenth of the best measured -
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
// everything needed to execute a job travels in the lease response - except
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
	// RepairLevel is how much of the fast-path ladder this job may not use.
	// 1 distrusts the placement record and the HEAD but still tries the mount;
	// 2 streams unconditionally. Set when a manifest push has already been
	// rejected for this content, so the destination's answers are no longer
	// evidence.
	RepairLevel int `json:"repairLevel,omitempty"`
	// MountFromRepository is a repository on the TARGET registry already known
	// to hold this digest, so the worker can ask the registry to relocate it
	// internally instead of streaming it across the network again.
	//
	// This is what stops a bundle's components costing twice their size: they
	// are published both inside the bundle and under their own name, and
	// without this the second copy re-fetched every byte from the vendor.
	// Advisory - a registry that declines the mount just streams.
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
	// discarded. Not an error - the expected outcome of finishing late.
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
	// list has been lost - reaped and possibly redone elsewhere - and must be
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
	ID          string `json:"id"`
	RequestID   string `json:"requestId"`
	Product     string `json:"product"`
	PackageName string `json:"packageName,omitempty"`
	// PackageID is WHICH package this moves, and it is the only unambiguous
	// answer.
	//
	// A vendor's tag is not unique within a product: one NEAR release appears
	// under the same tag in every repository the product watches, which for a
	// real product is ten of them. A consumer joining a transfer listing to a
	// package listing on (product, tag) therefore lights up ten packages for
	// one download - which is exactly what the software page did, reporting
	// twenty releases as DOWNLOADING when two were.
	PackageID string `json:"packageId,omitempty"`
	Tag       string `json:"tag"`
	// DisplayTag is Tag with the vendor's structural noise removed - `25.7.2131`
	// for NEAR's `orb_25.7.2131`. Empty where no shortening applies, which is
	// every source declaring no `vendor`. Cosmetic: Tag is the identity, and
	// both spellings resolve as input.
	DisplayTag string `json:"displayTag,omitempty"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	// SourceName and TargetName are the configured names an operator types into
	// --from and --to. Source and Target are the resolved host and path: right
	// for one transfer, far too wide for a page of them.
	SourceName string `json:"sourceName,omitempty"`
	TargetName string `json:"targetName,omitempty"`

	// Operation is REPLICATE or PROMOTE - what was ASKED for, from the
	// request. Strategy below is how it was carried out, which is a different
	// question: a promotion can be a copy, and a download never is one.
	Operation string `json:"operation,omitempty"`

	State    TransferState `json:"state"`
	Priority int           `json:"priority"`

	// Strategy is HOW this transfer was performed: `copy` (our workers moved
	// the bytes), `mirror` or `proxy` (the registry did).
	//
	// It is the field every byte column has to be read against. For anything
	// but `copy` the progress numbers below are structurally zero, and that is
	// not "nothing happened" - it is "we did not move those bytes and cannot
	// count them". A client that renders a percentage from them is inventing
	// one (docs/design/18 §6.1).
	Strategy string `json:"strategy,omitempty"`

	CurrentWave int `json:"currentWave"`
	MaxWave     int `json:"maxWave"`

	// Progress is always a ROLLUP over jobs, never a maintained counter
	// (invariant I6). A counter would be a second source of truth for the same
	// fact and would drift; this cannot.
	Progress TransferProgress `json:"progress"`

	FailureReason string `json:"failureReason,omitempty"`

	// Waves is the per-wave breakdown, on a single transfer only.
	Waves []TransferWave `json:"waves,omitempty"`
	// Content is what the transfer is made OF - images, charts, files - and how
	// each of them went. On a single transfer only, for the same reason as
	// Waves: it is a second table per row, and the question is always asked
	// about one transfer.
	Content []ContentGroup `json:"content,omitempty"`
	// Promotion is present only on a transfer the registry carried out
	// itself, and is the only honest progress such a transfer has: it moved no
	// bytes, so every byte column on it is structurally zero.
	Promotion *PromotionProgress `json:"promotion,omitempty"`
	CreatedAt string             `json:"createdAt,omitempty"`
	// StartedAt is when the first job was leased, not when the transfer was
	// asked for. Elapsed time and throughput measured from the request would
	// count however long it waited for a worker as transfer time.
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// PromotionProgress is a native promotion, as it stands.
//
// NAMES rather than bytes, and the difference is the whole reason the field
// exists. A registry relocating content within itself already holds every
// blob; what it publishes is names, so names are the only denominator a
// promotion has. A client rendering a percentage from the byte columns of a
// promoted transfer is inventing one.
type PromotionProgress struct {
	// Promoter is the plugin that carried it: `jfrog` today.
	Promoter string `json:"promoter"`
	// State is REQUESTED, RUNNING, SUCCEEDED or FAILED.
	State string `json:"state"`

	NamesTotal int `json:"namesTotal"`
	NamesDone  int `json:"namesDone"`

	Attempts  int    `json:"attempts,omitempty"`
	LastError string `json:"lastError,omitempty"`

	RequestedAt string `json:"requestedAt,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	FinishedAt  string `json:"finishedAt,omitempty"`
}

// TransferProgress is what has happened so far, and what was planned.
type TransferProgress struct {
	// ContentBytes is the size of the RELEASE - every distinct digest counted
	// once - which is not PlannedBytes and is not meant to be.
	//
	// A component is published inside its bundle AND under its own name, and a
	// registry stores blobs per repository, so one blob landing in two
	// repositories is two jobs and its bytes appear twice in the planned work.
	// This is what the release weighs; PlannedBytes is what the transfer has to
	// do. Omitted where the package's size was never established.
	ContentBytes Int64String `json:"contentBytes,omitempty"`

	JobsPlanned     int `json:"jobsPlanned"`
	JobsDone        int `json:"jobsDone"`
	JobsFailed      int `json:"jobsFailed"`
	JobsOutstanding int `json:"jobsOutstanding"`

	// JobsInFlight is how many are being worked on RIGHT NOW, across Workers
	// distinct workers. Concurrency is otherwise invisible - a page of jobs
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
	// JobsRepaired have been sent back to the queue because the destination
	// denied holding content it had previously reported. Surfaced because it
	// is the only thing that makes a done count go DOWN, and progress that
	// moves backwards with no explanation reads as a broken tool.
	JobsRepaired int `json:"jobsRepaired,omitempty"`
	// OutstandingBytes is what is actually left to move - the size of every job
	// still to run, less what each has already sent. NOT planned minus
	// transferred, which counts bytes that will never move.
	OutstandingBytes Int64String `json:"outstandingBytes,omitempty"`
	// QuietestInFlight is when the least recently active in-flight job last
	// moved, RFC 3339. "1 job in flight" does not distinguish a job that is
	// transferring from one that is hung, and those need opposite responses.
	QuietestInFlight string `json:"quietestInFlight,omitempty"`
	// Skips is what the transfer did not move, by reason. "Done" is four
	// different claims wearing one word, and only some of them are evidence
	// that bytes reached the destination - see SkipBreakdown.
	Skips []SkipBreakdown `json:"skips,omitempty"`

	PlannedBytes     Int64String `json:"plannedBytes"`
	BytesTransferred Int64String `json:"bytesTransferred"`
	// DedupeSkippedBytes is what this transfer will NOT move because the
	// destination already had it. Reported rather than buried: it is the
	// number that makes the second transfer of a product line nearly free.
	DedupeSkippedBytes Int64String `json:"dedupeSkippedBytes"`
	// SkippedBytes was queued and then not sent: the worker found the content
	// at the destination, or the registry relocated it internally.
	SkippedBytes Int64String `json:"skippedBytes,omitempty"`
	// ContentMovedBytes and ContentPresentBytes are the byte account over
	// DISTINCT content - each piece weighed once, however many repositories it
	// has to reach.
	//
	// The figures above are per (repository, digest), which is right for
	// bookkeeping and wrong for bytes: a component published under its own name
	// as well as inside the bundle needs its layers in two repositories, and the
	// second copy is a mount that moves nothing. Counted that way a 29.8 GB
	// release reported 63.7 GB of traffic, which never happened, and a saving
	// larger than the release it was saving on.
	//
	// These two and ContentBytes are one population: Moved + Present converges
	// on ContentBytes and never exceeds it. They are what a progress bar and a
	// saving should be drawn from.
	ContentMovedBytes   Int64String `json:"contentMovedBytes,omitempty"`
	ContentPresentBytes Int64String `json:"contentPresentBytes,omitempty"`

	// SavedBytes is the two above added up - everything this transfer did not
	// have to move.
	//
	// It exists because reporting only the first was wrong in the case that
	// matters most. On a fresh database nothing is deduplicated at PLANNING
	// time, by definition: every saving is discovered by a worker, so a
	// transfer that skipped 32 GiB of content already at the target reported
	// saving nothing at all.
	SavedBytes Int64String `json:"savedBytes,omitempty"`
}

// SetPriorityRequest is the body of :setPriority.
//
// A required field with no useful zero: priority 0 is a real, legal value
// meaning "behind everything", so a caller who omits the field cannot be given
// a default without guessing which of the two they meant. The pointer is what
// makes "not said" distinguishable from "said zero".
type SetPriorityRequest struct {
	Priority *int `json:"priority"`
}

// TransferControlResponse is what :pause, :resume, :stop or :setPriority did.
type TransferControlResponse struct {
	TransferID string `json:"transferId"`
	// State is the transfer's state afterwards.
	State string `json:"state"`
	// Jobs is how many job rows the verb affected.
	Jobs int `json:"jobs"`
	// InFlight is how many jobs were still leased when it was applied.
	//
	// The number that explains a `stop` reporting `CANCELLING` rather than
	// `CANCELLED`: a leased job belongs to a worker and stops at that worker's
	// next checkpoint, not the instant the command was typed.
	InFlight int `json:"inFlight,omitempty"`
	// Priority is where the transfer now sits in the queue. Set by
	// :setPriority, which is the verb whose effect is otherwise invisible -
	// pausing something says PAUSED, reordering it says nothing at all.
	Priority int `json:"priority,omitempty"`
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

	// Parent is the artifact this job belongs to - what makes a digest
	// legible. A blob on its own is not something anybody can recognise; the
	// image or chart that references it is.
	Parent *JobParent `json:"parent,omitempty"`
}

// JobParent identifies the artifact a job belongs to.
type JobParent struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType,omitempty"`
	// Ref is the vendor's own name for it, from
	// org.opencontainers.image.ref.name - `orbs/CFX-5000-k8s/nginx:1.2.3`.
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

// SkipBreakdown is one reason a transfer moved no bytes, and how much that
// saved.
//
// Reported because "1976 of 1976 done" hides the difference between "we
// streamed it" and "something told us it was already there". The second is
// only as good as the answer it trusted, and a destination that answers about
// its whole storage rather than the repository asked about makes it worth
// nothing at all.
type SkipBreakdown struct {
	Reason string      `json:"reason"`
	Jobs   int         `json:"jobs"`
	Bytes  Int64String `json:"bytes"`
	// Trusted reports whether this rests on an ACTION the registry took (a
	// mount) rather than on a claim it or we made (a placement record, a HEAD).
	Trusted bool `json:"trusted"`
}

// PresentComponent is one component the destination already held.
type PresentComponent struct {
	// Name is the vendor's own name for it - `cfx-5000-product/bgcf:2511.174.0`.
	// Empty for a component the release names only by digest.
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest"`
	// Kind is what it is, in the words somebody uses: image, chart, file. Given
	// rather than derived, for the same reason ContentGroup gives it: the
	// vendor's own annotations are part of the answer and only the product's
	// layout plugin can read them.
	Kind string `json:"kind"`
	// Bytes is what this component's skipped jobs would have moved.
	Bytes Int64String `json:"bytes"`
	// Partial says only PART of it was already there - the rest is still to
	// move. An ordinary state, and a different claim from "this was already
	// there", which is what the list is otherwise saying.
	Partial bool `json:"partial,omitempty"`
}

// ListPresentComponentsResponse is GET /api/v1/transfers/{transfer}/present.
//
// WHAT a transfer did not have to move, by name.
//
// "Saved 56.5 GB" is the system's best claim about itself and unverifiable as
// stated; this is the list behind it. Every component here is one the
// destination already held, and the bytes are what its skipped jobs would have
// moved.
type ListPresentComponentsResponse struct {
	TransferID string             `json:"transferId"`
	Components []PresentComponent `json:"components"`
	// TotalBytes is what the whole list saved, so a caller showing part of it
	// can say what the rest comes to.
	TotalBytes Int64String `json:"totalBytes,omitempty"`
}

// FailureGroup is one distinct reason a transfer is failing.
//
// The job listing answers "which jobs are failing". This answers "why", which
// is a different question with a much shorter answer: five hundred manifests
// rejected by the destination are five hundred rows and ONE cause, and the
// rows differ only in the digest and the path - the two parts of the message
// that carry no information about what went wrong.
type FailureGroup struct {
	// Class is the retry classification, which is what decides whether a retry
	// could help: auth, unsupported, timeout, unavailable, not_found …
	Class string `json:"class,omitempty"`
	// Message is the failure with the per-job parts replaced by placeholders,
	// so one sentence stands for the whole group.
	Message string `json:"message"`

	// Failed have exhausted their attempts; Retrying will try again on their
	// own. "Act now" versus "wait" - the distinction that makes a stalled
	// transfer distinguishable from a working one.
	Failed   int `json:"failed"`
	Retrying int `json:"retrying"`

	Kinds []string `json:"kinds,omitempty"`
	Waves []int    `json:"waves,omitempty"`

	// One concrete job to go and look at, and its message verbatim.
	ExampleJobID      string   `json:"exampleJobId,omitempty"`
	ExampleDigest     string   `json:"exampleDigest,omitempty"`
	ExampleRepository string   `json:"exampleRepository,omitempty"`
	ExampleTags       []string `json:"exampleTags,omitempty"`
	ExampleError      string   `json:"exampleError,omitempty"`

	// Retryable reports whether retrying could plausibly succeed.
	Retryable bool `json:"retryable"`
}

// ListFailuresResponse is GET /api/v1/transfers/{transfer}/failures.
type ListFailuresResponse struct {
	TransferID string `json:"transferId"`
	// State and FailureReason are the TRANSFER's own, which is not the same
	// question as which of its jobs are failing.
	//
	// A transfer can fail before it has any jobs at all - an origin that cannot
	// be reached, a package whose tree will not walk - and its reason is then
	// recorded on the transfer rather than on work that was never created. A
	// summary built only from jobs answers "nothing is failing" about a
	// transfer whose state is `failed`, which is the one answer that cannot be
	// right.
	State         TransferState  `json:"state,omitempty"`
	FailureReason string         `json:"failureReason,omitempty"`
	Failures      []FailureGroup `json:"failures"`
}

// ---------------------------------------------------------------------------
// Promotion (docs/design/22)
// ---------------------------------------------------------------------------

// PromotionMethod is HOW a hop would be carried out.
type PromotionMethod string

const (
	// PromotionRelocate is the registry moving it internally: no bytes over
	// the wire, and normally seconds regardless of how large the release is.
	PromotionRelocate PromotionMethod = "RELOCATE"
	// PromotionCopy is our workers reading from one target and writing to the
	// other. Always correct, and within one registry still cheap - every blob
	// is relocated by cross-repository mount - but it walks the manifest tree
	// and issues a request per blob.
	PromotionCopy PromotionMethod = "COPY"
)

// PromotionOrigin is a target this release could be promoted OUT of.
type PromotionOrigin struct {
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	Registry    string `json:"registry"`
	Repository  string `json:"repository,omitempty"`

	// Holds says a transfer to this target SUCCEEDED, which is what makes it a
	// candidate origin at all: promotion moves what is already somewhere, and
	// offering a target that never received the release would produce a
	// promotion that fails at the first read.
	Holds bool `json:"holds"`
	// LastTransferID is the transfer that put it there, for a link.
	LastTransferID string `json:"lastTransferId,omitempty"`
}

// PromotionDestination is a target this release could be promoted INTO, and
// what promoting into it would actually do.
type PromotionDestination struct {
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	Registry    string `json:"registry"`
	Repository  string `json:"repository,omitempty"`

	// PromotionOnly marks a target reachable ONLY by promotion - a production
	// registry a vendor may never replicate into directly.
	PromotionOnly bool `json:"promotionOnly,omitempty"`
	Default       bool `json:"default,omitempty"`

	// Method is RELOCATE or COPY, decided the same way the expander will
	// decide it - through the same plugin claim, so the dialog cannot promise
	// one thing and the transfer do another.
	Method PromotionMethod `json:"method"`
	// MethodReason says WHY, in words, whichever answer came back. On COPY it
	// is the diagnosis: two Artifactory hosts, a target typed `generic`, a
	// release nobody has analysed. Always populated.
	MethodReason string `json:"methodReason,omitempty"`

	// State is ABSENT, PRESENT or IN_FLIGHT - whether this release is already
	// there, or on its way. What stops somebody promoting the same release
	// four times because nothing on screen said it had landed.
	State string `json:"state"`
	// TransferID is the transfer that put it there or is putting it there.
	TransferID string `json:"transferId,omitempty"`

	// Unavailable explains why this destination cannot be chosen at all - it
	// is the origin, or it is disabled. Empty means it can.
	Unavailable string `json:"unavailable,omitempty"`
}

// PromotionOptionsResponse is
// GET /api/v1/products/{product}/packages/{package}/promotionOptions.
//
// It exists because the promotion dialog asks ONE question - "where can this
// go, and what will happen" - whose answer is spread across configuration, the
// catalog, the transfer history and the promoter plugins. A client assembling
// it from four endpoints would be re-implementing the resolution rules in
// TypeScript, and the copy that drifted would be the one people clicked.
type PromotionOptionsResponse struct {
	Product string `json:"product"`
	Package string `json:"package"`
	Tag     string `json:"tag"`

	Origins []PromotionOrigin `json:"origins"`
	// DefaultOrigin is where the release was downloaded to, which is what a
	// promotion means when nobody says otherwise. Empty when the release is
	// not at any target yet, and then Promotable is false.
	DefaultOrigin string `json:"defaultOrigin,omitempty"`

	Destinations []PromotionDestination `json:"destinations"`
	// DefaultDestinations are the targets pre-selected in a dialog: the
	// product's promotion path where it resolves to exactly one, and nothing
	// where it does not. Deliberately empty rather than guessed when several
	// targets could be meant - see transfer.exactlyOneIn.
	DefaultDestinations []string `json:"defaultDestinations,omitempty"`

	// Analysed says whether the release's manifest tree has been walked. It
	// gates the fast path: the names underneath an unanalysed release are not
	// known here, and claiming on a partial tree would promote the root and
	// leave a bundle's components behind.
	Analysed bool `json:"analysed"`

	// Promotable is false when there is nothing to offer, and Reason says
	// which of the several ways that can be true this is: the release is
	// nowhere yet, the product has one target, every destination already
	// holds it.
	Promotable bool   `json:"promotable"`
	Reason     string `json:"reason,omitempty"`
}

// CreateTransferRequest is POST /api/v1/transfers.
//
// One request, one origin, several destinations. The OPERATION is not a field:
// it is derived from what `from` resolves to - a configured source means
// replicate, a configured target means promote - because a field that can
// disagree with `from` is a field that eventually will.
type CreateTransferRequest struct {
	Product string `json:"product"`
	// Package is a tag or a digest.
	Package string `json:"package"`

	// From names the origin. Empty means the repository the package was
	// discovered in, or - when Promote is set - the product's promotion
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
	// Created is false when an identical request already existed - a replay,
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

// ---------------------------------------------------------------------------
// Target replication (docs/design/18)
// ---------------------------------------------------------------------------

// ReplicationView is one target's replication configuration, what we last
// wrote to the registry, and what the registry says now.
//
// Three timestamps and no progress fields, deliberately. A delegated target
// reports a STATE and never a percentage: we do not move those bytes and
// cannot count them, and a number derived from elapsed time would be worse
// than its absence because somebody would make a decision from it.
type ReplicationView struct {
	Product string `json:"product"`
	Target  string `json:"target"`
	// Mode is copy, mirror or proxy. A target with no replication block
	// reports copy, which is what it has always meant.
	Mode string `json:"mode"`

	// Desired is what the loaded configuration asks for, with every credential
	// removed. Present for delegated modes only.
	Desired *ReplicationDesired `json:"desired,omitempty"`

	// Applied is what we last wrote. Absent means never applied, which is a
	// different state from applied-and-drifted and is reported as such.
	Applied *ReplicationApplied `json:"applied,omitempty"`

	// Observed is what the registry says right now. Absent when we could not
	// read it, and the reason is in Unreachable.
	Observed    *ReplicationObserved `json:"observed,omitempty"`
	Unreachable string               `json:"unreachable,omitempty"`

	// PendingApply means the configuration in Git has changed since the last
	// apply. Distinct from Drift: not having applied yet is a normal state of
	// affairs after a merge, and somebody editing the registry by hand is not.
	PendingApply bool `json:"pendingApply"`

	Drift *ReplicationDrift `json:"drift,omitempty"`
}

// ReplicationDesired is the configured intent, with secrets removed.
type ReplicationDesired struct {
	// Upstream is where the registry will pull from.
	Upstream string   `json:"upstream,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Interval string   `json:"interval,omitempty"`
	Robot    string   `json:"robot,omitempty"`
	// Manage is apply or detect: whether we ever write this configuration.
	Manage string `json:"manage,omitempty"`
	// SyncOnRequest reports whether a download against this target becomes a
	// sync-now and a wait.
	SyncOnRequest bool `json:"syncOnRequest,omitempty"`

	Organization string `json:"organization,omitempty"`
	Expiration   string `json:"expiration,omitempty"`
	// Prewarm reports whether discovery pulls packages through the cache.
	Prewarm bool `json:"prewarm,omitempty"`

	// ConfigHash fingerprints the non-secret configuration, so a caller can
	// compare it with Applied.ConfigHash without reading any of the fields.
	ConfigHash string `json:"configHash"`
}

// ReplicationApplied is the record of our last successful write.
type ReplicationApplied struct {
	ConfigHash string `json:"configHash"`
	At         string `json:"at,omitempty"`
	By         string `json:"by,omitempty"`
}

// ReplicationObserved is what the registry reports.
type ReplicationObserved struct {
	// RepositoryState is Quay's repository state. A mirror configuration on a
	// repository that is not in MIRROR state is inert.
	RepositoryState string `json:"repositoryState,omitempty"`
	// SyncStatus is our normalisation; SyncStatusRaw is the registry's own
	// word, kept because an enum that has grown across versions must not be
	// flattened into ours.
	SyncStatus    string `json:"syncStatus,omitempty"`
	SyncStatusRaw string `json:"syncStatusRaw,omitempty"`
	// Configured reports whether the registry holds any configuration at all.
	Configured bool     `json:"configured"`
	Tags       []string `json:"tags,omitempty"`
	Upstream   string   `json:"upstream,omitempty"`
	At         string   `json:"at,omitempty"`
}

// ReplicationDrift is what differs between the configuration and the registry.
type ReplicationDrift struct {
	Detected bool   `json:"detected"`
	Summary  string `json:"summary"`
	// Absent means the registry holds no configuration: not drift from a
	// previous apply, and reported separately because the fix differs.
	Absent bool `json:"absent,omitempty"`
	// StateWrong means the repository is not in the state mirroring needs.
	StateWrong bool `json:"stateWrong,omitempty"`
	// CredentialsRotated is derived from the hash of what we sent, never from
	// the response: the registry redacts stored passwords.
	CredentialsRotated bool               `json:"credentialsRotated,omitempty"`
	Fields             []ReplicationField `json:"fields,omitempty"`
}

// ReplicationField is one field on which configuration and registry disagree.
type ReplicationField struct {
	Field string `json:"field"`
	Want  string `json:"want"`
	Got   string `json:"got"`
}

// ApplyReplicationRequest asks for the configuration to be written.
type ApplyReplicationRequest struct {
	// ValidateOnly renders the plan and writes nothing.
	ValidateOnly bool `json:"validateOnly,omitempty"`
	// Confirm must be true for a destructive apply. A plan that will make a
	// repository read-only and delete tags the glob does not match is not
	// something a caller should be able to do by omission.
	Confirm bool `json:"confirm,omitempty"`
}

// ApplyReplicationResponse is what an apply did, or would do.
type ApplyReplicationResponse struct {
	Product string `json:"product"`
	Target  string `json:"target"`
	Mode    string `json:"mode"`

	// Applied reports whether anything was written. False for a dry run, for a
	// no-op, and for a destructive plan that was not confirmed.
	Applied bool `json:"applied"`
	// Steps is what was done, or would be, in the reader's own terms.
	Steps []string `json:"steps"`
	// Destructive marks a plan that makes a repository read-only or removes
	// content. Carried separately from Steps so a client cannot render the
	// plan without being able to render the warning.
	Destructive bool `json:"destructive"`
	// NeedsConfirmation means the plan is destructive and Confirm was not set.
	NeedsConfirmation bool `json:"needsConfirmation,omitempty"`
	NoOp              bool `json:"noOp,omitempty"`

	ConfigHash string `json:"configHash,omitempty"`
}

// SyncReplicationResponse is the outcome of asking for a sync now.
//
// No progress, no ETA and no byte count. The registry gives us a state and, at
// best, a completion time.
type SyncReplicationResponse struct {
	Product string `json:"product"`
	Target  string `json:"target"`
	// Requested reports that we asked. AlreadyRunning reports that a sync was
	// already under way, which SATISFIES the request rather than failing it.
	Requested      bool   `json:"requested"`
	AlreadyRunning bool   `json:"alreadyRunning,omitempty"`
	At             string `json:"at"`
	SyncID         int64  `json:"syncId,omitempty"`
}

// MirrorSyncView is one observed sync run.
type MirrorSyncView struct {
	ID     int64  `json:"id"`
	Target string `json:"target"`
	// Status is OUR classification: requested, running, succeeded, diverged,
	// failed or unknown. QuayStatus is the registry's own word.
	Status     string `json:"status"`
	QuayStatus string `json:"quayStatus,omitempty"`

	RequestedAt string `json:"requestedAt,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	FinishedAt  string `json:"finishedAt,omitempty"`

	// ExpectedDigest is what we asked the registry to end up holding;
	// ObservedDigest is what a walk of the destination actually found. They
	// differ on a `diverged` outcome, which is a fact rather than a failure.
	ExpectedDigest string `json:"expectedDigest,omitempty"`
	ObservedDigest string `json:"observedDigest,omitempty"`

	TransferID string `json:"transferId,omitempty"`
	Message    string `json:"message,omitempty"`
}

// ListSyncsResponse is a target's observed sync history.
type ListSyncsResponse struct {
	Syncs []MirrorSyncView `json:"syncs"`
}

// ListReplicationResponse is every target's replication state.
type ListReplicationResponse struct {
	Targets []ReplicationView `json:"targets"`
}

// ---------------------------------------------------------------------------
// Downloads and auto-download (docs/design/20)
//
// Two resources, because they are two things: a download is WHAT happens, and
// an auto-download rule is WHEN it happens by itself. A rule holds a pattern
// and nothing else; a download holds no pattern at all, because by the time
// one runs the software has already been chosen.
// ---------------------------------------------------------------------------

// DownloadView is one configured download and the chain it resolves to.
type DownloadView struct {
	Product string `json:"product"`
	// Name is empty for a product that declares a single unnamed download.
	Name string `json:"name,omitempty"`

	// Targets is what the download NAMES. Chain is what that resolves to,
	// closed over the targets' own `mirror.from`. They differ whenever a
	// download names the tail of a chain, which is the normal case.
	Targets   []string `json:"targets,omitempty"`
	Chain     []string `json:"chain,omitempty"`
	ChainText string   `json:"chainText,omitempty"`
	// ChainError is why the chain could not be derived. One broken download
	// must not blank a listing.
	ChainError string `json:"chainError,omitempty"`

	Priority int  `json:"priority"`
	Default  bool `json:"default"`

	// VerifyBefore and VerifyAfter are tri-state: "true", "false" or
	// "inherit". A download that says nothing about destination verification
	// is not one that turned it off, and rendering both as false would tell a
	// reader the opposite of the truth.
	VerifyBefore string `json:"verifyBefore,omitempty"`
	VerifyAfter  string `json:"verifyAfter,omitempty"`
	VerifyPolicy string `json:"verifyPolicy,omitempty"`

	Revision string `json:"revision"`
}

// ListDownloadsResponse is a product's downloads.
type ListDownloadsResponse struct {
	Downloads []DownloadView `json:"downloads"`
}

// AutoDownloadRuleView is one rule and the download it triggers.
type AutoDownloadRuleView struct {
	Product string `json:"product"`
	Name    string `json:"name"`

	// TagPattern is the whole of what a rule decides. Where the software goes
	// is the download's business.
	TagPattern string   `json:"tagPattern"`
	Sources    []string `json:"sources,omitempty"`

	// Download names what this rule triggers; Chain is that download's
	// resolved steps, repeated here so a reader does not have to cross-
	// reference two listings to answer "and then what happens".
	Download   string   `json:"download,omitempty"`
	Chain      []string `json:"chain,omitempty"`
	ChainText  string   `json:"chainText,omitempty"`
	ChainError string   `json:"chainError,omitempty"`

	// Enabled is configuration, from Git, and the only way a rule is turned
	// off. There is deliberately no runtime override.
	Enabled bool `json:"enabled"`

	// Inline reports that the rule carries its own targets - the older
	// spelling, from before downloads were a block of their own.
	Inline bool `json:"inline,omitempty"`
}

// ListAutoDownloadRulesResponse is a product's rules.
type ListAutoDownloadRulesResponse struct {
	// Enabled is the master switch over automatic firing. Downloads by hand
	// are unaffected by it.
	Enabled bool                   `json:"enabled"`
	Rules   []AutoDownloadRuleView `json:"rules"`
}

// RunDownloadRequest downloads named software by hand.
//
// Note what is absent: any pattern or filter. Patterns belong to auto-download
// rules, which decide what to download when nobody is asking. Here somebody is
// asking, and they named the software.
type RunDownloadRequest struct {
	// Tags names the software. Required.
	//
	// Each entry is a package REFERENCE, not merely a version: `25.7_mp2604_2131`
	// or `orbs/cfx-5000-k8s:25.7_mp2604_2131`. A vendor publishes one version
	// into every repository of a product, so a bare version is ambiguous there
	// and is REFUSED with the repositories it matched rather than resolved to
	// whichever row came back first - which is how a download of one component
	// came to move a different one.
	//
	// The field keeps its name so an existing client's request is still a valid
	// request; a version unique to one repository still resolves bare.
	Tags []string `json:"tags"`
	// Download names which configured download to use. Empty means the
	// default.
	Download string `json:"download,omitempty"`
	// ValidateOnly renders the plan and creates nothing.
	ValidateOnly bool `json:"validateOnly,omitempty"`
}

// RunDownloadResponse is what a download did, or would do.
type RunDownloadResponse struct {
	Product  string   `json:"product"`
	Download string   `json:"download"`
	Chain    []string `json:"chain"`

	Requested []string `json:"requested,omitempty"`
	// Created names the requests opened; AlreadyRequested is the software
	// whose idempotency key already existed, which is a normal outcome rather
	// than a failure.
	Created          []string `json:"created,omitempty"`
	AlreadyRequested []string `json:"alreadyRequested,omitempty"`
	ValidateOnly     bool     `json:"validateOnly,omitempty"`
}

// MatchesResponse is what an auto-download rule would pick up.
type MatchesResponse struct {
	Product string   `json:"product"`
	Rule    string   `json:"rule"`
	Matches []string `json:"matches"`
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// AuditEvent is one entry in the trail.
//
// The read side of a table that has been written since the first migration
// (docs/design/12 §4). Nothing consumed it until there was an interface that
// could show it.
type AuditEvent struct {
	// Name is the AIP-122 resource name: "auditEvents/{id}".
	Name string `json:"name"`
	ID   string `json:"id"`

	OccurredAt string `json:"occurredAt"`
	EventType  string `json:"eventType"`

	// Actor is who, and ActorKind is what sort of who: user, system, worker,
	// schedule or auto_rule. Both matter - "the system did it" and "a person
	// did it" are the two answers this trail exists to distinguish.
	Actor     string `json:"actor"`
	ActorKind string `json:"actorKind"`

	Product string `json:"product,omitempty"`
	// SubjectKind and SubjectID name what the event was ABOUT, which is what
	// makes the trail filterable down to one release.
	SubjectKind string `json:"subjectKind,omitempty"`
	SubjectID   string `json:"subjectId,omitempty"`

	RequestID string `json:"requestId,omitempty"`
	TraceID   string `json:"traceId,omitempty"`
	Outcome   string `json:"outcome"`

	// Detail is the producer's own JSON payload, passed through unparsed.
	// Every event type writes a different shape, and a reader that insisted on
	// knowing them all would need editing for each new one.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// ListAuditEventsResponse is GET /api/v1/auditEvents.
type ListAuditEventsResponse struct {
	AuditEvents   []AuditEvent `json:"auditEvents"`
	NextPageToken string       `json:"nextPageToken,omitempty"`
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

// ReportSummary is the operational rollup for a period.
//
// Note what is NOT here: no verification metric, because nothing writes
// verification records yet and a confident zero would be worse than an absence
// (docs/design/19 §6). Nor an "average download time", which measures how big
// the releases were rather than how the system performed.
type ReportSummary struct {
	// Period restates the bounds the server actually applied, so a reader
	// never has to assume which period a figure belongs to.
	Period ReportPeriod `json:"period"`

	// Totals is every in-scope product added together.
	Totals ReportTotals `json:"totals"`
	// Products is the same figures per product. Present always, because a
	// caller scoped to a subset must get their subset's numbers rather than an
	// estate-wide total they are not entitled to see.
	Products []ProductReport `json:"products"`

	// FailureCauses groups the period's failed jobs, worst first.
	FailureCauses []FailureCause `json:"failureCauses,omitempty"`
	// Volume is the per-day trend, oldest first.
	Volume []DailyVolume `json:"volume,omitempty"`
}

// ReportPeriod is the window a report covers.
type ReportPeriod struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Label is the requested period as typed - `7d`, `30d` - or empty for an
	// explicit range.
	Label string `json:"label,omitempty"`
}

// ReportTotals is one scope's figures.
type ReportTotals struct {
	DownloadsCompleted int `json:"downloadsCompleted"`
	DownloadsFailed    int `json:"downloadsFailed"`
	DownloadsCancelled int `json:"downloadsCancelled"`
	DownloadsRunning   int `json:"downloadsRunning"`
	Promotions         int `json:"promotions"`

	BytesTransferred Int64String `json:"bytesTransferred"`
	// SavedBytes is what did not have to move, and SavedPercent is that against
	// what would otherwise have moved. The clearest demonstration of the
	// system's value, so it is stated as both.
	SavedBytes Int64String `json:"savedBytes"`
	// SavedPercent is omitted when nothing moved and nothing was saved, since
	// a percentage of zero is not zero percent.
	SavedPercent *float64 `json:"savedPercent,omitempty"`

	// AverageBytesPerSecond is measured over `copy` transfers ONLY - the ones
	// whose bytes we moved and counted. OMITTED, not zero, when no such
	// transfer completed in the period: a mirror-only period has no speed we
	// are entitled to state (docs/design/18 §6.1).
	AverageBytesPerSecond *Int64String `json:"averageBytesPerSecond,omitempty"`
	// SuccessRate is completed against completed-plus-failed, 0..1. Omitted
	// when nothing settled in the period.
	SuccessRate *float64 `json:"successRate,omitempty"`
}

// ProductReport is one product's figures for the period.
type ProductReport struct {
	Product string       `json:"product"`
	Totals  ReportTotals `json:"totals"`
}

// FailureCause is one group of failed jobs.
type FailureCause struct {
	Product string `json:"product"`
	// Class is the worker's error classification, or "unclassified" where a
	// job failed before one was assigned. Never blank, so a reader is not left
	// wondering whether the field is missing or the value is.
	Class string `json:"class"`
	Jobs  int    `json:"jobs"`
}

// DailyVolume is one day of throughput.
type DailyVolume struct {
	Day              string      `json:"day"`
	BytesTransferred Int64String `json:"bytesTransferred"`
	Downloads        int         `json:"downloads"`
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// WhoAmIResponse describes the calling identity and what it may do.
//
// # Why this exists before authentication does
//
// So that no client ever hardcodes a role model. A UI that decides for itself
// which buttons an operator may press has to be edited in every place the day
// real roles arrive; a UI that renders what this endpoint reports does not.
//
// Today it answers `anonymous`, method `none`, permissions `["*"]`, which is
// exactly what the Coordinator's AnonymousAuthenticator grants. That is not
// authentication and does not pretend to be - it is the shape authentication
// will fill in (docs/design/09 §10).
type WhoAmIResponse struct {
	Subject string `json:"subject"`
	// Method records how the identity was established: "none", "oidc",
	// "kubernetes" or "token". A client shows "Authentication is not enabled"
	// on "none" rather than implying a real session.
	Method string `json:"method"`
	// Authenticated is false whenever Method is "none". Stated as its own
	// field so a client does not have to know which method strings count.
	Authenticated bool `json:"authenticated"`

	// Tenant is which tenant this caller belongs to. Empty until tenancy
	// exists; a client treats empty as "the whole estate".
	Tenant string `json:"tenant,omitempty"`

	Roles []string `json:"roles,omitempty"`
	// Permissions are the actions this caller may perform. `["*"]` means
	// everything, which is what an unauthenticated deployment reports.
	Permissions []string `json:"permissions"`

	// Products limits what this caller may see. Empty means every product -
	// the same convention the server-side scope filters use, so a client
	// reading this never has to special-case the unscoped deployment.
	Products []string `json:"products,omitempty"`

	// Features are deployment-wide switches unrelated to who is asking - a
	// separate section from Permissions, which is about what THIS caller may
	// do. A client reads this once to decide which controls exist at all,
	// rather than growing a new top-level boolean each time one is added.
	Features Features `json:"features"`
}

// Features are deployment-wide toggles, off a config file rather than a role.
type Features struct {
	// FileDownloads says whether a reader may save a release file's raw
	// bytes, separately from looking at its text - see
	// FilesConfig.DownloadEnabled. A client hides the control rather than
	// showing one that always 404s.
	FileDownloads bool `json:"fileDownloads"`
}
