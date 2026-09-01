package security

import (
	"context"
	"strings"
	"time"
)

// Documents: what a scanner says about an artifact that is not a list of
// vulnerabilities.
//
// # Why these are one concept and not three features
//
// An SBOM, a set of policy violations and a malware verdict arrive from
// different endpoints, mean different things and are rendered on different
// tabs. What they have in common is the only thing this package needs: each is
// a BODY the scanner produced about one artifact, which somebody eventually
// wants to read, keep, and hand to a customer unaltered.
//
// Modelling them as one kind of thing is what makes the storage, the retention,
// the eviction, the export tree and the download route one implementation
// instead of four. The alternative - an sbom table, a violations table, a
// malware table, three cache policies and three export writers - is the version
// of this that never gets a fourth document type added to it.
//
// # Why the RAW body is kept, when everything else here is normalized
//
// Because normalization is lossy on purpose, and the person asking for an SBOM
// is asking for the SBOM. A CycloneDX document regenerated from this platform's
// component model would be a different document with the same component names
// in it, and handing that to somebody's compliance team is worse than handing
// them nothing.

// DocumentKind names one body about one artifact.
type DocumentKind string

const (
	// DocumentVulnerabilities is the scanner's own vulnerability response, as
	// it sent it. The normalized findings are derived from this; keeping the
	// original is what makes an export shareable and a disagreement between
	// this platform and the scanner's UI resolvable.
	DocumentVulnerabilities DocumentKind = "vulnerabilities"
	// DocumentSBOM is the component inventory, CycloneDX or SPDX as the scanner
	// produces it.
	DocumentSBOM DocumentKind = "sbom"
	// DocumentPolicy is the violations of the watches and policies configured
	// on the scanner - which is a different question from "what is wrong with
	// this image", and the one a release gate actually asks.
	DocumentPolicy DocumentKind = "policy"
	// DocumentMalware is what the scanner found that is not a vulnerability at
	// all: a malicious package, a known-bad component.
	DocumentMalware DocumentKind = "malware"
)

// AllDocumentKinds is every kind, in the order an export lays them out.
//
// Vulnerabilities first because that is what somebody opened the export for;
// malware second because it is the one that stops a release; policy third;
// SBOM last because it is the largest and the least often read.
var AllDocumentKinds = []DocumentKind{
	DocumentVulnerabilities, DocumentMalware, DocumentPolicy, DocumentSBOM,
}

// ParseDocumentKind maps a request parameter onto a kind.
func ParseDocumentKind(s string) (DocumentKind, bool) {
	switch DocumentKind(strings.ToLower(strings.TrimSpace(s))) {
	case DocumentVulnerabilities:
		return DocumentVulnerabilities, true
	case DocumentSBOM:
		return DocumentSBOM, true
	case DocumentPolicy:
		return DocumentPolicy, true
	case DocumentMalware:
		return DocumentMalware, true
	default:
		return "", false
	}
}

