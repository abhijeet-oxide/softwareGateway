package security

import "testing"

// The count and the stored rows have to come from one list.
//
// A release reported 90,808 findings on its listing row and 86,085 on its own
// security tab, from one sync. The listing quoted what was summed in memory;
// the tab counted the rows that reached the database, whose key did not carry
// the package VERSION - so an image holding two builds of one package wrote one
// row where the sum counted two. This is the collapse that makes the two agree.
func TestDedupeFindingsCollapsesTheStoredRow(t *testing.T) {
	openssl := func(version string, fixedIn []string) Finding {
		return Finding{
			CVE:       "CVE-2026-31789",
			ID:        "XRAY-964095",
			Severity:  SeverityCritical,
			Component: Component{ID: "alpine://libcrypto3", Name: "libcrypto3", Version: version},
			FixedIn:   fixedIn,
			Fixable:   len(fixedIn) > 0,
			Provider:  "jfrog-xray",
		}
	}

	got := DedupeFindings([]Finding{
		// The same advisory against the same package at the same version,
		// reached by two impact paths. One row, and it was being counted twice.
		openssl("3.5.5-r0", []string{"3.5.6-r0"}),
		openssl("3.5.5-r0", nil),
		// The same package at a DIFFERENT version. Two things to upgrade, and
		// collapsing them is what lost 4,723 findings.
		openssl("3.1.4-r2", []string{"3.5.6-r0"}),
	})

	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (one per package version)", len(got))
	}
	for _, f := range got {
		if !f.Fixable {
			t.Errorf("%s at %s lost its fix to deduplication",
				f.CVE, f.Component.Version)
		}
	}
}

// The union of what the copies knew, not the first one's answer.
//
// Xray reports the same finding by two impact paths and only one of them
// carries the fixed version. Keeping the first would report a fixable finding
// as unfixable, which is the one direction this must never be wrong in.
func TestDedupeFindingsKeepsTheWorstGradeAndEveryFix(t *testing.T) {
	base := Finding{CVE: "CVE-1", Component: Component{ID: "deb://openssl", Version: "1.1.1n"}}

	quiet := base
	quiet.Severity = SeverityLow

	loud := base
	loud.Severity = SeverityCritical
	loud.FixedIn = []string{"1.1.1w"}
	loud.Fixable = true
	loud.Sources = []string{"anchore"}

	quiet.Sources = []string{"jfrog-xray"}

	got := DedupeFindings([]Finding{quiet, loud})
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Severity != SeverityCritical {
		t.Errorf("severity = %q, want the worst of the two", got[0].Severity)
	}
	if !got[0].Fixable || len(got[0].FixedIn) != 1 {
		t.Errorf("the fix was lost: fixable=%t fixedIn=%v", got[0].Fixable, got[0].FixedIn)
	}
	if len(got[0].Sources) != 2 {
		t.Errorf("sources = %v, want both scanners", got[0].Sources)
	}
}

// Three numbers, three questions, and none of them wearing another's name.
func TestSummarizeCountsFindingsPairsAndAdvisoriesSeparately(t *testing.T) {
	finding := func(cve, pkg string) Finding {
		return Finding{
			CVE: cve, Severity: SeverityHigh, Provider: "jfrog-xray",
			Component: Component{ID: "deb://" + pkg, Name: pkg},
		}
	}
	report := func(name string, fs ...Finding) Report {
		r := Report{
			Artifact: ArtifactRef{Name: name, Digest: "sha256:" + name},
			Status:   StatusScanned, Provider: "jfrog-xray", Findings: fs,
		}
		r.Recount()
		return r
	}

	// One advisory, two packages, two images: four findings, two pairs, one CVE.
	p := Summarize([]Report{
		report("a", finding("CVE-1", "openssl"), finding("CVE-1", "libssl3")),
		report("b", finding("CVE-1", "openssl"), finding("CVE-1", "libssl3")),
	})

	if p.Counts.Total != 4 {
		t.Errorf("counts.total = %d, want 4 (every occurrence)", p.Counts.Total)
	}
	if p.UniqueCounts.Total != 2 {
		t.Errorf("uniqueCounts.total = %d, want 2 (CVE and package pairs)", p.UniqueCounts.Total)
	}
	if p.UniqueCVEs != 1 {
		t.Errorf("uniqueCVEs = %d, want 1 (one advisory to read)", p.UniqueCVEs)
	}
}

