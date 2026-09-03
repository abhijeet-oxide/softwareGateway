package security

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is what is known about one artifact's security state.
//
// Five states, and the distinction between them is the single most important
// thing this package gets right. "No vulnerabilities were found" and "nobody
// looked" render identically as an empty list, and a release manager reading
// the second as the first ships an unscanned image believing it is clean.
// Everything downstream - the badge, the comparison verdict, the export - keys
// off this field rather than off len(Findings).
type Status string

const (
	// StatusScanned means the provider has results for this artifact. An empty
	// finding list under this status is a genuine clean result.
	StatusScanned Status = "scanned"
	// StatusNotScanned means the provider knows the artifact and has not
	// indexed it. Xray returns this for artifacts outside an indexed
	// repository, and for ones whose scan has not finished.
	StatusNotScanned Status = "not_scanned"
	// StatusUnsupported means the provider cannot scan this kind of artifact at
	// all - a signature, an attestation, a plain file layer. Not a problem to
	// fix, and counting it as unscanned coverage would permanently pin every
	// release below 100%.
	StatusUnsupported Status = "unsupported"
	// StatusDisabled means Xray is switched off for the repository this
	// artifact lives in. A configuration fact, not a failure.
	StatusDisabled Status = "disabled"
	// StatusUnavailable means we asked and could not find out - the scanner was
	// down, refused us, or timed out. Message carries what happened.
	StatusUnavailable Status = "unavailable"
)

// Conclusive reports whether this status permits a security judgement.
//
// Only a completed scan does. Everything else is an absence of information, and
// a comparison resting on one of them is reported inconclusive rather than
// guessed at.
func (s Status) Conclusive() bool { return s == StatusScanned }

// Counts reports whether this status participates in coverage arithmetic.
//
// Unsupported artifacts do not: a cosign signature is not a thing Xray declines
// to scan, it is a thing there is nothing to scan in.
func (s Status) Counts() bool { return s != StatusUnsupported }

// Label is the status in the words the interface shows.
func (s Status) Label() string {
	switch s {
	case StatusScanned:
		return "Scanned"
	case StatusNotScanned:
		return "Not scanned"
	case StatusUnsupported:
		return "Not applicable"
	case StatusDisabled:
		return "Xray disabled"
	case StatusUnavailable:
		return "Unavailable"
	default:
		return "Unknown"
	}
}

// Component is the package a finding is against.
//
// The identity is (Type, Name, Version) and it is normalized by the provider,
// not here: only the provider knows that Xray spells a Debian package
// "deb://openssl:1.1.1n-0+deb11u3" and an npm one "npm://lodash:4.17.20".
// What the core needs is that the same package in two releases produces the
// same ID, because that is what makes a comparison a comparison rather than a
// pair of lists.
type Component struct {
	// ID is the stable identifier - "deb://openssl:1.1.1n". Compared across
	// releases, so it must not contain anything release-specific.
	ID string `json:"id"`
	// Name and Version are the human halves of the same fact.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Type is the ecosystem: deb, rpm, npm, go, maven, pypi, generic.
	Type string `json:"type,omitempty"`
	// Path is where inside the artifact the component was found, when the
	// scanner says. Not part of the identity - the same package at two paths in
	// one image is one package with one CVE, and counting it twice inflates
	// every number on the page.
	Path string `json:"path,omitempty"`
}

// ComponentKey is the identity used for cross-release alignment.
func (c Component) ComponentKey() string {
	if c.ID != "" {
		return c.ID
	}
	if c.Type != "" {
		return c.Type + "://" + c.Name
	}
	return c.Name
}

// Display is the component as a person names it.
func (c Component) Display() string {
	switch {
	case c.Name == "":
		return c.ID
	case c.Version == "":
		return c.Name
	default:
		return c.Name + " " + c.Version
	}
}

// EPSS is the Exploit Prediction Scoring System's estimate for a vulnerability.
//
// Two numbers rather than one, because they answer different questions and the
// second is the readable one: Score is the modelled probability of exploitation
// in the next thirty days (0.00042 means what it looks like), and Percentile is
// where that sits among every scored CVE. "0.00042" tells almost nobody
// anything; "in the bottom 12%" tells everybody something.
type EPSS struct {
	Score      float64 `json:"score"`
	Percentile float64 `json:"percentile,omitempty"`
}

