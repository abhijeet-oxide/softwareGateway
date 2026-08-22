package v1

// Security types.
//
// The wire shape of internal/security, and deliberately a SEPARATE set of types
// rather than the domain types with JSON tags. Two reasons, and the second is
// the one that matters:
//
//  1. The API is a contract with clients that ship independently of this
//     binary, and the domain model must stay free to change shape.
//  2. Several fields here exist ONLY for the interface - StatusLabel,
//     SeverityLabel, Explanation, Caveats - and they are the whole point of the
//     "simple view". Computing them in the browser would mean every client
//     re-deriving the sentence "Release B is better than Release A", and two
//     clients would eventually derive it differently over the same data.

// SecuritySeverityCounts is a count per severity. Every key is always present,
// because a table cell whose answer is zero must render as zero rather than as
// a blank.
type SecuritySeverityCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

// SecurityCounts is a full account of a set of findings.
//
// Fixable travels separately from the total because it is the number that
// decides what somebody does this afternoon: 900 non-fixable findings and 4
// fixable ones is four pieces of work, and "904" hides all four.
type SecurityCounts struct {
	Total      int `json:"total"`
	Fixable    int `json:"fixable"`
	NonFixable int `json:"nonFixable"`

	BySeverity        SecuritySeverityCounts `json:"bySeverity"`
	FixableBySeverity SecuritySeverityCounts `json:"fixableBySeverity"`
}

// SecurityCoverage states how much of a release the numbers actually cover.
//
// Always sent alongside the counts, never separately. "1,286 vulnerabilities"
// means one thing when every artifact was scanned and something else when a
// fifth were not, and a client that received the first number without the
// second would have no way to know which it was looking at.
type SecurityCoverage struct {
	Artifacts   int `json:"artifacts"`
	Scanned     int `json:"scanned"`
	NotScanned  int `json:"notScanned"`
	Unsupported int `json:"unsupported"`
	Unavailable int `json:"unavailable"`
	Disabled    int `json:"disabled"`
	// Scannable is the denominator a percentage should use: it excludes
	// artifacts a scanner could never have an opinion about, such as
	// signatures, which would otherwise pin every release below full coverage.
	Scannable int `json:"scannable"`
	// Complete is whether everything that could be scanned, was.
	Complete bool `json:"complete"`
}

// SecurityComponent is the package a finding is against.
type SecurityComponent struct {
	// ID excludes the version on purpose - "deb://openssl", not
	// "deb://openssl:1.1.1n". It is the key two releases are compared on, and a
	// version in it would make every package bump read as one finding resolved
	// and one introduced.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Type    string `json:"type,omitempty"`
	Path    string `json:"path,omitempty"`
}

// SecurityArtifact identifies what was scanned.
type SecurityArtifact struct {
	// Name is what two releases are ALIGNED on. Digest is the identity within
	// one release. They are different fields because digests are what change
	// between releases.
	Name       string `json:"name"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Repository string `json:"repository,omitempty"`
	Kind       string `json:"kind,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	Platform   string `json:"platform,omitempty"`
	// Display is the artifact as the interface names it, "cfx-main:25.7.2131".
	Display string `json:"display,omitempty"`
}

