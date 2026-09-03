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
	// KEV is how many are known to be exploited, and KEVFixable how many of
	// those have a version to upgrade to.
	//
	// The second is this afternoon's work, in one number. Sent rather than
	// derived because a listing draws the badge without any findings loaded.
	KEV        int `json:"kev"`
	KEVFixable int `json:"kevFixable"`

	BySeverity        SecuritySeverityCounts `json:"bySeverity"`
	FixableBySeverity SecuritySeverityCounts `json:"fixableBySeverity"`
	// KEVBySeverity grades the exploited ones, because "4 KEVs" and "4
	// critical KEVs" are read differently.
	KEVBySeverity SecuritySeverityCounts `json:"kevBySeverity"`
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
	// Missing is artifacts that are not in the scanned repository at all,
	// which is a transfer to run rather than a scan to wait for.
	Missing int `json:"missing"`
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

	// KEV says this vulnerability is on a known-exploited catalogue, and
	// KEVSource names the scanner that said so.
	//
	// The single most load-bearing field on this type. It is not a severity -
	// it is a record that somebody has already exploited this - so it sorts
	// above severity, carries its own badge, and has its own segment on the
	// page. False means "no scanner that answered said so", which on a scanner
	// with no exploited-vulnerability feed is every finding.
	KEV       bool   `json:"kev,omitempty"`
	KEVSource string `json:"kevSource,omitempty"`
	// EPSS is the exploit-prediction score and its percentile, where a scanner
	// supplied them. Displayed, never sorted on.
	EPSS *SecurityEPSS `json:"epss,omitempty"`
	// WillNotFix says the vendor has declined to fix this in the affected
	// stream - which is not the same as having no fixed version yet. The first
	// is a wait; the second is a decision to mitigate or accept.
	WillNotFix bool `json:"willNotFix,omitempty"`
	// Observations is every severity and score this finding was reported with,
	// and who reported each.
	//
	// Sent because the disagreements are the evidence behind the one number the
	// row shows: two scanners grading one CVE differently is not noise, and the
	// reader is the one who knows whether their deployment matches the vendor's
	// assumption.
	Observations []SecurityObservation `json:"observations,omitempty"`

	Provider string `json:"provider"`
	Policy   string `json:"policy,omitempty"`
	// Sources names every scanner that reported this finding.
	//
	// Provider says which row this came from; Sources says who agrees. On a
	// deployment with one scanner it holds one name and the interface hides the
	// column - a column that says "JFrog Xray" on every row is a column that
	// costs width and says nothing.
	Sources []string `json:"sources,omitempty"`
}

// SecurityEPSS is the Exploit Prediction Scoring System's estimate.
//
// Two numbers because the second is the readable one: 0.00042 tells almost
// nobody anything, and "in the bottom 12%" tells everybody something.
type SecurityEPSS struct {
	Score      float64 `json:"score"`
	Percentile float64 `json:"percentile,omitempty"`
}

// SecurityObservation is one source's grading of one finding.
type SecurityObservation struct {
	Provider      string  `json:"provider"`
	ProviderLabel string  `json:"providerLabel,omitempty"`
	Source        string  `json:"source,omitempty"`
	Severity      string  `json:"severity,omitempty"`
	SeverityLabel string  `json:"severityLabel,omitempty"`
	Score         float64 `json:"score,omitempty"`
	Vector        string  `json:"vector,omitempty"`
}

