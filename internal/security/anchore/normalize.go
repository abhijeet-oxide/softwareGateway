package anchore

import (
	"sort"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Turning Anchore's answer into the platform's own model.
//
// This file is the whole of the provider boundary's cost, and it is the reason
// nothing above it had to change to gain a second scanner. Everything Anchore
// spells its own way - the fixed version that is the string "None", the
// severity that may be a distribution's word, the two independent gradings of
// one CVE - is dealt with here and nowhere else.
//
// # The rule this file follows everywhere
//
// Never resolve a disagreement by discarding one side. Anchore reports a vendor
// grading and an NVD grading for the same CVE and they routinely differ; a
// normalizer that picked one would silently decide, for every reader, which
// vulnerability database their organization believes. Both are kept as
// Observations, one of them is chosen as the REPORTED severity by a documented
// rule, and the rule is written down where a reader can see it (see
// severityFor).

// normalizeImage turns one image's vulnerability response into findings.
//
// One finding per (advisory, package), which is what Anchore already returns
// and what the platform's own storage identity is - so this is a field mapping
// rather than a restructuring, and the identity survives the round trip
// unchanged.
func normalizeImage(res imageVulnerabilities) []security.Finding {
	out := make([]security.Finding, 0, len(res.Vulnerabilities))
	for _, v := range res.Vulnerabilities {
		out = append(out, normalizeVuln(v))
	}
	out = security.DedupeFindings(out)
	// Sorted here rather than left to the reader, so what is STORED is already
	// in the order somebody works in - known-exploited first, then severity,
	// then fixable. A page that has to sort ninety thousand rows to show the
	// first twenty is a page that sorts ninety thousand rows.
	security.SortFindings(out)
	return out
}

func normalizeVuln(v packageVuln) security.Finding {
	severity, observations := severityFor(v)
	fixed := fixVersions(v.Fix)

	f := security.Finding{
		CVE:         upperCVE(v.Vuln),
		ID:          v.Vuln,
		Severity:    severity,
		Summary:     summaryOf(v),
		Description: descriptionOf(v),
		Component:   componentOf(v),
		FixedIn:     fixed,
		// Fixable is len(fixed) != 0 here and not `v.Fix != ""`, because the
		// string Anchore sends for "no fix" is "None" - see fixVersions.
		Fixable:      len(fixed) > 0,
		WillNotFix:   v.WillNotFix || willNotFixAnnotation(v),
		Provider:     ProviderName,
		Sources:      []string{ProviderName},
		Observations: observations,
		Published:    v.DetectedAt.Ptr(),
	}
	if v.URL != "" {
		f.References = []string{v.URL}
	}
	// The CVSS pair the platform quotes: the highest base score across every
	// grading, with whichever vector came with it. Highest rather than first,
	// for the same reason the severity is the worst: two sources scoring one
	// CVE differently is not a licence to quote the gentler number.
	f.CVSSScore, f.CVSSVector = worstCVSS(v)

	// The two fields this integration is worth having for.
	for _, nvd := range v.NVDData {
		if nvd.IsKEV {
			f.KEV, f.KEVSource = true, ProviderName
		}
		if nvd.EPSS != nil && (f.EPSS == nil || nvd.EPSS.Score > f.EPSS.Score) {
			f.EPSS = &security.EPSS{Score: nvd.EPSS.Score, Percentile: nvd.EPSS.Percentile}
		}
	}
	return f
}

// severityFor decides the grade this platform reports, and keeps the rest.
//
// # The rule
//
// The WORST severity any source gave it, and every source's grading kept
// beside it. Anchore's own `severity` field - the one its interface shows - is
// always among them and is used when it is the worst, which it usually is
// because Anchore derives it from the same data.
//
// # Why the worst rather than the vendor's
//
// The vendor's grading is the better answer to "how bad is this for the
// distribution that shipped it", and it is the wrong default here. This
// platform's readers are deciding whether to accept a vendor's software, and a
// vendor grading their own package's CVE as low is exactly the judgement the
// reader wants to check rather than adopt. So the platform reports the harshest
// available view and shows who said what.
//
// Nothing is lost: Observations carries every grading with its source, and the
// interface puts them beside the number.
func severityFor(v packageVuln) (security.Severity, []security.Observation) {
	worst := security.ParseSeverity(v.Severity)
	observations := make([]security.Observation, 0, 1+len(v.NVDData)+len(v.VendorData))

	// Anchore's own headline grade, recorded with the feed it came from so a
	// reader can tell "Anchore says high" from "Debian says high".
	if v.Severity != "" {
		observations = append(observations, security.Observation{
			Provider: ProviderName,
			Source:   sourceLabel(v.Feed, v.FeedGroup, "anchore"),
			Severity: worst,
		})
	}

	for _, nvd := range v.NVDData {
		score, vector := bestScore(nvd.CVSSV3, nvd.CVSSV2)
		observations = append(observations, security.Observation{
			Provider: ProviderName,
			Source:   sourceLabel(nvd.Source, nvd.Type, "nvd"),
			Score:    score,
			Vector:   vector,
		})
	}
	for _, vendor := range v.VendorData {
		score, vector := bestScore(vendor.CVSSV3, vendor.CVSSV2)
		observations = append(observations, security.Observation{
			Provider: ProviderName,
			Source:   sourceLabel(vendor.Source, vendor.Type, "vendor"),
			Score:    score,
			Vector:   vector,
		})
	}
	return worst, observations
}

// sourceLabel names where one grading came from, in the words the response
// used, falling back to what the field means.
func sourceLabel(primary, secondary, fallback string) string {
	for _, s := range []string{primary, secondary} {
		if v := strings.TrimSpace(s); v != "" && !strings.EqualFold(v, "none") {
			return strings.ToLower(v)
		}
	}
	return fallback
}

// bestScore is the higher of a CVSS v3 and v2 base score, with its vector.
//
// v3 preferred where both are present and scored - it is the newer model and
// the one every modern advisory carries - but a v2-only advisory must still
// report the score it has rather than a zero.
func bestScore(v3, v2 *cvssScore) (float64, string) {
	if s := v3.base(); s > 0 {
		return s, vectorOf(v3)
	}
	if s := v2.base(); s > 0 {
		return s, vectorOf(v2)
	}
	return 0, ""
}

func vectorOf(c *cvssScore) string {
	if c == nil {
		return ""
	}
	return c.Vector
}

// worstCVSS is the highest base score across every grading of one finding.
func worstCVSS(v packageVuln) (float64, string) {
	var score float64
	var vector string
	consider := func(s float64, vec string) {
		if s > score {
			score, vector = s, vec
		}
	}
	for _, nvd := range v.NVDData {
		s, vec := bestScore(nvd.CVSSV3, nvd.CVSSV2)
		consider(s, vec)
	}
	for _, vendor := range v.VendorData {
		s, vec := bestScore(vendor.CVSSV3, vendor.CVSSV2)
		consider(s, vec)
	}
	return score, vector
}

// fixVersions reads Anchore's `fix` field.
//
// # The quirk this exists for, and what it costs to get wrong
//
// Anchore sends the LITERAL STRING "None" when there is no fix - not an empty
// string, not a null. Read naively, every unfixable finding in a release
// arrives with a fixed version called None, `Fixable` is true for all of them,
// and the number a release manager acts on - "4 fixable criticals" - becomes
// "900 fixable criticals". That number decides what somebody does this
// afternoon, and there is nothing on the page that would make them doubt it.
//
// Several fixed versions may be listed comma-separated for a package that has
// more than one fixed stream; they are split so the interface can offer the
// cleanest and the export can carry all of them.
func fixVersions(fix string) []string {
	fix = strings.TrimSpace(fix)
	switch strings.ToLower(fix) {
	case "", "none", "null", "n/a", "wont_fix", "won't fix", "will not fix":
		return nil
	}
	parts := strings.Split(fix, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || strings.EqualFold(p, "None") {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	// Deterministic, because these reach an export that people diff.
	sort.Strings(out)
	return out
}

// willNotFixAnnotation reads the vendor's refusal out of the annotation field,
// for builds that report it there rather than in `will_not_fix`.
func willNotFixAnnotation(v packageVuln) bool {
	switch strings.ToLower(strings.TrimSpace(v.AnnotationStatus)) {
	case "will_not_fix", "wont_fix", "not_affected":
		return true
	}
	return false
}

// componentOf builds the platform's component identity from Anchore's package
// fields.
//
// # Why the identity carries no version, and why the purl is not used raw
//
// Component.ID is what two RELEASES are aligned on, so a version in it would
// make every package bump read as one finding resolved and one introduced - a
// patch release reporting a fix it did not make. A package URL carries the
// version, so using it as the identity would do exactly that.
//
// The shape is `<type>://<name>`, which is the same shape the Xray provider
// normalizes to. That is not a coincidence and it is not cosmetic: it is what
// makes a merge across the two scanners find the same package rather than two,
// and it is the single most important line in this file for the enrichment to
// work at all.
func componentOf(v packageVuln) security.Component {
	name := strings.TrimSpace(v.PackageName)
	version := strings.TrimSpace(v.PackageVersion)
	if name == "" {
		// `package` is "<name>-<version>" and is all some builds send for a
		// language package. Better a component named after the whole string
		// than a finding with no package at all.
		name = strings.TrimSpace(v.Package)
	}
	typ := normalizeType(v.PackageType)

	c := security.Component{
		Name:    name,
		Version: version,
		Type:    typ,
		Path:    strings.TrimSpace(v.PackagePath),
	}
	if name != "" {
		c.ID = typ + "://" + name
	}
	return c
}

// normalizeType maps Anchore's package types onto the ecosystem names the
// platform already uses, so a merge across scanners aligns.
//
// Anchore is more specific than Xray in places - it distinguishes `apkg` from
// `APKG` and names Java archives by their packaging - and the mapping folds
// those onto the coarser vocabulary that both can express. Anything unknown is
// lower-cased and passed through: a type this build has not seen is better
// carried as the scanner spelled it than flattened to "generic", which would
// merge a Go module with a Ruby gem of the same name.
func normalizeType(raw string) string {
	t := strings.ToLower(strings.TrimSpace(raw))
	switch t {
	case "":
		return "generic"
	case "dpkg", "deb":
		return "deb"
	case "rpm":
		return "rpm"
	case "apkg", "apk":
		return "apk"
	case "java", "jar", "war", "ear", "java-archive", "maven":
		return "maven"
	case "npm", "node", "javascript":
		return "npm"
	case "python", "pypi", "wheel", "egg":
		return "pypi"
	case "gem", "ruby":
		return "gem"
	case "go-module", "golang", "go":
		return "go"
	case "nuget", "dotnet":
		return "nuget"
	case "binary", "os":
		return "generic"
	default:
		return t
	}
}

// summaryOf is the one line a table row shows.
//
// The advisory identifier and the package, because that is what a row is: the
// description is a paragraph and belongs in the detail tier, and a summary
// column filled with the first eighty characters of an NVD paragraph is a
// column nobody reads twice.
func summaryOf(v packageVuln) string {
	pkg := strings.TrimSpace(v.PackageName)
	if pkg == "" {
		pkg = strings.TrimSpace(v.Package)
	}
	switch {
	case pkg == "":
		return v.Vuln
	case v.Vuln == "":
		return pkg
	default:
		return v.Vuln + " in " + pkg
	}
}

// descriptionOf is the advisory's prose, from wherever this response carries
// it.
//
// The top-level `description` is populated only when the request asked for it;
// the NVD entry carries one regardless. Preferring the top-level one and
// falling back is what makes the prose present on both.
func descriptionOf(v packageVuln) string {
	if d := strings.TrimSpace(v.Description); d != "" {
		return d
	}
	for _, nvd := range v.NVDData {
		if d := strings.TrimSpace(nvd.Description); d != "" {
			return d
		}
	}
	return ""
}

// upperCVE normalizes a public identifier, and leaves anything else alone.
//
// Only CVE-shaped identifiers are upper-cased. Anchore reports advisory ids
// from a dozen feeds - GHSA-xxxx, ALAS2-2023-xxx, ELSA-xxxx - and upper-casing
// a GHSA identifier would produce a string that does not match the one every
// other system uses.
func upperCVE(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(strings.ToUpper(id), "CVE-") {
		return strings.ToUpper(id)
	}
	return ""
}

// normalizeVersionReport turns the application-version report into findings,
// keyed by the image each match landed in.
//
// # What this adds over the per-image responses
//
// Not more findings - the same vulnerabilities, aggregated the other way. What
// it adds is the RELEASE-LEVEL grading: the version report carries the vendor
// and NVD entries once per advisory rather than once per package, and on a
// release where a base-image CVE lands in forty images that is one grading
// instead of forty copies of it.
//
// It is used to ENRICH what the image responses already produced, never to
// replace them: the image view is the one with the package path, the fix
// observed time and the base-image inheritance, and the guide is explicit that
// the application view must not automatically replace the image dataset.
func normalizeVersionReport(res versionVulnerabilities) map[string][]security.Finding {
	out := map[string][]security.Finding{}
	for _, v := range res.Vulnerabilities {
		severity, observations, description, url, willNotFix := versionGrading(v)
		for _, m := range v.Matches {
			digest := strings.TrimSpace(m.Location.Artifact.ID)
			if digest == "" || !strings.EqualFold(m.Location.Artifact.Type, "image") {
				continue
			}
			fixed := fixVersions(m.Fix)
			f := security.Finding{
				CVE:         upperCVE(v.ID),
				ID:          v.ID,
				Severity:    severity,
				Description: description,
				Component: security.Component{
					ID:      normalizeType(m.Location.Package.Type) + "://" + m.Location.Package.Name,
					Name:    m.Location.Package.Name,
					Version: m.Location.Package.Version,
					Type:    normalizeType(m.Location.Package.Type),
					Path:    m.Location.Package.Location,
				},
				FixedIn:      fixed,
				Fixable:      len(fixed) > 0,
				WillNotFix:   willNotFix,
				Provider:     ProviderName,
				Sources:      []string{ProviderName},
				Observations: observations,
			}
			if m.Location.Package.Name == "" {
				f.Component.ID = ""
			}
			f.Summary = strings.TrimSpace(v.ID + " in " + m.Location.Package.Name)
			if url != "" {
				f.References = []string{url}
			}
			out[digest] = append(out[digest], f)
		}
	}
	return out
}

// versionGrading reads one advisory's gradings out of the version report.
func versionGrading(v versionVuln) (
	severity security.Severity, observations []security.Observation,
	description, url string, willNotFix bool,
) {
	severity = security.SeverityUnknown
	consider := func(sev, source, desc, link string, cvss *versionCVSS) {
		parsed := security.ParseSeverity(sev)
		if parsed.Rank() > severity.Rank() {
			severity = parsed
		}
		score, vector := 0.0, ""
		if cvss != nil {
			score, vector = bestScore(cvss.CVSSV3, cvss.CVSSV2)
		}
		if sev != "" || score > 0 {
			observations = append(observations, security.Observation{
				Provider: ProviderName, Source: source, Severity: parsed,
				Score: score, Vector: vector,
			})
		}
		if description == "" {
			description = strings.TrimSpace(desc)
		}
		if url == "" {
			url = strings.TrimSpace(link)
		}
	}

	if v.VendorData != nil {
		consider(v.VendorData.Severity,
			sourceLabel(v.VendorData.Feed, v.VendorData.Group, "vendor"),
			v.VendorData.Description, v.VendorData.URL, v.VendorData.CVSS)
		willNotFix = v.VendorData.WillNotFix
	}
	for _, nvd := range v.NVD {
		consider(nvd.Severity, "nvd", nvd.Description, nvd.URL, nvd.CVSS)
	}
	return severity, observations, description, url, willNotFix
}