// SecurityFinding is one vulnerability against one package in one artifact.
type SecurityFinding struct {
	CVE string `json:"cve,omitempty"`
	// ID is the scanner's own identifier. Present because a finding without a
	// CVE still needs an identity, and because a vendor support case is opened
	// against this one.
	ID string `json:"id,omitempty"`

	Severity string `json:"severity"`
	// SeverityLabel is the severity in the words to display. Sent rather than
	// derived so every client says the same word.
	SeverityLabel string `json:"severityLabel"`

	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`

	Component SecurityComponent `json:"component"`

	FixedIn []string `json:"fixedIn,omitempty"`
	Fixable bool     `json:"fixable"`

	CVSSScore  float64  `json:"cvssScore,omitempty"`
	CVSSVector string   `json:"cvssVector,omitempty"`
	References []string `json:"references,omitempty"`
	Published  string   `json:"published,omitempty"`

	Provider string `json:"provider"`
	Policy   string `json:"policy,omitempty"`
}

// SecurityReport is one artifact's security state.
type SecurityReport struct {
	Artifact SecurityArtifact `json:"artifact"`

	// Status is scanned | not_scanned | unsupported | disabled | unavailable.
	//
	// A client that renders Findings without reading this has written the bug
	// the whole feature exists to prevent: "scanned and clean" and "nobody
	// looked" are both an empty list.
	Status      string `json:"status"`
	StatusLabel string `json:"statusLabel"`
	Provider    string `json:"provider,omitempty"`
	Message     string `json:"message,omitempty"`

	Findings []SecurityFinding `json:"findings,omitempty"`
	Counts   SecurityCounts    `json:"counts"`

	ScannedAt   string `json:"scannedAt,omitempty"`
	RetrievedAt string `json:"retrievedAt,omitempty"`
	FromCache   bool   `json:"fromCache,omitempty"`
}

// PackageSecurityResponse is
// GET /api/v1/products/{product}/packages/{package}/security.
type PackageSecurityResponse struct {
	Product string `json:"product"`
	Package string `json:"package"`

	// Provider names the scanner; Enabled says whether it is switched on for
	// the repository this release was discovered in.
	Provider string `json:"provider,omitempty"`
	Enabled  bool   `json:"enabled"`
	// Repository is the CONFIGURED repository name the scanner answers for.
	Repository string `json:"repository,omitempty"`

	// State is the one-word summary of whether these numbers can be trusted:
	// ok | partial | unavailable | disabled. Distinct from a clean result,
	// which is State "ok" with zero counts.
	State string `json:"state"`
	// Message explains a State that is not ok, in words with an action in them.
	Message string `json:"message,omitempty"`

	// Counts is every scanned artifact's findings summed - the same CVE in ten
	// images is ten things to fix in ten places. UniqueCounts collapses them,
	// which is the number to quote for "how many distinct problems".
	Counts       SecurityCounts   `json:"counts"`
	UniqueCounts SecurityCounts   `json:"uniqueCounts"`
	Coverage     SecurityCoverage `json:"coverage"`

	Reports   []SecurityReport `json:"reports"`
	Providers []string         `json:"providers,omitempty"`

	ScannedAt   string `json:"scannedAt,omitempty"`
	RetrievedAt string `json:"retrievedAt,omitempty"`
	// Fingerprint is the ETag body. Exposed so a client can tell an unchanged
	// re-read from a changed one without diffing megabytes.
	Fingerprint string `json:"fingerprint,omitempty"`
	// FromCache and Fetched say how the answer was assembled, which is what
	// makes a refresh button meaningful.
	FromCache int `json:"fromCache"`
	Fetched   int `json:"fetched"`
	// Detail says whether Reports carry findings or counts alone.
	Detail bool `json:"detail"`
}

// SecurityChange is one finding's fate between two releases.
type SecurityChange struct {
	// Type is introduced | resolved | unchanged | severity_increased |
	// severity_decreased | remediation_changed | removed_artifact.
	Type      string `json:"type"`
	TypeLabel string `json:"typeLabel"`

	CVE string `json:"cve,omitempty"`
	ID  string `json:"id,omitempty"`

	Severity      string `json:"severity"`
	SeverityLabel string `json:"severityLabel"`
	FromSeverity  string `json:"fromSeverity,omitempty"`
	ToSeverity    string `json:"toSeverity,omitempty"`

	Fixable bool     `json:"fixable"`
	FixedIn []string `json:"fixedIn,omitempty"`

	Summary     string            `json:"summary,omitempty"`
	Description string            `json:"description,omitempty"`
	Component   SecurityComponent `json:"component"`

	Artifact SecurityArtifact `json:"artifact"`
	// ArtifactChange is common | upgraded | added | removed, so a reader can
	// tell "this arrived in a new image" from "this arrived in an image that
	// was already there".
	ArtifactChange string `json:"artifactChange"`
	// ViaRemoval marks a resolution that happened because the artifact carrying
	// it is no longer shipped, rather than because anything was patched.
	ViaRemoval bool `json:"viaRemoval,omitempty"`

	Provider string `json:"provider,omitempty"`
}

// SecurityArtifactDelta is one artifact's fate, with its arithmetic.
type SecurityArtifactDelta struct {
	Key    string `json:"key"`
	Change string `json:"change"`

	A *SecurityArtifact `json:"a,omitempty"`
	B *SecurityArtifact `json:"b,omitempty"`

	StatusA string `json:"statusA,omitempty"`
	StatusB string `json:"statusB,omitempty"`

	CountsA SecurityCounts `json:"countsA"`
	CountsB SecurityCounts `json:"countsB"`

	Introduced      int `json:"introduced"`
	Resolved        int `json:"resolved"`
	Unchanged       int `json:"unchanged"`
	SeverityChanged int `json:"severityChanged"`

	// Comparable is false when one side has no scan result. The counts are then
	// zero because nothing was computed, not because nothing changed.
	Comparable bool `json:"comparable"`
}

// SecurityArtifactSummary counts the artifact delta. Two releases do not
// contain the same artifacts, and a comparison that assumed they did would be
// wrong before it started.
type SecurityArtifactSummary struct {
	Common        int `json:"common"`
	Upgraded      int `json:"upgraded"`
	Added         int `json:"added"`
	Removed       int `json:"removed"`
	NotComparable int `json:"notComparable"`
}

// SecurityComparisonEnd identifies one side of a comparison.
type SecurityComparisonEnd struct {
	Label      string           `json:"label"`
	Package    string           `json:"package,omitempty"`
	Tag        string           `json:"tag,omitempty"`
	Digest     string           `json:"digest,omitempty"`
	Repository string           `json:"repository,omitempty"`
	Provider   string           `json:"provider,omitempty"`
	Enabled    bool             `json:"enabled"`
	Counts     SecurityCounts   `json:"counts"`
	Coverage   SecurityCoverage `json:"coverage"`
	ScannedAt  string           `json:"scannedAt,omitempty"`
}

// SecurityComparisonResponse is POST
// /api/v1/products/{product}/packages/{package}:compareSecurity.
//
// The simple view and the detailed view are the SAME object read at two
// depths. Two objects would drift, and the day they drift is the day the
// headline says "better" over a table that says otherwise.
type SecurityComparisonResponse struct {
	Product string                `json:"product"`
	A       SecurityComparisonEnd `json:"a"`
	B       SecurityComparisonEnd `json:"b"`

	// Verdict is better | worse | unchanged | inconclusive.
	Verdict      string `json:"verdict"`
	VerdictLabel string `json:"verdictLabel"`
	// Headline is the one-line answer. Explanation is the paragraph a person
	// with no security background can act on.
	Headline    string `json:"headline"`
	Explanation string `json:"explanation"`
	// Caveats qualify the answer - unscanned artifacts, a disabled scanner,
	// findings that could not be classified. Separate from Explanation so a
	// client renders them as warnings rather than burying them in a sentence.
	Caveats []string `json:"caveats,omitempty"`

	Introduced         SecurityCounts `json:"introduced"`
	Resolved           SecurityCounts `json:"resolved"`
	Unchanged          SecurityCounts `json:"unchanged"`
	SeverityIncreased  SecurityCounts `json:"severityIncreased"`
	SeverityDecreased  SecurityCounts `json:"severityDecreased"`
	RemediationChanged SecurityCounts `json:"remediationChanged"`
	// RemovedArtifact counts findings that left with an artifact whose removal
	// the rules would not confirm as an improvement. Not in Resolved.
	RemovedArtifact SecurityCounts `json:"removedArtifact"`

	// NetScore is the severity-weighted difference; negative is better.
	// Exposed because "why did it say worse" is the second question, and a
	// number that can be checked beats a rule that cannot.
	NetScore int `json:"netScore"`

	Changes         []SecurityChange        `json:"changes"`
	Artifacts       []SecurityArtifactDelta `json:"artifacts"`
	ArtifactSummary SecurityArtifactSummary `json:"artifactSummary"`

	Fingerprint string `json:"fingerprint,omitempty"`
	RetrievedAt string `json:"retrievedAt,omitempty"`
}

// SecurityCompareRequest is the body of a security comparison.
type SecurityCompareRequest struct {
	// Against is the other release - a tag or a digest. Required: a security
	// comparison is between two versions, unlike a package comparison which
	// may be between two places.
	Against string `json:"against,omitempty"`
	// Repository disambiguates Against when the same tag exists in two of a
	// product's repositories.
	Repository string `json:"repository,omitempty"`
	// Refresh bypasses the cache on both sides.
	Refresh bool `json:"refresh,omitempty"`
	// ProgressToken is a caller-minted id it can poll at
	// GET /api/v1/security/progress/{token} while this request is open.
	ProgressToken string `json:"progressToken,omitempty"`
}

// SecurityRelease names a release a finding was found in.
type SecurityRelease struct {
	PackageID  string `json:"packageId"`
	Tag        string `json:"tag"`
	DisplayTag string `json:"displayTag,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

// SecuritySearchHit is one search result, with everything needed to navigate
// outward from it: to the package, to the image, to the releases shipping it.
type SecuritySearchHit struct {
	CVE     string `json:"cve,omitempty"`
	IssueID string `json:"issueId,omitempty"`

	Severity      string `json:"severity"`
	SeverityLabel string `json:"severityLabel"`
	Fixable       bool   `json:"fixable"`
	Summary       string `json:"summary,omitempty"`

	Component SecurityComponent `json:"component"`
	FixedIn   string            `json:"fixedIn,omitempty"`

	Artifact   SecurityArtifact `json:"artifact"`
	Provider   string           `json:"provider,omitempty"`
	Repository string           `json:"repository,omitempty"`
	ScannedAt  string           `json:"scannedAt,omitempty"`

	// Releases are the releases that ship this artifact. The edge that makes
	// the relationship navigable in both directions.
	Releases []SecurityRelease `json:"releases,omitempty"`
}

// SecuritySearchResponse is GET /api/v1/products/{product}/security/search.
type SecuritySearchResponse struct {
	Product string `json:"product"`
	// Kind is cve | package | image.
	Kind  string `json:"kind"`
	Query string `json:"query"`
	Exact bool   `json:"exact,omitempty"`

	Hits []SecuritySearchHit `json:"hits"`
	// Truncated says the result hit the limit, so a reader knows the list is a
	// page rather than the whole answer.
	Truncated bool `json:"truncated,omitempty"`

	// Searched says what the search actually covered. A search reads what the
	// platform has already retrieved, so a release nobody has opened is not in
	// it - and a result that did not say so would read as "this CVE is not in
	// your estate", which is a dangerous thing to say wrongly.
	Searched SecuritySearchScope `json:"searched"`
}

// SecuritySearchScope describes what a search covered.
type SecuritySearchScope struct {
	// Artifacts and Releases are how much retrieved security data was searched.
	Artifacts int `json:"artifacts"`
	Releases  int `json:"releases"`
	// Note is the sentence to show under the results.
	Note string `json:"note,omitempty"`
}

// SecurityProgressStage is one phase of a retrieval.
type SecurityProgressStage struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Done  int    `json:"done"`
	// Total is what is KNOWN so far; zero means not yet known, which is honest
	// while a tree is still being walked.
	Total int `json:"total"`
}

// SecurityProgressResponse is GET /api/v1/security/progress/{token}.
//
// The side channel a security retrieval reports through while its own request
// is open. A 404 is a normal answer: progress lives in the memory of the
// replica doing the work and is dropped shortly after it finishes.
type SecurityProgressResponse struct {
	Stages []SecurityProgressStage `json:"stages"`
	// Notes are the sentences worth showing that are not a position - "Asking
	// JFrog Xray about 157 artifacts in 4 requests."
	Notes     []string `json:"notes,omitempty"`
	Done      bool     `json:"done"`
	StartedAt string   `json:"startedAt,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

// Security export formats.
const (
	ExportCSV   = "csv"
	ExportXLSX  = "xlsx"
	ExportJSON  = "json"
	ExportExcel = "excel"
)

// Security export views.
const (
	// ExportViewSummary is the simple result: the verdict, the counts, the
	// coverage. What goes in a release note.
	ExportViewSummary = "summary"
	// ExportViewDetailed is every row: each finding or each change, with the
	// release, artifact, package and CVE it belongs to.
	ExportViewDetailed = "detailed"
)