// SecurityViolation is one breach of a configured policy.
//
// Not a finding with a policy field. A finding is "this image contains
// CVE-2026-31789"; a violation is "your Production watch forbids critical
// fixable issues and this image has four" - it exists because somebody wrote a
// rule, it disappears when the rule changes, and it can be raised against a
// licence with no CVE anywhere near it.
type SecurityViolation struct {
	ID string `json:"id,omitempty"`
	// Type is security | license | operational_risk, as the scanner grades it.
	Type          string `json:"type,omitempty"`
	Severity      string `json:"severity"`
	SeverityLabel string `json:"severityLabel"`

	// Watch, Policy and Rule are the rule's address. All three, because "a
	// policy violation" with no policy named is a row nobody can act on.
	Watch  string `json:"watch,omitempty"`
	Policy string `json:"policy,omitempty"`
	Rule   string `json:"rule,omitempty"`

	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`

	CVE       string            `json:"cve,omitempty"`
	Component SecurityComponent `json:"component"`
	FixedIn   []string          `json:"fixedIn,omitempty"`

	Created  string `json:"created,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// SecurityDocumentRef says that a scanner body is held, without carrying it.
//
// A page draws a download button per image per kind - 157 images times four
// kinds - and reading the bodies to decide whether to draw a button would be
// hundreds of megabytes to render a row of icons.
type SecurityDocumentRef struct {
	// Kind is vulnerabilities | sbom | policy | malware.
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Provider names the scanner whose body this is, and is EMPTY on the
	// default entry for a kind.
	//
	// Two shapes on purpose: an unqualified entry per kind, which is what a
	// menu offers by default ("download the SBOM"), and one entry per scanner
	// underneath once two of them hold one. A reader who wants "the SBOM"
	// should not have to choose; a reader sending a vendor "what YOUR scanner
	// said" must be able to.
	Provider      string `json:"provider,omitempty"`
	ProviderLabel string `json:"providerLabel,omitempty"`
	// Available is false for a body the scanner was asked for and did not
	// have, which is worth saying: the alternative is a button that silently
	// downloads nothing.
	Available   bool   `json:"available"`
	ContentType string `json:"contentType,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	FetchedAt   string `json:"fetchedAt,omitempty"`
	Message     string `json:"message,omitempty"`
	// URL downloads it.
	URL string `json:"url,omitempty"`
}

// SecuritySourceCounts is one scanner's contribution.
//
// OnlyHere is the number the comparison exists for: advisories this scanner
// reported and no other did. Sent rather than derived, because the client that
// needs it most is the one rendering a summary without any findings loaded.
type SecuritySourceCounts struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`

	Counts     SecurityCounts `json:"counts"`
	UniqueCVEs int            `json:"uniqueCves"`
	OnlyHere   int            `json:"onlyHere"`
	Artifacts  int            `json:"artifacts"`
	// KEVs is the distinct known-exploited advisories this scanner reported.
	//
	// Its own number because it is the one that decides whether a second
	// scanner earned its licence: four thousand extra lows nobody will read
	// and two exploited CVEs nobody else saw look identical in OnlyHere.
	KEVs int `json:"kevs"`
	// KEVOnly is how many of OnlyHere are known-exploited.
	KEVOnly int `json:"kevOnly"`
	// Enriched is advisories another scanner also reported, where this one
	// supplied a fact the other lacked - a fix version, a description, a CVSS
	// vector, a KEV flag.
	//
	// The honest defence of a scanner whose OnlyHere is zero: it found nothing
	// new and it explained several thousand findings better.
	Enriched int `json:"enriched"`
	// Status says whether this scanner answered at all: ok | partial |
	// unavailable | disabled.
	Status string `json:"status,omitempty"`
	// Message explains a Status that is not ok, in words with an action.
	Message string `json:"message,omitempty"`
	// SyncedAt is when this scanner was last asked.
	SyncedAt string `json:"syncedAt,omitempty"`
	// Coverage is this scanner's own coverage of the release, which is NOT the
	// release's: Anchore may have analysed 140 of 157 images while Xray has
	// indexed all of them, and one coverage figure for both would be a lie
	// about whichever it did not describe.
	Coverage SecurityCoverage `json:"coverage"`
}

// SecuritySourceComparison is the set arithmetic between the scanners.
//
// # Why the server computes this
//
// Because the numbers have to agree with the export, the release summary and
// the stored per-scanner rows, and four implementations of "only in Anchore"
// is four chances for them not to. It is also the answer to the question a
// second scanner exists to make askable, and that answer should not depend on
// which page you are on.
//
// Absent on a single-scanner deployment, where there is nothing to compare.
type SecuritySourceComparison struct {
	// Providers is every scanner that answered.
	Providers []string `json:"providers"`
	// Shared is how many advisories every scanner agreed on.
	Shared int `json:"shared"`
	// SharedCVEs is those advisories, capped - a list somebody reads rather
	// than a set they download.
	SharedCVEs []string `json:"sharedCves,omitempty"`
	// OnlyIn maps a scanner to the advisories only it reported, capped the
	// same way. This is the list somebody opens the comparison for.
	OnlyIn map[string][]string `json:"onlyIn,omitempty"`
	// KEVOnlyIn maps a scanner to the KNOWN-EXPLOITED advisories only it
	// reported.
	//
	// Its own field because it is the finding that decides whether a scanner
	// stays switched on. Two thousand unique lows is a difference in feed
	// coverage; one unique KEV is a vulnerability being exploited that the
	// other scanner did not mention.
	KEVOnlyIn map[string][]string `json:"kevOnlyIn,omitempty"`
	// Truncated says a list was capped, so the interface can say "and 1,204
	// more" rather than implying it has them all.
	Truncated bool `json:"truncated,omitempty"`
}

// SecurityStatusNotFound is a report status the SCANNER never returns: it is
// the platform's own answer to "the image is not in the repository", which Xray
// reports with the same sentence it uses for one it has not indexed yet.
const SecurityStatusNotFound = "not_found"

// SecurityReport is one artifact's security state.
type SecurityReport struct {
	Artifact SecurityArtifact `json:"artifact"`

	// Status is scanned | not_scanned | not_found | unsupported | disabled |
	// unavailable.
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

	// Malware is what the scanner found that is not a vulnerability. Its own
	// list rather than findings with a flag, because it is read by a different
	// person for a different reason: a vulnerability count is a backlog, a
	// malware hit is a release that does not ship.
	Malware []SecurityFinding `json:"malware,omitempty"`
	// Violations is what the scanner's configured policies say - the gate,
	// rather than the backlog.
	Violations []SecurityViolation `json:"violations,omitempty"`
	// Documents are the scanner bodies held for this image, named and measured
	// but not carried.
	Documents []SecurityDocumentRef `json:"documents,omitempty"`

	ScannedAt   string `json:"scannedAt,omitempty"`
	RetrievedAt string `json:"retrievedAt,omitempty"`
	FromCache   bool   `json:"fromCache,omitempty"`
	// ScanURL links to the scanner's own view of this artifact, built from the
	// configured platform host and repository.
	ScanURL string `json:"scanUrl,omitempty"`
}

// SecuritySyncStatus is where a release's vulnerability sync has got to.
//
// Travels inside the security response rather than on a channel of its own,
// because the interface polls one endpoint while a sync runs and wants both the
// position and whatever is already stored in the same answer. A separate
// progress endpoint would mean two requests that can disagree.
type SecuritySyncStatus struct {
	// State is "" (never synced) | syncing | synced | failed.
	//
	// Four values, because "has this been synced" has two answers and needs
	// four, and three of them look identical to a timestamp.
	State string `json:"state"`
	Label string `json:"label"`
	Error string `json:"error,omitempty"`

	SyncedAt  string `json:"syncedAt,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	// HeartbeatAt is the last time the process running the sync said it was
	// alive. A sync beats every few seconds while it runs.
	HeartbeatAt string `json:"heartbeatAt,omitempty"`

	// Here is whether the Coordinator answering this request is the one running
	// the sync, and Stalled whether the claim has stopped beating.
	//
	// `state = syncing` says a sync was STARTED and nothing else. Without these
	// two a client cannot tell a sync running on another replica from a
	// Coordinator that was killed mid-sync - and it showed the second as the
	// first, on single-Coordinator deployments where that sentence is simply
	// untrue. Stalled means nothing is running: the release can be synced
	// again, and the next claim takes the row.
	Here    bool `json:"here,omitempty"`
	Stalled bool `json:"stalled,omitempty"`

	// CanSync is whether any configured repository of this product has a
	// scanner switched on. Reason says which knob turns one on when it does
	// not - an interface that only greyed the button out would leave the reader
	// with nowhere to go.
	CanSync bool   `json:"canSync"`
	Reason  string `json:"reason,omitempty"`

	// Repository is the CONFIGURED repository whose scanner answers, and
	// Provider the scanner. Shown because in a normal estate this is a JFrog
	// TARGET rather than the vendor registry the release came from, and a
	// reader wondering where the numbers come from deserves to be told.
	Repository string `json:"repository,omitempty"`
	Provider   string `json:"provider,omitempty"`

	// Stages and Notes are live progress, present only while this replica is
	// the one running the sync. Absent is normal - the work may be elsewhere -
	// and State remains authoritative.
	Stages []SecurityProgressStage `json:"stages,omitempty"`
	Notes  []string                `json:"notes,omitempty"`

	// Log is the transcript of the run: live while it is running here, and the
	// stored one from the last run otherwise.
	//
	// A sync is a job, and a job whose only durable output is one `error`
	// sentence is one nobody can ask anything about afterwards - which is
	// exactly the position a reader is in when a release comes back 4% scanned.
	Log []SecurityLogEntry `json:"log,omitempty"`
}

// SecurityLogEntry is one line of a sync's transcript.
type SecurityLogEntry struct {
	At string `json:"at,omitempty"`
	// Level is info | warning | error.
	Level   string `json:"level"`
	Message string `json:"message"`
	// Repeat counts identical consecutive lines, so a scanner that timed out
	// forty times costs one line and says forty.
	Repeat int `json:"repeat,omitempty"`
}

// SyncSecurityResponse is POST
// /api/v1/products/{product}/packages/{package}:syncSecurity.
// SyncSecurityRequest is the optional body of a sync.
//
// Empty is the ordinary case and means "bring this release up to date": images
// whose stored answer is inside the deployment's max age are reused rather than
// asked about again, which for a release sharing its images with one synced
// recently is most of the release and most of the time.
type SyncSecurityRequest struct {
	// Force asks the scanner about every image regardless of what is held.
	//
	// For the person who has a reason to distrust what is stored - a scanner
	// that was misconfigured when the last sync ran, a policy that has since
	// changed. It is minutes of somebody else's scanner, so it is asked for
	// rather than assumed.
	Force bool `json:"force,omitempty"`

	// Provider narrows the sync to one scanner - "jfrog-xray", "anchore".
	//
	// Empty syncs every scanner configured for the release, which is what the
	// main Sync button does. Naming one is for the case the scanners' very
	// different speeds create: a reader whose Anchore is mid-analysis should be
	// able to refresh Xray without waiting ten minutes for the other half.
	//
	// A name the release has no scanner for is ignored rather than refused - a
	// stale browser tab must not turn a sync into an error, and syncing
	// everything is never the wrong answer to "sync this".
	Provider string `json:"provider,omitempty"`
}

type SyncSecurityResponse struct {
	Product string `json:"product"`
	Package string `json:"package"`
	// Status is started | already_running. The second is not a failure: the
	// thing the caller wanted is happening.
	Status  string `json:"status"`
	Started bool   `json:"started"`
	// Artifacts is how many the sync will ask about, so the interface can say
	// what it has taken on.
	Artifacts int                `json:"artifacts"`
	Sync      SecuritySyncStatus `json:"sync"`
}

// CancelSecuritySyncResponse is POST
// /api/v1/products/{product}/packages/{package}:cancelSecuritySync.
type CancelSecuritySyncResponse struct {
	Product string `json:"product"`
	Package string `json:"package"`
	// Stopped is false when there was nothing to stop - the sync finished
	// between the reader deciding to stop it and the request arriving. Not a
	// failure, and the sync below says what the release's state actually is.
	Stopped bool               `json:"stopped"`
	Sync    SecuritySyncStatus `json:"sync"`
}

// PackageSecurityResponse is
// GET /api/v1/products/{product}/packages/{package}/security.
//
// Served entirely from storage. Nothing behind this route queries a scanner,
// which is what makes it cheap enough to poll while a sync runs and cheap
// enough for a listing to carry the same numbers.
type PackageSecurityResponse struct {
	Product string `json:"product"`
	Package string `json:"package"`

	// Sync is where the vulnerability sync has got to, and is the field a
	// client reads first: everything below it is meaningless until a sync has
	// happened.
	Sync SecuritySyncStatus `json:"sync"`

	// Provider names the scanner; Enabled says whether one is configured for
	// the repository this release lands in.
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
	Counts          SecurityCounts   `json:"counts"`
	UniqueCounts    SecurityCounts   `json:"uniqueCounts"`
	UniqueCVECounts SecurityCounts   `json:"uniqueCveCounts"`
	Coverage        SecurityCoverage `json:"coverage"`

	Reports   []SecurityReport `json:"reports"`
	Providers []string         `json:"providers,omitempty"`

	// ScannedAt is when the SCANNER produced the oldest contributing result,
	// SyncedAt when this platform last asked. Two different facts, and the gap
	// between them is how stale the answer is allowed to look.
	ScannedAt string `json:"scannedAt,omitempty"`
	SyncedAt  string `json:"syncedAt,omitempty"`
	// Freshness is when these answers stop being current, and whether they
	// already have. On the wire so the rule lives in one place rather than
	// being guessed at by every page that draws a date.
	Freshness SecurityFreshness `json:"freshness"`
	// Fingerprint is the ETag body. Exposed so a client can tell an unchanged
	// re-read from a changed one without diffing megabytes.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Detail says whether Reports carry findings or counts alone.
	Detail bool `json:"detail"`
	// DistinctTotal collapses the same (CVE, component) PAIR across artifacts;
	// DistinctCVEs collapses the advisory alone.
	//
	// Both, and named for what they count. The interface printed the first
	// under the label "unique CVEs", and a reader hearing that counts the
	// second: openssl and libssl3 carrying one advisory are two things to
	// upgrade and one advisory to read.
	DistinctTotal int `json:"distinctTotal"`
	DistinctCVEs  int `json:"distinctCves"`

	// Sources is one entry per scanner that contributed, present only where
	// more than one did. A segmented control with a single position is a
	// control that should not be drawn.
	Sources []SecuritySourceCounts `json:"sources,omitempty"`
	// SourceComparison is what each scanner found that the others did not.
	// Absent with fewer than two scanners.
	SourceComparison *SecuritySourceComparison `json:"sourceComparison,omitempty"`

	// KEVs is the DISTINCT known-exploited advisories in this release, and
	// KEVFixable how many have a fix.
	//
	// Distinct rather than per-occurrence, because it is the number the page
	// prints: a KEV in a base image carried by forty images is one advisory to
	// chase. Counts.KEV carries the per-occurrence figure.
	KEVs       int `json:"kevs"`
	KEVFixable int `json:"kevFixable"`
	// KEVSeverity grades the exploited advisories.
	KEVSeverity SecuritySeverityCounts `json:"kevSeverity"`
	// KEVCapable says at least one scanner that answered has an
	// exploited-vulnerability feed at all.
	//
	// # Why "0 known-exploited" needs this to be readable
	//
	// Because zero means two different things. On a deployment running a
	// scanner with a KEV feed, zero is a genuine and very good result. On one
	// running only a scanner without one, zero is "nobody checked" - and
	// drawing that as a clean bill of health is the same failure the whole
	// scanned/not-scanned distinction exists to prevent, one level up.
	KEVCapable bool `json:"kevCapable"`
}

// PackageSecuritySummary is a release's vulnerability counts, for a listing.
//
// # Why this is on the package rather than fetched per row
//
// Because a listing renders twenty of them. Fetching each one separately meant
// twenty scanner-backed reads to draw one column, which is why that column once
// shipped behind a toggle - the shape of a design apologising for itself. These
// come from a table a sync wrote, so the column costs one join and is always on.
//
// Nil means "this release has never been synced", which is a different fact
// from zero vulnerabilities and must not render as one.
type PackageSecuritySummary struct {
	// State is "" (never synced) | syncing | synced | failed.
	State string `json:"state"`
	Label string `json:"label"`
	// Stalled is a `syncing` row whose claim has stopped beating: the process
	// that started it is gone and nothing is running. Without this a listing
	// shows a spinner on a release nobody is syncing, for as long as it takes
	// the stale sweep to notice.
	Stalled bool `json:"stalled,omitempty"`

	Counts SecurityCounts `json:"counts"`
	// DistinctTotal collapses the same (CVE, component) pair across artifacts;
	// DistinctCVEs collapses the advisory alone.
	DistinctTotal   int            `json:"distinctTotal"`
	DistinctCVEs    int            `json:"distinctCves"`
	DistinctCounts  SecurityCounts `json:"distinctCounts"`
	UniqueCVECounts SecurityCounts `json:"uniqueCveCounts"`
	// KEVs is the distinct known-exploited advisories in this release, and
	// KEVFixable how many have a fix. The listing's most important cell.
	KEVs       int `json:"kevs"`
	KEVFixable int `json:"kevFixable"`
	// Providers names every scanner that contributed to these numbers, so a
	// listing can say "Xray + Anchore" rather than one of the two.
	Providers []string `json:"providers,omitempty"`
	// Complete is whether every scannable artifact has a result. False means
	// the counts cover only part of the release.
	Complete bool `json:"complete"`
	// Scanned and Scannable are what "0 vulnerabilities" actually means.
	//
	// Zero of zero is "nobody looked" and must never render as "none found";
	// zero of fourteen is a clean release. A listing cell cannot tell them
	// apart from the counts alone, which is why both travel with them.
	Scanned   int `json:"scanned"`
	Scannable int `json:"scannable"`

	SyncedAt string `json:"syncedAt,omitempty"`
	Error    string `json:"error,omitempty"`
	// CanSync is whether a scanner is configured for this release at all, so a
	// listing knows whether to offer the action.
	CanSync bool   `json:"canSync"`
	Reason  string `json:"reason,omitempty"`
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
	Label           string           `json:"label"`
	Package         string           `json:"package,omitempty"`
	Tag             string           `json:"tag,omitempty"`
	Digest          string           `json:"digest,omitempty"`
	Repository      string           `json:"repository,omitempty"`
	Provider        string           `json:"provider,omitempty"`
	Enabled         bool             `json:"enabled"`
	Counts          SecurityCounts   `json:"counts"`
	UniqueCVECounts SecurityCounts   `json:"uniqueCveCounts"`
	Coverage        SecurityCoverage `json:"coverage"`
	ScannedAt       string           `json:"scannedAt,omitempty"`
	// Sync is this end's sync state. A comparison against a release nobody
	// synced is inconclusive, and this is what lets the interface offer the
	// sync rather than just reporting the verdict.
	Sync SecuritySyncStatus `json:"sync"`
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

	// Changes is the classified findings, worst first: what became more
	// severe, what is new, what left, what was resolved, and - last - what
	// carried over unchanged.
	//
	// It can be a PREFIX of the whole set. ChangesTotal is the whole set's
	// size, and when it is larger than len(Changes) the rows that were left
	// out are the least important ones in that order, which in practice means
	// findings that are in both releases and identical in both. A client must
	// take its counts from the Introduced/Resolved/Unchanged totals above and
	// never from len(Changes), and should say plainly that the list is
	// shortened - see ChangesTotal.
	Changes []SecurityChange `json:"changes"`
	// ChangesTotal is how many classified findings there are in all, whether
	// or not they are listed in Changes.
	ChangesTotal    int                     `json:"changesTotal"`
	Artifacts       []SecurityArtifactDelta `json:"artifacts"`
	ArtifactSummary SecurityArtifactSummary `json:"artifactSummary"`

	Fingerprint string `json:"fingerprint,omitempty"`
	RetrievedAt string `json:"retrievedAt,omitempty"`
}

// SecurityFreshness is the deployment's rule about how old an answer may be.
//
// # Why the ANSWER carries the policy
//
// Because otherwise every page draws its own line. The rule is one number in
// one configuration file, and a client that hardcoded "a week is old" would be
// wrong in every deployment that decided otherwise - silently, and in the
// direction of telling somebody stale data is current.
//
// Nothing here expires or refetches. Past MaxAgeSeconds an answer is still
// served, still counted and still exported; it is presented with its age in
// words and a Refresh beside it, and the decision stays with the person.
type SecurityFreshness struct {
	// MaxAgeSeconds is how old a vulnerability answer may be before it is
	// shown as out of date. Zero means never.
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
	// SBOMMaxAgeSeconds is the same for the component inventory, and is
	// normally zero: an SBOM describes one immutable set of bytes, so it
	// cannot go out of date without the digest changing.
	SBOMMaxAgeSeconds int `json:"sbomMaxAgeSeconds,omitempty"`
	// Stale says the release's own answer is past MaxAgeSeconds. Computed here
	// rather than in the client so "how old is too old" is answered once.
	Stale bool `json:"stale,omitempty"`
	// StaleAt is when it will be, or was. Empty when nothing ever goes stale.
	StaleAt string `json:"staleAt,omitempty"`
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
	// Refresh is accepted and ignored.
	//
	// It bypassed the cache when a comparison queried the scanner. Both sides
	// now come from storage, and re-reading them is what a sync is for. Kept on
	// the wire so an older client's request is still a valid request.
	Refresh bool `json:"refresh,omitempty"`
	// ProgressToken is accepted and ignored, for the same reason: a comparison
	// over stored data has no position worth reporting.
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
	// KEV says this finding is known to be exploited, so a search result
	// carries the same badge the release page does - and so the first row of a
	// truncated result is the one that matters.
	KEV     bool   `json:"kev,omitempty"`
	Summary string `json:"summary,omitempty"`

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
	// KEVOnly says the search was narrowed to known-exploited vulnerabilities.
	KEVOnly bool `json:"kevOnly,omitempty"`
	// Provider says the search was narrowed to one scanner. Empty searched
	// every scanner's findings, which is the right default: a reader hunting a
	// CVE wants it found, not attributed.
	Provider string `json:"provider,omitempty"`

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
