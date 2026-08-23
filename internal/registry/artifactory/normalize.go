package artifactory

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// normalizeArtifact turns one Xray artifact summary into normalized findings.
//
// This function is the whole width of the JFrog/Xray boundary. Above it nothing
// knows what an `issue_id` is, that severities are capitalised, that a component
// identifier looks like `deb://openssl:1.1.1n-0+deb11u3`, or that one Xray issue
// can name several CVEs and several components at once. Below it, nothing knows
// what a release is.
func normalizeArtifact(a xrayArtifact) []security.Finding {
	var out []security.Finding

	for _, issue := range a.Issues {
		// License issues share the payload with security ones and are not
		// vulnerabilities. Including them would inflate every count on every
		// page with something that is not a security finding at all.
		if issue.IssueType != "" && !strings.EqualFold(issue.IssueType, "security") {
			continue
		}

		severity := security.ParseSeverity(issue.Severity)
		published := parseXrayTime(issue.Created)
		score, vector := bestCVSS(issue)

		// One Xray issue can name several CVEs and several components. The
		// platform's unit is (CVE, component) because that is what a person
		// fixes: "CVE-2024-3094 in openssl" is one job, and the same CVE in
		// zlib is a different one. Expanding here rather than downstream keeps
		// every consumer - counts, comparison, search, export - working in the
		// same unit.
		cves := issue.CVEs
		components := issueComponents(issue)

		emit := func(cve string, component security.Component, fixed []string) {
			f := security.Finding{
				CVE:         strings.ToUpper(strings.TrimSpace(cve)),
				ID:          issue.IssueID,
				Severity:    severity,
				Summary:     issue.Summary,
				Description: issue.Description,
				Component:   component,
				FixedIn:     dedupe(fixed),
				Fixable:     len(fixed) > 0,
				CVSSScore:   score,
				CVSSVector:  vector,
				References:  issue.References,
				Published:   published,
				Provider:    providerName,
				Policy:      issue.WatchName,
			}
			out = append(out, f)
		}

		switch {
		case len(components) == 0 && len(cves) == 0:
			emit("", security.Component{}, nil)
		case len(components) == 0:
			for _, c := range cves {
				emit(c.CVE, security.Component{}, nil)
			}
		default:
			for _, comp := range components {
				component := componentOf(comp)
				if len(cves) == 0 {
					emit("", component, comp.FixedVersions)
					continue
				}
				for _, c := range cves {
					emit(c.CVE, component, comp.FixedVersions)
				}
			}
		}
	}

	security.SortFindings(out)
	return out
}

// issueComponents is the vulnerable packages, from wherever this Xray put them.
//
// The array is the answer when it is there. When it is not - a v1 payload has
// no `components` at all - the impact path still names the package at its end,
// as ".../sha256__<layer>.tar.gz/3.23:libssl3:3.5.5-r0", and a finding that
// says WHICH package is wrong beats one that says only that something is.
func issueComponents(issue xrayIssue) []xrayComponent {
	if len(issue.Components) > 0 {
		return issue.Components
	}
	seen := map[string]bool{}
	var out []xrayComponent
	for _, path := range issue.ImpactPath {
		id, version := componentFromImpactPath(path)
		if id == "" || seen[id+"@"+version] {
			continue
		}
		seen[id+"@"+version] = true
		out = append(out, xrayComponent{ComponentID: id, Version: version})
	}
	return out
}

// componentFromImpactPath reads the package off the end of an impact path.
//
// The tail is `<qualifier>:<name>:<version>` for a distro package and `<name>`
// for anything else, so the version is taken only when the tail has one.
func componentFromImpactPath(path string) (id, version string) {
	tail := path
	if i := strings.LastIndex(tail, "/"); i >= 0 {
		tail = tail[i+1:]
	}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return "", ""
	}
	if i := strings.LastIndex(tail, ":"); i > 0 {
		return tail[:i], tail[i+1:]
	}
	return tail, ""
}