// DocumentKindsFrom maps configured strings onto kinds, dropping what it does
// not recognise.
//
// Dropping rather than refusing, for the reason the configuration says: a typo
// in a list of optional extras must not stop a Coordinator starting. The
// failure that causes is an outage; the failure it prevents is a missing tab.
func DocumentKindsFrom(names []string) []DocumentKind {
	out := make([]DocumentKind, 0, len(names))
	seen := map[DocumentKind]bool{}
	for _, name := range names {
		kind, ok := ParseDocumentKind(name)
		// The vulnerability body is captured from the scan itself and is never
		// a separate request, so naming it here would cost a round trip per
		// image to fetch what is already in hand.
		if !ok || kind == DocumentVulnerabilities || seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	return out
}

// Label is the kind in the words the interface shows.
func (k DocumentKind) Label() string {
	switch k {
	case DocumentVulnerabilities:
		return "Vulnerabilities"
	case DocumentSBOM:
		return "SBOM"
	case DocumentPolicy:
		return "Policy violations"
	case DocumentMalware:
		return "Malware"
	default:
		return string(k)
	}
}

// Folder is the directory this kind occupies in a ZIP export.
func (k DocumentKind) Folder() string {
	switch k {
	case DocumentVulnerabilities:
		return "vulnerabilities"
	case DocumentSBOM:
		return "sbom"
	case DocumentPolicy:
		return "policy"
	case DocumentMalware:
		return "malware"
	default:
		return string(k)
	}
}

// Document is one body about one artifact.
type Document struct {
	Artifact ArtifactRef  `json:"artifact"`
	Kind     DocumentKind `json:"kind"`
	Provider string       `json:"provider"`

	// ContentType is what the scanner sent, so a download can be served with
	// the right header and a ZIP entry can be given the right extension.
	ContentType string `json:"contentType,omitempty"`
	// Payload is the body, unaltered. Nil when the scanner had nothing to give,
	// which is a fact and not a failure - see Message.
	Payload []byte `json:"-"`

	// Message says why there is no payload, in words with an action in them.
	// An SBOM that "is not available on this Xray version" and one that "has
	// not been generated for this image yet" send a reader to two different
	// places.
	Message string `json:"message,omitempty"`

	// Available distinguishes "asked and there is none" from "never asked".
	// Both have a nil payload, and only one of them is worth retrying.
	Available bool `json:"available"`

	FetchedAt time.Time `json:"fetchedAt"`
	// SourceBytes is the decoded size, carried so a caller deciding whether to
	// include a 400 MB SBOM in a download does not have to fetch it to find out.
	SourceBytes int `json:"sourceBytes,omitempty"`
	// Fingerprint is over the payload, so an unchanged re-fetch is recognisable
	// without a byte comparison.
	Fingerprint string `json:"fingerprint,omitempty"`

	// Violations and Findings are the SAME body in the platform's own terms,
	// where the provider was able to produce them.
	//
	// # Why the normalized form rides along with the raw one
	//
	// Because they come from one request and are wanted by two readers. The raw
	// body is what an export ships; the normalized rows are what the policy and
	// malware tables render. Fetching twice to serve both would double the
	// requests for one answer, and parsing the stored raw body at render time
	// would put Xray's JSON shape in the handler - which is the exact thing the
	// provider boundary exists to prevent.
	//
	// Neither is authoritative over the other: the raw body is what the scanner
	// said, and these are what this platform understood of it.
	Violations []Violation `json:"violations,omitempty"`
	Findings   []Finding   `json:"findings,omitempty"`
}

// Extension is the file suffix for this document's content type.
//
// Content type first, kind second. An SBOM arrives as JSON from one scanner and
// as XML from another, and a `.json` on an XML body is a file somebody's tool
// refuses with a parse error rather than a helpful one.
func (d Document) Extension() string {
	switch {
	case strings.Contains(d.ContentType, "xml"):
		return ".xml"
	case strings.Contains(d.ContentType, "json"):
		return ".json"
	case strings.Contains(d.ContentType, "text"):
		return ".txt"
	default:
		return ".json"
	}
}

// Violation is one breach of a configured policy.
//
// # Why this is not a Finding with a policy field
//
// Because they answer different questions and a release gate reads only the
// second. A finding is "this image contains CVE-2026-31789". A violation is
// "your Production watch forbids critical fixable issues and this image has
// four" - it exists because somebody wrote a rule, it disappears when the rule
// changes, and it can be raised against a licence or an operational risk with
// no CVE anywhere near it. Folding the two together produced a vulnerabilities
// table with rows that were not vulnerabilities.
type Violation struct {
	// ID is the scanner's identifier for the violation.
	ID string `json:"id,omitempty"`
	// Type is security | license | operational_risk, as the scanner grades it.
	Type     string   `json:"type,omitempty"`
	Severity Severity `json:"severity"`

	// Watch and Policy are the rule's address: which watch, which policy inside
	// it, which rule inside that. All three, because "a policy violation" with
	// no policy named is a row nobody can act on.
	Watch  string `json:"watch,omitempty"`
	Policy string `json:"policy,omitempty"`
	Rule   string `json:"rule,omitempty"`

	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`

	// CVE is present when the violation is about a vulnerability, so a reader
	// can jump from the gate to the finding that tripped it.
	CVE       string    `json:"cve,omitempty"`
	Component Component `json:"component"`

	FixedIn []string `json:"fixedIn,omitempty"`

	Created  *time.Time `json:"created,omitempty"`
	Provider string     `json:"provider"`
}

// Key identifies a violation within one artifact.
func (v Violation) Key() string {
	id := v.ID
	if id == "" {
		id = v.CVE
	}
	return id + "|" + v.Watch + "|" + v.Component.ComponentKey()
}

// DocumentProvider is a scanner that can produce more than findings.
//
// # Why this is separate from Provider rather than more methods on it
//
// Because a scanner that answers "what is wrong with this image" and one that
// also exports SBOMs are both legitimate, and a Provider interface carrying
// four methods three implementations must stub is an interface that lies about
// what its implementations do. A caller asks with a type assertion and has a
// sentence ready for the answer "this scanner does not do that".
type DocumentProvider interface {
	Provider
	// Documents retrieves the requested kinds for the requested artifacts.
	//
	// A per-artifact or per-kind failure is a Document with no payload and a
	// Message, never an error: one image whose SBOM would not generate must not
	// lose the other hundred and fifty-six.
	Documents(
		ctx context.Context, refs []ArtifactRef, kinds []DocumentKind, opts ScanOptions,
	) ([]Document, error)
}

// DocumentStore is where fetched documents are kept.
//
// Separate from Cache because the two have different lifetimes and very
// different sizes: a document is megabytes and is fetched when somebody asks
// for an export, where a summary is bytes and is written by every sync.
type DocumentStore interface {
	// SaveDocuments records what was fetched.
	SaveDocuments(ctx context.Context, scope Scope, docs []Document, ttl time.Duration) error
	// LoadDocuments returns stored documents for these artifacts, keyed by
	// ArtifactRef.Ref() and then by kind.
	LoadDocuments(
		ctx context.Context, scope Scope, refs []ArtifactRef, kinds []DocumentKind,
	) (map[string]map[DocumentKind]Document, error)
}