func TestSummarizeCanonicalCountsAgreeAcrossEveryBreakdown(t *testing.T) {
	reports := []Report{
		report("one", "sha256:1",
			finding("CVE-1", SeverityHigh, "openssl", true),
			finding("CVE-2", SeverityMedium, "zlib", false),
		),
		report("two", "sha256:2",
			finding("CVE-1", SeverityCritical, "openssl", false),
			finding("CVE-1", SeverityHigh, "libssl3", true),
		),
	}

	p := Summarize(reports)
	if p.Counts.Total != 4 || p.Counts.Fixable != 2 || p.Counts.NonFixable != 2 {
		t.Fatalf("all findings = %+v, want total 4, fixable 2, non-fixable 2", p.Counts)
	}
	if p.UniqueCounts.Total != 3 || p.UniqueCounts.Fixable != 2 || p.UniqueCounts.NonFixable != 1 {
		t.Fatalf("unique CVE/component pairs = %+v, want total 3, fixable 2, non-fixable 1", p.UniqueCounts)
	}
	if p.UniqueCVEs != 2 || p.UniqueCVECounts.Total != 2 || p.UniqueCVECounts.Fixable != 1 || p.UniqueCVECounts.NonFixable != 1 {
		t.Fatalf("unique advisories = %d / %+v, want total 2, fixable 1, non-fixable 1", p.UniqueCVEs, p.UniqueCVECounts)
	}
	if p.UniqueCVECounts.BySeverity.Critical != 1 || p.UniqueCVECounts.BySeverity.Medium != 1 {
		t.Errorf("unique advisory severities = %+v, want one critical and one medium", p.UniqueCVECounts.BySeverity)
	}
	if p.UniqueCVECounts.FixableBySeverity.Critical != 1 || p.UniqueCVECounts.FixableBySeverity.Medium != 0 {
		t.Errorf("unique advisory fixable severities = %+v, want one critical and no medium", p.UniqueCVECounts.FixableBySeverity)
	}
	for _, counts := range []Counts{p.Counts, p.UniqueCounts, p.UniqueCVECounts} {
		if counts.Total != counts.BySeverity.Critical+counts.BySeverity.High+counts.BySeverity.Medium+counts.BySeverity.Low+counts.BySeverity.Unknown {
			t.Errorf("total does not equal severity sum: %+v", counts)
		}
		if counts.Fixable != counts.FixableBySeverity.Critical+counts.FixableBySeverity.High+counts.FixableBySeverity.Medium+counts.FixableBySeverity.Low+counts.FixableBySeverity.Unknown {
			t.Errorf("fixable does not equal severity sum: %+v", counts)
		}
	}
}

// One scanner is not a comparison, so there is nothing to draw a toggle from.
func TestSummarizeOmitsTheBreakdownForOneScanner(t *testing.T) {
	r := Report{
		Artifact: ArtifactRef{Name: "a", Digest: "sha256:a"},
		Status:   StatusScanned, Provider: "jfrog-xray",
		Findings: []Finding{{CVE: "CVE-1", Severity: SeverityHigh, Provider: "jfrog-xray"}},
	}
	r.Recount()
	if got := Summarize([]Report{r}); len(got.BySource) != 0 {
		t.Errorf("bySource = %v on a single-scanner release; want it empty", got.BySource)
	}
}

// Two scanners, and the number the comparison exists for.
func TestSummarizeCountsWhatOnlyOneScannerSaw(t *testing.T) {
	shared := Finding{
		CVE: "CVE-1", Severity: SeverityHigh, Provider: "jfrog-xray",
		Sources: []string{"jfrog-xray", "anchore"},
	}
	xrayOnly := Finding{
		CVE: "CVE-2", Severity: SeverityLow, Provider: "jfrog-xray",
		Sources: []string{"jfrog-xray"},
	}
	anchoreOnly := Finding{
		CVE: "CVE-3", Severity: SeverityLow, Provider: "anchore",
		Sources: []string{"anchore"},
	}

	xray := Report{
		Artifact: ArtifactRef{Name: "a", Digest: "sha256:a"},
		Status:   StatusScanned, Provider: "jfrog-xray",
		Findings: []Finding{shared, xrayOnly},
	}
	xray.Recount()
	anchore := Report{
		Artifact: ArtifactRef{Name: "a", Digest: "sha256:a2"},
		Status:   StatusScanned, Provider: "anchore",
		Findings: []Finding{shared, anchoreOnly},
	}
	anchore.Recount()

	p := Summarize([]Report{xray, anchore})
	if len(p.BySource) != 2 {
		t.Fatalf("bySource has %d entries, want 2", len(p.BySource))
	}
	byName := map[string]SourceCounts{}
	for _, src := range p.BySource {
		byName[src.Provider] = src
	}
	if got := byName["jfrog-xray"].OnlyHere; got != 1 {
		t.Errorf("jfrog-xray onlyHere = %d, want 1 (CVE-2)", got)
	}
	if got := byName["anchore"].OnlyHere; got != 1 {
		t.Errorf("anchore onlyHere = %d, want 1 (CVE-3)", got)
	}
	if got := byName["jfrog-xray"].UniqueCVEs; got != 2 {
		t.Errorf("jfrog-xray uniqueCVEs = %d, want 2", got)
	}
}