// componentOf turns Xray's component into the platform's.
//
// # Why the id is not simply split on its last colon
//
// Because two Xray versions mean two different things by it. The old form is
// `deb://openssl:1.1.1n-0+deb11u3` - ecosystem, name and version in one string.
// The current one is `3.23:libcrypto3` with `version` and `pkg_type` as fields
// of their own, and splitting THAT on its last colon yields a package called
// "3.23" at version "libcrypto3".
func componentOf(c xrayComponent) security.Component {
	id := strings.TrimSpace(c.ComponentID)
	if id == "" {
		return security.Component{}
	}

	// The old self-describing form, which parses on its own.
	if strings.Contains(id, "://") {
		out := parseComponentID(id)
		if c.Version != "" {
			out.Version = c.Version
		}
		if out.Type == "" {
			out.Type = c.PkgType
		}
		return out
	}

	name := id
	// A distro qualifier - the alpine release in `3.23:libcrypto3` - is noise
	// in front of the package name. Stripped only when it is one: a Maven
	// coordinate is `com.google.guava:guava` and its first half is the answer
	// to "which guava".
	if i := strings.Index(id, ":"); i > 0 && isDistroQualifier(id[:i]) {
		name = id[i+1:]
	}

	out := security.Component{Name: name, Version: c.Version, Type: c.PkgType}
	if c.PkgType != "" {
		out.ID = strings.ToLower(c.PkgType) + "://" + name
	} else {
		out.ID = name
	}
	return out
}

// isDistroQualifier reports whether a component id's first segment is a release
// number - "3.23", "11", "9.4" - rather than part of the package's name.
func isDistroQualifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// parseComponentID turns Xray's component identifier into a normalized
// component.
//
// # Why the version is not part of the identity
//
// Xray identifies `deb://openssl:1.1.1n-0+deb11u3`, version included. Carrying
// that version into the comparison key would be wrong in the case the
// comparison exists for: an upgraded image whose openssl moved from 1.1.1n to
// 1.1.1w, still carrying the same CVE, would read as one finding resolved and
// one introduced - a patch release reporting a fix it did not make.
//
// So the identity is `deb://openssl` and the version rides alongside as data.
// A genuine fix removes the finding and shows as resolved; a version bump that
// changes nothing shows as unchanged, which is the truth.
func parseComponentID(id string) security.Component {
	id = strings.TrimSpace(id)
	if id == "" {
		return security.Component{}
	}

	ecosystem, rest := "", id
	if i := strings.Index(id, "://"); i >= 0 {
		ecosystem, rest = id[:i], id[i+3:]
	}

	name, version := rest, ""
	// Last colon wins: a Maven coordinate is `gav://group:artifact:version`,
	// so splitting on the FIRST colon would put the artifact in the version.
	if i := strings.LastIndex(rest, ":"); i > 0 {
		name, version = rest[:i], rest[i+1:]
	}

	c := security.Component{Name: name, Version: version, Type: ecosystem}
	if ecosystem != "" {
		c.ID = ecosystem + "://" + name
	} else {
		c.ID = name
	}
	return c
}

// bestCVSS prefers the newest scoring system present, and takes the highest
// score among the issue's CVEs. Informational only - nothing in the platform
// sorts on it.
func bestCVSS(issue xrayIssue) (float64, string) {
	// v4, v3, v2: each tier is tried in full before the next, so an issue that
	// carries both does not end up scored by the older one because its number
	// happened to be larger.
	for _, pick := range []func(xrayCVE) (float64, string){
		func(c xrayCVE) (float64, string) { return splitCVSS(c.CVSSV4) },
		func(c xrayCVE) (float64, string) {
			if s, v := splitCVSS(c.CVSSV3); s > 0 {
				return s, v
			}
			return toFloat(c.CVSSV3Score), c.CVSSV3Vector
		},
		func(c xrayCVE) (float64, string) {
			if s, v := splitCVSS(c.CVSSV2); s > 0 {
				return s, v
			}
			return toFloat(c.CVSSV2Score), c.CVSSV2Vector
		},
	} {
		var best float64
		var vector string
		for _, c := range issue.CVEs {
			if s, v := pick(c); s > best {
				best, vector = s, v
			}
		}
		if best > 0 {
			return best, vector
		}
	}
	return 0, ""
}

