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
}

// Key identifies a finding within one artifact: which problem, in which
// package. Severity is deliberately NOT part of it - a finding whose severity
// was re-graded between releases is the same finding, and that re-grading is
// exactly what the comparison exists to report.
func (f Finding) Key() string {
	return f.Identifier() + "|" + f.Component.ComponentKey()
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

	// ScannedAt is when the SCANNER produced this result, and RetrievedAt when
	// we fetched it. Two different times, and the gap between them is how stale
	// a cached answer is allowed to look before somebody asks for a refresh.
	ScannedAt   *time.Time `json:"scannedAt,omitempty"`
	RetrievedAt time.Time  `json:"retrievedAt"`
	// FromCache says this report was served without asking the scanner.
	FromCache bool `json:"fromCache,omitempty"`
}

// Recount recomputes Counts from Findings. Called by providers after
// normalizing, and by anything that filters a report's findings.
func (r *Report) Recount() {
	r.Counts = Counts{}
	for _, f := range r.Findings {
		r.Counts.Add(f.Severity, f.Fixable)
	}
}

// Sort orders findings worst first, then by identifier, then by component, so
// that two runs over the same data produce byte-identical output. Exports are
// diffed by people; a stable order is not cosmetic.
func (r *Report) Sort() { SortFindings(r.Findings) }

// SortFindings orders a finding slice worst-first and deterministically.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
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

	Coverage Coverage `json:"coverage"`

	// Providers names every scanner that contributed, sorted.
	Providers []string `json:"providers,omitempty"`
	// ScannedAt is the OLDEST scan time among contributing reports - the age of
	// the weakest link, which is the honest answer to "how fresh is this".
	ScannedAt *time.Time `json:"scannedAt,omitempty"`
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
}

// Complete reports whether every artifact that could be scanned, was.
func (c Coverage) Complete() bool {
	return c.Scanned > 0 && c.NotScanned == 0 && c.Unavailable == 0 && c.Disabled == 0
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

	for _, r := range reports {
		p.Coverage.Artifacts++
		switch r.Status {
		case StatusScanned:
			p.Coverage.Scanned++
		case StatusNotScanned:
			p.Coverage.NotScanned++
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
		}
	}

	for k, sev := range unique {
		p.UniqueCounts.Add(sev, uniqueFixable[k])
	}
	sort.Strings(p.Providers)
	return p
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
			fmt.Fprintf(h, "%s\x00%s\x00%t\x00%s\x00", f.Key(), f.Severity, f.Fixable, strings.Join(f.FixedIn, ","))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
