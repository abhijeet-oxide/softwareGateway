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
		components := issue.Components

		emit := func(cve string, componentID string, fixed []string) {
			f := security.Finding{
				CVE:         strings.ToUpper(strings.TrimSpace(cve)),
				ID:          issue.IssueID,
				Severity:    severity,
				Summary:     issue.Summary,
				Description: issue.Description,
				Component:   parseComponentID(componentID),
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
			emit("", "", nil)
		case len(components) == 0:
			for _, c := range cves {
				emit(c.CVE, "", nil)
			}
		default:
			for componentID, detail := range components {
				if len(cves) == 0 {
					emit("", componentID, detail.FixedVersions)
					continue
				}
				for _, c := range cves {
					emit(c.CVE, componentID, detail.FixedVersions)
				}
			}
		}
	}

	security.SortFindings(out)
	return out
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

// bestCVSS prefers v3 over v2, and takes the highest score among the issue's
// CVEs. Informational only - nothing in the platform sorts on it.
func bestCVSS(issue xrayIssue) (float64, string) {
	var best float64
	var vector string
	for _, c := range issue.CVEs {
		if s := toFloat(c.CVSSV3Score); s > best {
			best, vector = s, c.CVSSV3Vector
		}
	}
	if best > 0 {
		return best, vector
	}
	for _, c := range issue.CVEs {
		if s := toFloat(c.CVSSV2Score); s > best {
			best, vector = s, c.CVSSV2Vector
		}
	}
	return best, vector
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
// Signatures, attestations and SBOMs are not things Xray declines to scan; they
// are things there is nothing to scan in. Counting them as unscanned coverage
// would pin every release permanently below full coverage and teach people to
// ignore the number.
func scannable(ref security.ArtifactRef) bool {
	switch strings.ToLower(ref.Kind) {
	case "signature", "attestation", "sbom", "provenance":
		return false
	}
	switch {
	case strings.Contains(ref.MediaType, "signature"),
		strings.Contains(ref.MediaType, "in-toto"),
		strings.Contains(ref.MediaType, "spdx"),
		strings.Contains(ref.MediaType, "cyclonedx"):
		return false
	}
	return true
}

// notIndexedMessage turns Xray's own words about an unindexed artifact into a
// sentence with an action in it.
func notIndexedMessage(xraySaid string) string {
	if strings.TrimSpace(xraySaid) == "" {
		return "JFrog Xray has no scan result for this artifact yet."
	}
	return fmt.Sprintf("JFrog Xray has no scan result for this artifact: %s", strings.TrimSpace(xraySaid))
}