// Observation is one scanner's grading of one finding, kept with its source.
//
// See Finding.Observations for why the disagreements are preserved rather than
// resolved. Nothing sorts or counts on these; they are what a reader opens when
// two sources put a different word on the same CVE.
type Observation struct {
	// Provider is the scanner this came from - "anchore", "jfrog-xray".
	Provider string `json:"provider"`
	// Source is the scanner's own name for the data set within it: "nvd",
	// "vendor", the feed group. Empty where the scanner reports one grade and
	// does not say where it got it.
	Source   string   `json:"source,omitempty"`
	Severity Severity `json:"severity,omitempty"`
	// Score and Vector are the CVSS pair, where this source supplied one.
	Score  float64 `json:"score,omitempty"`
	Vector string  `json:"vector,omitempty"`
}

// ObservationKey identifies one observation, so merging two reports of the same
// grading does not print it twice.
func (o Observation) ObservationKey() string {
	return o.Provider + "|" + o.Source + "|" + string(o.Severity) + "|" + o.Vector
}

// Finding is one vulnerability, against one component, in one artifact.
//
// Flat rather than nested under the component, because every question the
// platform answers - "which images have CVE-2024-3094", "what does this
// package expose me to", "what changed between releases" - is a different
// grouping of the same rows, and a shape that privileges one grouping makes the
// other three expensive.
type Finding struct {
	// CVE is the public identifier when there is one, normalized to upper case.
	// Empty for a scanner-private issue; ID is then the only handle.
	CVE string `json:"cve,omitempty"`
	// ID is the scanner's own identifier - "XRAY-123456". Kept because it is
	// what a support case with the vendor is opened against, and because
	// findings without a CVE still need an identity.
	ID string `json:"id,omitempty"`

	Severity Severity `json:"severity"`

	// Summary is one line. Description is the paragraph, when there is one.
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`

	Component Component `json:"component"`

	// FixedIn lists versions that resolve this finding, cleanest first.
	// Fixable is not len(FixedIn) != 0: a scanner may report fixability without
	// naming a version, and the two facts are asked about separately.
	FixedIn []string `json:"fixedIn,omitempty"`
	Fixable bool     `json:"fixable"`

	// CVSSScore and CVSSVector are carried when the scanner supplies them, for
	// the users who work in those terms. Nothing in this package sorts or
	// compares on them - Severity does that.
	CVSSScore  float64 `json:"cvssScore,omitempty"`
	CVSSVector string  `json:"cvssVector,omitempty"`

	// KEV says this vulnerability is on a known-exploited catalogue - CISA's,
	// in every scanner that reports one today.
	//
	// # Why this outranks severity everywhere it appears
	//
	// Because it is not a grade, it is an observation. A critical is somebody's
	// judgement that a vulnerability WOULD be bad to exploit; a KEV is a record
	// that somebody HAS exploited it. A release with nine hundred criticals and
	// four KEVs has four things to do first, and every sort order, every default
	// filter and every badge in this platform says so.
	//
	// False is not "not exploited". It is "no scanner that answered for this
	// finding said so", which on a scanner with no KEV feed is every finding -
	// hence KEVSource, which says who claimed it.
	KEV bool `json:"kev,omitempty"`
	// KEVSource names the scanner whose data carried the KEV flag, so a reader
	// who wants to check the claim knows where it came from. Empty when KEV is
	// false.
	KEVSource string `json:"kevSource,omitempty"`

	// EPSS is the exploit-prediction score and its percentile, where a scanner
	// supplies them. Carried, never sorted on: it is a probability and the
	// people who use it know what to do with it, while the people who do not
	// must not have a list reordered by a number they cannot read.
	EPSS *EPSS `json:"epss,omitempty"`

	// WillNotFix says the vendor has declined to fix this in the affected
	// stream.
	//
	// Distinct from `Fixable == false`, and the distinction is the point: no
	// fixed version means nobody has shipped one YET, and will-not-fix means
	// nobody is going to. The first is a wait, the second is a decision to
	// mitigate or accept, and a table that renders them identically sends
	// somebody back to the same vendor every month for an answer they already
	// have.
	WillNotFix bool `json:"willNotFix,omitempty"`

	// Observations is every severity and score this finding was reported with,
	// with the source that reported each.
	//
	// # Why the disagreements are kept rather than resolved
	//
	// Because they are not noise. Anchore reports a vendor severity and an NVD
	// severity for one CVE and they routinely differ - Debian grades an OpenSSL
	// issue low that NVD grades critical, and both are right about different
	// questions. Xray reports a third. Collapsing them to one number loses the
	// only evidence a reader has for which to believe, and the reader is the
	// one who knows whether their deployment looks like the vendor's assumption.
	//
	// Severity above is the REPORTED grade - the one this platform sorts,
	// counts and compares on - and it is chosen by a documented rule (worst
	// wins, see MergeFindings). This is the audit trail behind it.
	Observations []Observation `json:"observations,omitempty"`

	// References are advisory URLs.
	References []string `json:"references,omitempty"`

	// Published is when the advisory was issued, not when we saw it.
	Published *time.Time `json:"published,omitempty"`

	// Provider names the scanner that reported this - "jfrog-xray". Present on
	// every finding rather than only on the report, because findings are
	// regrouped constantly (by CVE, by package, by release) and a row that has
	// travelled away from its report must still be able to say where it came
	// from.
	Provider string `json:"provider"`

	// Policy names the Xray watch or policy that flagged this, when the scanner
	// reports one. Informational.
	Policy string `json:"policy,omitempty"`

	// Sources names every scanner that reported this finding, sorted.
	//
	// Provider says which one this ROW came from; Sources says who agrees. They
	// are different questions and the second only has an interesting answer once
	// a second scanner exists - which is exactly why it is here now. A field
	// added the day Anchore is switched on is a field every stored row, every
	// export column and every table lacks for the releases synced before it.
	//
	// A single-source deployment carries one entry, and the interface hides a
	// column that can only say "JFrog Xray" on every row.
	Sources []string `json:"sources,omitempty"`
}

// SourceSet is Sources, or Provider when nothing has merged this finding yet.
//
// Never let a caller read Sources directly and get an empty slice for a finding
// that plainly came from somewhere: a filter reading "reported by nothing" would
// hide every row on a single-scanner deployment.
func (f Finding) SourceSet() []string {
	if len(f.Sources) > 0 {
		return f.Sources
	}
	if f.Provider != "" {
		return []string{f.Provider}
	}
	return nil
}

// Key identifies a finding within one artifact: which problem, in which
// package. Severity is deliberately NOT part of it - a finding whose severity
// was re-graded between releases is the same finding, and that re-grading is
// exactly what the comparison exists to report.
func (f Finding) Key() string {
	return f.Identifier() + "|" + f.Component.ComponentKey()
}

// StorageKey identifies a finding within one artifact for STORAGE and for
// COUNTING, and it is deliberately not Key().
//
// # The number that did not add up
//
// A release reported 90,808 findings on its listing row and 86,085 on its own
// security tab, from the same sync. The listing quotes what the sync summed in
// memory; the tab counts the rows that reached `security_findings`, whose
// unique key was (scan, CVE, issue, component id). Component id carries no
// VERSION - that is Key()'s whole point, and the right decision for comparing
// two releases - so an image holding two builds of one package (libcrypto3 at
// 3.1.4-r2 in one layer and 3.5.5-r0 in another, which a multi-stage image does
// routinely) wrote one row where the sum counted two. Four and a half thousand
// findings disappeared between the two pages, and neither page was wrong about
// its own arithmetic.
//
// Two identities, then, for two questions. Key() answers "is this the same
// problem as the one in the other release", and must not carry the version.
// StorageKey answers "is this the same ROW", and must, or the count and the
// table disagree forever.
func (f Finding) StorageKey() string {
	return f.Identifier() + "|" + f.Component.ComponentKey() + "|" + f.Component.Version
}

// Identifier is the CVE where there is one, and the scanner's id otherwise.
func (f Finding) Identifier() string {
	if f.CVE != "" {
		return f.CVE
	}
	return f.ID
}

// ArtifactRef identifies something that can be scanned, in terms the core
// already uses. No JFrog path, no Xray component id: the provider translates.
type ArtifactRef struct {
	// Name is the artifact's own name within its release - "cfx-main". This is
	// the key two releases are aligned on, because digests and tags both change
	// between releases and the name is what a person means by "the same image".
	Name string `json:"name"`
	// Tag and Digest are the concrete identity within one release.
	Tag    string `json:"tag,omitempty"`
	Digest string `json:"digest"`
	// Repository is the full path the artifact lives at, source-side.
	Repository string `json:"repository,omitempty"`
	// Registry is the host, which is what decides WHICH provider can answer for
	// this artifact.
	Registry string `json:"registry,omitempty"`
	// MediaType and Kind say what it is. Kind is the core's vocabulary -
	// image, chart, index, file, signature - and is what decides whether a
	// provider will find anything to scan.
	MediaType string `json:"mediaType,omitempty"`
	Kind      string `json:"kind,omitempty"`
	// SizeBytes is carried for display only.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Platform is os/arch for a per-platform image within an index.
	Platform string `json:"platform,omitempty"`
}

// ArtifactKey is the identity used for CROSS-RELEASE alignment: the name, or
// the repository path when a release does not name its artifacts.
//
// Not the digest. Two releases of the same product hold the same images at
// different digests - that IS the release - so aligning on digest would report
// every artifact as removed and every artifact as added, which is a diff that
// contains no information.
func (a ArtifactRef) ArtifactKey() string {
	switch {
	case a.Name != "":
		return a.Name
	case a.Repository != "":
		return a.Repository
	default:
		return a.Digest
	}
}

// Ref is the concrete identity WITHIN one release, used for caching and for
// telling an upgraded artifact from an unchanged one.
func (a ArtifactRef) Ref() string {
	if a.Digest != "" {
		return a.Digest
	}
	if a.Repository != "" && a.Tag != "" {
		return a.Repository + ":" + a.Tag
	}
	return a.ArtifactKey()
}

// Display is the artifact as the interface names it - "cfx-main:25.7.2131".
func (a ArtifactRef) Display() string {
	name := a.ArtifactKey()
	if a.Tag != "" {
		return name + ":" + a.Tag
	}
	if a.Digest != "" && len(a.Digest) > 19 {
		return name + "@" + a.Digest[:19]
	}
	return name
}

// Report is one artifact's normalized security state.
//
// Status first, findings second. A consumer that reads Findings without reading
// Status has written the bug this type exists to prevent.
type Report struct {
	Artifact ArtifactRef `json:"artifact"`
	Status   Status      `json:"status"`
	// Provider names the scanner. Set even for a disabled or unavailable
	// report, so the interface can say WHICH scanner is off.
	Provider string `json:"provider"`
	// Message explains a non-scanned status in words a person can act on.
	Message string `json:"message,omitempty"`

	Findings []Finding `json:"findings,omitempty"`
	Counts   Counts    `json:"counts"`

	// Malware is what the scanner found that is not a vulnerability: a
	// malicious package, a known-bad component.
	//
	// Its own list rather than findings with a flag, because it is read by a
	// different person for a different reason. A vulnerability count is a
	// backlog; a malware hit is a release that does not ship tonight, and
	// burying one row among ninety thousand is how it gets shipped anyway.
	Malware []Finding `json:"malware,omitempty"`
	// Violations is what the scanner's configured policies say about this
	// artifact - the gate, rather than the backlog.
	Violations []Violation `json:"violations,omitempty"`

	// Documents says which scanner bodies are held for this artifact, without
	// carrying any of them. A page offering a "download SBOM" button needs to
	// know whether there is one; it does not need forty megabytes to draw a
	// button.
	Documents []DocumentSummary `json:"documents,omitempty"`

	// Missing says the artifact is not in the repository the scanner answers
	// for. Only meaningful with StatusNotScanned, and the difference between
	// "nobody has scanned this yet" and "this was never shipped here" - which
	// are different teams' work and which the scanner reports identically.
	Missing bool `json:"missing,omitempty"`

	// ScannedAt is when the SCANNER produced this result, and RetrievedAt when
	// we fetched it. Two different times, and the gap between them is how stale
	// a cached answer is allowed to look before somebody asks for a refresh.
	ScannedAt   *time.Time `json:"scannedAt,omitempty"`
	RetrievedAt time.Time  `json:"retrievedAt"`
	// FromCache says this report was served without asking the scanner.
	FromCache bool `json:"fromCache,omitempty"`
}

// Detail says how much of a stored report a reader needs.
//
// A report has two tiers. The INDEX is identity and grade: the CVE, the
// component, the severity, whether a fix exists. It is one row per finding and
// it is what makes a finding countable, sortable, searchable and comparable.
// The PROSE is the paragraph: the description, the references, the CVSS
// vector, the policy that flagged it.
//
// The two are wildly different sizes, and not because the paragraph is long.
// Prose belongs to a CVE and the index belongs to an occurrence, and a release
// here has eighty-four thousand occurrences of three thousand CVEs. Reading
// the prose tier for such a release means decompressing and parsing every
// stored scanner payload in order to write the same three thousand paragraphs
// out twenty-seven times each - fifty-nine megabytes of duplication in a
// hundred-and-nineteen-megabyte answer.
//
// So a reader says which tier it needs. A comparison classifies on identity
// and grade alone and never reads a paragraph, so it asks for IndexOnly and
// the fifty-nine megabytes never exist. A page that shows a person one CVE
// asks for WithProse.
type Detail bool

const (
	// IndexOnly reads the durable half: statuses, counts, identities, grades.
	IndexOnly Detail = false
	// WithProse also merges the descriptions, references and CVSS vectors from
	// the detail tier, where they are still held.
	WithProse Detail = true
)

// DocumentSummary is a held document, named and measured but not carried.
type DocumentSummary struct {
	Kind DocumentKind `json:"kind"`
	// Provider names the scanner whose body this is.
	//
	// Needed once two scanners answer for one image: the vulnerability response
	// exists once per scanner, they are different documents about the same
	// bytes, and a download menu that could not name them would offer two
	// identical-looking buttons.
	Provider string `json:"provider,omitempty"`
	// Available is false for a document the scanner was asked for and did not
	// have - which is worth saying, because the alternative is a button that
	// silently downloads nothing.
	Available   bool   `json:"available"`
	ContentType string `json:"contentType,omitempty"`
	SourceBytes int    `json:"sourceBytes,omitempty"`
	FetchedAt   string `json:"fetchedAt,omitempty"`
	Message     string `json:"message,omitempty"`
}

// Recount recomputes Counts from Findings. Called by providers after
// normalizing, and by anything that filters a report's findings.
func (r *Report) Recount() {
	r.Counts = Counts{}
	for _, f := range r.Findings {
		r.Counts.AddGrade(GradeOf(f))
	}
}

// Sort orders findings worst first, then by identifier, then by component, so
// that two runs over the same data produce byte-identical output. Exports are
// diffed by people; a stable order is not cosmetic.
func (r *Report) Sort() { SortFindings(r.Findings) }

// DedupeFindings collapses findings that are the same STORED row, merging what
// the copies knew.
//
// # Why a provider must call this before it hands findings back
//
// Because the count and the table have to agree, and the row is the narrower of
// the two. One Xray issue naming three CVEs across four component entries
// expands to twelve findings, and two of those entries are routinely the same
// package at the same version reached by two impact paths - so two of the twelve
// are one row, counted twice. Collapsing here rather than in the store means the
// number the sync records and the rows it writes come from one list, and a page
// can never quote two totals for one sync again.
//
// Deliberately NOT a collapse on Key(): that would merge two genuinely different
// builds of one package, which is a real second thing to upgrade.
func DedupeFindings(in []Finding) []Finding {
	if len(in) < 2 {
		return in
	}
	at := make(map[string]int, len(in))
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		k := f.StorageKey()
		i, seen := at[k]
		if !seen {
			at[k] = len(out)
			out = append(out, f)
			continue
		}
		// The copies are the same row and may not carry the same detail: one
		// impact path may name a fixed version the other omits. Keep the worse
		// grade and the union of what is known, because losing a fix version to
		// deduplication would report a fixable finding as unfixable.
		out[i] = enrich(out[i], f)
	}
	return out
}

// enrich folds one finding's knowledge into another's, keeping the worse grade
// and the union of the facts.
//
// # Why this is one function and not two
//
// Two findings arrive at the same row by two routes: a single scanner reporting
// the same package twice by two impact paths (DedupeFindings), and two
// scanners reporting the same package once each (MergeFindings). The arithmetic
// is identical - keep the worse severity, union what is known, never lose a
// fix version - and writing it twice is how the two drift until a fix version
// survives one path and not the other.
//
// The rules, and each of them is a bug that was fixed by choosing this way:
//
//   - Severity: the WORSE wins. Two scanners disagreeing about one CVE is not
//     a reason to report the kinder answer, and Observations keeps both so the
//     disagreement is still auditable.
//   - Fixable and FixedIn: the UNION. Losing a fix version to a merge would
//     report a fixable finding as unfixable, which is the one direction of
//     error that costs somebody an upgrade they could have asked for.
//   - KEV: sticky, with its claimant. One scanner with a KEV feed is enough;
//     a scanner without one saying nothing is not a denial.
//   - Prose, CVSS, EPSS, references: filled where absent. This is the
//     enrichment the whole exercise is for - Anchore carries an EPSS score and
//     a description where Xray carries a CVSS vector and a policy, and a reader
//     wants the union rather than whichever arrived first.
//   - WillNotFix: sticky. A vendor's refusal is a fact one source may know and
//     another may not have loaded.
func enrich(kept, f Finding) Finding {
	if f.Severity.Rank() > kept.Severity.Rank() {
		kept.Severity = f.Severity
	}
	kept.Fixable = kept.Fixable || f.Fixable
	kept.FixedIn = mergeStrings(kept.FixedIn, f.FixedIn)
	kept.Sources = mergeStrings(kept.Sources, f.Sources)
	kept.References = mergeStrings(kept.References, f.References)
	if !kept.KEV && f.KEV {
		kept.KEV, kept.KEVSource = true, f.KEVSource
	}
	kept.WillNotFix = kept.WillNotFix || f.WillNotFix
	if kept.Summary == "" {
		kept.Summary = f.Summary
	}
	if kept.Description == "" {
		kept.Description = f.Description
	}
	if kept.ID == "" {
		kept.ID = f.ID
	}
	if kept.CVE == "" {
		kept.CVE = f.CVE
	}
	if kept.Policy == "" {
		kept.Policy = f.Policy
	}
	if kept.Component.Path == "" {
		kept.Component.Path = f.Component.Path
	}
	if kept.Published == nil {
		kept.Published = f.Published
	}
	// The higher score wins rather than the first one, for the same reason the
	// worse severity does: two sources scoring one CVE differently is not a
	// licence to quote the gentler number.
	if f.CVSSScore > kept.CVSSScore {
		kept.CVSSScore, kept.CVSSVector = f.CVSSScore, f.CVSSVector
	} else if kept.CVSSVector == "" {
		kept.CVSSVector = f.CVSSVector
	}
	if kept.EPSS == nil || (f.EPSS != nil && f.EPSS.Score > kept.EPSS.Score) {
		kept.EPSS = f.EPSS
	}
	kept.Observations = mergeObservations(kept.Observations, f.Observations)
	return kept
}

// mergeObservations unions two gradings lists, preserving first-seen order.
func mergeObservations(a, b []Observation) []Observation {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]Observation, 0, len(a)+len(b))
	for _, list := range [][]Observation{a, b} {
		for _, o := range list {
			k := o.ObservationKey()
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, o)
		}
	}
	return out
}

// mergeStrings unions two lists, preserving first-seen order.
func mergeStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// SortFindings orders a finding slice worst-first and deterministically.
//
// # The order, and why it is that order
//
// Known-exploited first, then severity, then fixable, then identity. It is the
// order somebody works in, not the order the data arrives in:
//
//  1. KEV, because a vulnerability that is being exploited is not the same kind
//     of thing as one that is merely severe. Every other criterion is a
//     judgement about what COULD happen; this one is a record of what HAS.
//  2. Severity within that, so the four exploited criticals come before the
//     eleven exploited mediums.
//  3. FIXABLE ahead of unfixable at equal severity, and this is the one that
//     surprises people. A release here is a vendor's build: nothing in it can
//     be patched locally, so the only actionable rows are the ones with a
//     version to ask the vendor for. Sorting a hundred unfixable criticals
//     above four fixable ones puts the whole of the afternoon's work below the
//     fold.
//  4. Identifier, then component, so two runs over the same data produce
//     byte-identical output. Exports are diffed by people; a stable order is
//     not cosmetic.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.KEV != b.KEV {
			return a.KEV
		}
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if a.Fixable != b.Fixable {
			return a.Fixable
		}
		if a.Identifier() != b.Identifier() {
			return a.Identifier() < b.Identifier()
		}
		return a.Component.ComponentKey() < b.Component.ComponentKey()
	})
}

// Posture is the security state of a whole release: every artifact's report,
// aggregated, with the coverage that produced it stated alongside.
//
// The aggregate and the coverage travel together on purpose. "1,286
// vulnerabilities" means one thing when every artifact was scanned and
// something else entirely when a fifth of them were not, and a summary that
// carries the first number without the second is the one that gets quoted in a
// release meeting.
type Posture struct {
	// Reports is one per artifact, including the ones with nothing to report.
	Reports []Report `json:"reports"`
	// Counts is every scanned artifact's findings, summed. Findings appearing
	// in more than one artifact are counted once per artifact: a base-image CVE
	// present in ten images is ten things to fix in ten places, and reporting
	// it as one understates the work. UniqueCounts states the other number.
	Counts Counts `json:"counts"`
	// UniqueCounts collapses the same (CVE, component) across artifacts, which
	// is the number to quote when asking "how many distinct problems".
	UniqueCounts Counts `json:"uniqueCounts"`
	// UniqueCVEs collapses the ADVISORY alone, ignoring which package carries
	// it. A third number, and it earns its place because it is the one the page
	// prints biggest.
	//
	// The three are genuinely different questions and a page that quotes one
	// while labelling it another teaches a reader to trust none of them:
	// openssl and libssl3 carrying CVE-2026-31789 are two things to upgrade
	// (UniqueCounts), one advisory to read (UniqueCVEs), and seventeen packages
	// across a hundred and forty-eight images to actually replace (Counts).
	UniqueCVEs int `json:"uniqueCves"`
	// UniqueCVECounts assigns each advisory to its highest observed severity.
	// It is the severity breakdown for UniqueCVEs, rather than the component
	// pair breakdown in UniqueCounts.
	UniqueCVECounts Counts `json:"uniqueCveCounts"`

	// KEVs is the DISTINCT advisories in this release that are known to be
	// exploited, and KEVFixable how many of those have a fix.
	//
	// Distinct rather than per-occurrence, and that is the difference between a
	// number somebody acts on and one they argue with: a KEV in a base image
	// carried by forty images is one advisory to chase and forty places it
	// lands, and "40 known-exploited vulnerabilities" reads as forty problems.
	// Counts.KEV still carries the per-occurrence figure for the work estimate.
	KEVs        int            `json:"kevs"`
	KEVFixable  int            `json:"kevFixable"`
	KEVSeverity SeverityCounts `json:"kevSeverity"`

	Coverage Coverage `json:"coverage"`

	// BySource is the same arithmetic, per scanner, plus how much of it only
	// that scanner reported.
	//
	// Empty on a deployment with one scanner - there is nothing to compare - and
	// the interface hides the control rather than offering a toggle with one
	// position.
	BySource []SourceCounts `json:"bySource,omitempty"`

	// Providers names every scanner that contributed, sorted.
	Providers []string `json:"providers,omitempty"`
	// ScannedAt is the OLDEST scan time among contributing reports - the age of
	// the weakest link, which is the honest answer to "how fresh is this".
	ScannedAt *time.Time `json:"scannedAt,omitempty"`
}

// SourceCounts is one scanner's contribution to a posture.
//
// # Why "only" is a field rather than something the client derives
//
// Because deriving it needs every finding from every scanner in one place, and
// the client that needs the number most is the one rendering a summary row
// without the rows behind it. "Xray found 3,111 of which 402 nobody else saw"
// is the sentence a reader wants when a second scanner arrives, and it must not
// require downloading two hundred thousand findings to compose.
type SourceCounts struct {
	// Provider is the scanner - "jfrog-xray", "anchore", "astra".
	Provider string `json:"provider"`
	// Counts is every finding this scanner reported, summed per artifact.
	Counts Counts `json:"counts"`
	// UniqueCVEs is the distinct advisories this scanner reported.
	UniqueCVEs int `json:"uniqueCves"`
	// OnlyHere is the distinct advisories NO other scanner reported. Zero on a
	// single-scanner deployment, where every finding is trivially only there.
	OnlyHere int `json:"onlyHere"`
	// KEVs is the distinct known-exploited advisories this scanner reported.
	//
	// Worth its own field because it is the number that decides whether a
	// second scanner earned its licence: a scanner that contributed four
	// thousand extra lows nobody will read and a scanner that contributed two
	// KEVs nobody else saw look identical in OnlyHere.
	KEVs int `json:"kevs"`
	// Artifacts is how many artifacts this scanner answered for.
	Artifacts int `json:"artifacts"`
}

// Coverage states how much of a release the numbers actually cover.
type Coverage struct {
	// Artifacts is every artifact considered, Scanned those with results.
	Artifacts   int `json:"artifacts"`
	Scanned     int `json:"scanned"`
	NotScanned  int `json:"notScanned"`
	Unsupported int `json:"unsupported"`
	Unavailable int `json:"unavailable"`
	Disabled    int `json:"disabled"`
	// Missing is artifacts that are not in the scanned repository at all. A
	// bucket of its own because it is the only one whose fix is a transfer
	// rather than a scan, and counting it as unscanned sent people to look for
	// a scanning problem that was not there.
	Missing int `json:"missing"`
}

// Complete reports whether every artifact that could be scanned, was.
func (c Coverage) Complete() bool {
	return c.Scanned > 0 && c.NotScanned == 0 && c.Unavailable == 0 &&
		c.Disabled == 0 && c.Missing == 0
}

// Any reports whether there is any security data at all.
func (c Coverage) Any() bool { return c.Scanned > 0 }

// Scannable is the denominator a percentage should use: artifacts a scanner
// could have an opinion about. Excludes the unsupported ones.
func (c Coverage) Scannable() int {
	n := c.Artifacts - c.Unsupported
	if n < 0 {
		return 0
	}
	return n
}

// Summarize aggregates reports into a Posture.
func Summarize(reports []Report) Posture {
	p := Posture{Reports: reports}
	seenProvider := map[string]bool{}
	unique := map[string]Severity{}
	uniqueFixable := map[string]bool{}
	uniqueCVEs := map[string]bool{}
	uniqueCVESeverity := map[string]Severity{}
	uniqueCVEFixable := map[string]bool{}
	// KEV is tracked per ADVISORY rather than per occurrence. See Posture.KEVs.
	uniqueCVEKEV := map[string]bool{}
	// Per-source arithmetic, and which sources saw each advisory. Built in the
	// same pass because a second walk over a release's findings is a second walk
	// over a hundred thousand rows.
	bySource := map[string]*SourceCounts{}
	sawCVE := map[string]map[string]bool{}

	for _, r := range reports {
		p.Coverage.Artifacts++
		switch r.Status {
		case StatusScanned:
			p.Coverage.Scanned++
		case StatusNotScanned:
			if r.Missing {
				p.Coverage.Missing++
			} else {
				p.Coverage.NotScanned++
			}
		case StatusUnsupported:
			p.Coverage.Unsupported++
		case StatusDisabled:
			p.Coverage.Disabled++
		case StatusUnavailable:
			p.Coverage.Unavailable++
		}
		if r.Provider != "" && !seenProvider[r.Provider] {
			seenProvider[r.Provider] = true
			p.Providers = append(p.Providers, r.Provider)
		}
		if r.Status != StatusScanned {
			continue
		}
		if r.ScannedAt != nil && (p.ScannedAt == nil || r.ScannedAt.Before(*p.ScannedAt)) {
			t := *r.ScannedAt
			p.ScannedAt = &t
		}
		p.Counts = p.Counts.Plus(r.Counts)
		for _, src := range sourcesOf(r) {
			sc, ok := bySource[src]
			if !ok {
				sc = &SourceCounts{Provider: src}
				bySource[src] = sc
			}
			sc.Artifacts++
		}
		for _, f := range r.Findings {
			k := f.Key()
			// Worst severity wins for the unique roll-up: the same CVE graded
			// differently in two images is one problem at its worst grade.
			if prev, ok := unique[k]; !ok || f.Severity.Rank() > prev.Rank() {
				unique[k] = f.Severity
			}
			if f.Fixable {
				uniqueFixable[k] = true
			}
			if id := f.Identifier(); id != "" {
				uniqueCVEs[id] = true
				if prev, ok := uniqueCVESeverity[id]; !ok || f.Severity.Rank() > prev.Rank() {
					uniqueCVESeverity[id] = f.Severity
				}
				if f.Fixable {
					uniqueCVEFixable[id] = true
				}
				if f.KEV {
					uniqueCVEKEV[id] = true
				}
				for _, src := range f.SourceSet() {
					seen, ok := sawCVE[id]
					if !ok {
						seen = map[string]bool{}
						sawCVE[id] = seen
					}
					seen[src] = true
				}
			}
			for _, src := range f.SourceSet() {
				sc, ok := bySource[src]
				if !ok {
					sc = &SourceCounts{Provider: src}
					bySource[src] = sc
				}
				sc.Counts.AddGrade(GradeOf(f))
			}
		}
	}

	for k, sev := range unique {
		p.UniqueCounts.Add(sev, uniqueFixable[k])
	}
	p.UniqueCVEs = len(uniqueCVEs)
	for id, severity := range uniqueCVESeverity {
		p.UniqueCVECounts.AddGrade(Grade{
			Severity: severity, Fixable: uniqueCVEFixable[id], KEV: uniqueCVEKEV[id],
		})
	}
	p.KEVs = len(uniqueCVEKEV)
	p.KEVFixable = p.UniqueCVECounts.KEVFixable
	p.KEVSeverity = p.UniqueCVECounts.KEVBySeverity

	for id, srcs := range sawCVE {
		for src := range srcs {
			if sc, ok := bySource[src]; ok {
				sc.UniqueCVEs++
				if len(srcs) == 1 {
					sc.OnlyHere++
				}
				if uniqueCVEKEV[id] {
					sc.KEVs++
				}
			}
		}
	}
	// Only worth reporting once there is something to compare. One scanner's
	// per-source breakdown is the release's breakdown restated, and a segmented
	// control with a single position is a control that should not be drawn.
	if len(bySource) > 1 {
		p.BySource = make([]SourceCounts, 0, len(bySource))
		for _, sc := range bySource {
			p.BySource = append(p.BySource, *sc)
		}
		sort.Slice(p.BySource, func(i, j int) bool {
			return p.BySource[i].Provider < p.BySource[j].Provider
		})
	}
	sort.Strings(p.Providers)
	return p
}

// sourcesOf is the scanners that answered for one artifact.
func sourcesOf(r Report) []string {
	if r.Provider != "" {
		return []string{r.Provider}
	}
	return nil
}

// FingerprintReports produces a stable hash over a set of reports.
//
// Used as an ETag: a browser holding last minute's answer needs to know whether
// this minute's is the same one, and re-sending two megabytes of findings to
// say "nothing changed" is the cost this avoids. Over the findings themselves
// rather than over a timestamp, so a re-scan that produced identical results
// does not invalidate anything.
func FingerprintReports(reports []Report) string {
	h := sha256.New()
	keys := make([]string, 0, len(reports))
	index := map[string]Report{}
	for _, r := range reports {
		k := r.Artifact.Ref()
		keys = append(keys, k)
		index[k] = r
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := index[k]
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", k, r.Status, len(r.Findings))
		fs := append([]Finding(nil), r.Findings...)
		SortFindings(fs)
		for _, f := range fs {
			fmt.Fprintf(h, "%s\x00%s\x00%t\x00%t\x00%s\x00%s\x00",
				f.Key(), f.Severity, f.Fixable, f.KEV,
				strings.Join(f.FixedIn, ","), strings.Join(f.SourceSet(), ","))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