// splitCVSS reads the combined form, "7.5/CVSS:3.1/AV:N/AC:L/...", where the
// score is everything before the first slash and the vector is the rest.
func splitCVSS(s string) (float64, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	score, vector := s, ""
	if i := strings.Index(s, "/"); i >= 0 {
		score, vector = s[:i], s[i+1:]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(score), 64)
	if err != nil {
		return 0, ""
	}
	return f, strings.TrimSpace(vector)
}

// toFloat reads a score that Xray sends as a number on some versions and as a
// string on others. A tolerant read rather than a typed one, because a score is
// decoration and failing a whole release's findings over it would be absurd.
func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case int:
		return float64(t)
	default:
		return 0
	}
}

// parseXrayTime reads the timestamps Xray emits, tolerating the formats it has
// used across versions. Returns nil rather than an error: a missing advisory
// date is not a reason to lose a finding.
func parseXrayTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999Z0700",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// scannable reports whether Xray could have anything to say about this kind of
// artifact.
//
// # An allow-list, and why it is not a deny-list
//
// Xray indexes CONTAINER IMAGES. It has nothing to say about a Helm chart, a
// signature, an SBOM or a loose file, and asking it produces the same answer
// every time: not indexed. That answer is indistinguishable, in a coverage
// number, from an image somebody genuinely forgot to index - so a release of
// 261 artifacts with 150 charts and signatures in it reported 4% coverage, a
// scary banner and two hundred rows of work that did not exist.
//
// It was a deny-list of the four things obviously not scannable, which meant
// every artifact type nobody had thought about was asked about by default. The
// list of things Xray does scan is short and known; the list of things anybody
// might put in a release is not.
func scannable(ref security.ArtifactRef) bool {
	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "image":
		return true
	case "":
		// An artifact the core could not classify is decided on its media type
		// rather than assumed either way.
		return isImageMediaType(ref.MediaType)
	default:
		return false
	}
}

// isImageMediaType recognises the Docker and OCI IMAGE manifest types. An index
// is deliberately not one: in a release it is the manifest listing everything
// else, and the per-platform images it points at are asked about individually.
func isImageMediaType(mediaType string) bool {
	m := strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case strings.Contains(m, "docker.distribution.manifest.list"),
		strings.Contains(m, "image.index"):
		return false
	case strings.Contains(m, "docker.distribution.manifest"),
		strings.Contains(m, "image.manifest"):
		return true
	}
	return false
}

// unsupportedMessage says what the artifact is, and that this is not a failure.
//
// "JFrog Xray does not scan this kind of artifact" left a reader wondering
// whether that was a configuration they could change. Naming the kind and the
// scanner's scope in one sentence closes the question.
func unsupportedMessage(ref security.ArtifactRef) string {
	kind := strings.ToLower(strings.TrimSpace(ref.Kind))
	switch kind {
	case "chart", "helm":
		return "JFrog Xray scans container images. This is a Helm chart, so there is nothing for it to scan."
	case "index":
		return "JFrog Xray scans container images. This is the manifest that lists them, " +
			"and the images it points at are scanned individually."
	case "signature", "attestation", "sbom", "provenance":
		return "JFrog Xray scans container images. This is a " + kind +
			", which describes an image rather than containing software."
	case "":
		return "JFrog Xray scans container images, and this artifact is not one."
	default:
		return "JFrog Xray scans container images. This is a " + kind + ", which it does not index."
	}
}

// notIndexedMessage turns Xray's own words about an unindexed artifact into a
// sentence with an action in it.
func notIndexedMessage(xraySaid string) string {
	if strings.TrimSpace(xraySaid) == "" {
		return "JFrog Xray has no scan result for this artifact yet."
	}
	return fmt.Sprintf("JFrog Xray has no scan result for this artifact: %s", strings.TrimSpace(xraySaid))
}
