package security

import (
	"strings"
	"testing"
	"time"
)

// scannedAt is a fixed time so a report's freshness never varies between runs.
var scannedAt = time.Date(2026, 6, 29, 1, 14, 0, 0, time.UTC)

// report builds a scanned artifact with the given findings.
func report(name, digest string, findings ...Finding) Report {
	r := Report{
		Artifact:    ArtifactRef{Name: name, Digest: digest, Tag: "t", Kind: "image"},
		Status:      StatusScanned,
		Provider:    "jfrog-xray",
		Findings:    findings,
		ScannedAt:   &scannedAt,
		RetrievedAt: scannedAt,
	}
	r.Recount()
	return r
}

func finding(cve string, sev Severity, pkg string, fixable bool) Finding {
	return Finding{
		CVE:       cve,
		Severity:  sev,
		Fixable:   fixable,
		Provider:  "jfrog-xray",
		Component: Component{ID: "deb://" + pkg, Name: pkg, Version: "1.0", Type: "deb"},
	}
}

func TestCompareResolvedAndIntroduced(t *testing.T) {
	a := []Report{report("main", "sha256:a",
		finding("CVE-1", SeverityHigh, "openssl", true),
		finding("CVE-2", SeverityHigh, "zlib", true),
		finding("CVE-3", SeverityHigh, "curl", true),
		finding("CVE-4", SeverityMedium, "nginx", true),
		finding("CVE-5", SeverityMedium, "busybox", true),
		finding("CVE-6", SeverityMedium, "glibc", true),
		finding("CVE-7", SeverityMedium, "pcre", true),
		finding("CVE-8", SeverityMedium, "expat", true),
	)}
	b := []Report{report("main", "sha256:b",
		finding("CVE-9", SeverityLow, "libxml", false),
	)}

	got := Compare(CompareInput{A: a, B: b, NameA: "Release A", NameB: "Release B"})

	if got.Verdict != VerdictBetter {
		t.Fatalf("verdict = %q, want better (score %d)", got.Verdict, got.NetScore)
	}
	if got.Resolved.Total != 8 || got.Introduced.Total != 1 {
		t.Fatalf("resolved=%d introduced=%d, want 8 and 1", got.Resolved.Total, got.Introduced.Total)
	}
	// The sentence in the requirement, near enough word for word.
	want := "Release B is better than Release A. It resolves 3 high and 5 medium vulnerabilities and introduces 1 low vulnerability."
	if got.Explanation != want {
		t.Errorf("explanation:\n got %q\nwant %q", got.Explanation, want)
	}
}

func TestCompareWorseWhenCriticalIntroduced(t *testing.T) {
	a := []Report{report("main", "sha256:a", finding("CVE-1", SeverityLow, "a", false))}
	b := []Report{report("main", "sha256:b",
		finding("CVE-1", SeverityLow, "a", false),
		finding("CVE-2", SeverityCritical, "b", true),
	)}

	got := Compare(CompareInput{A: a, B: b})
	if got.Verdict != VerdictWorse {
		t.Fatalf("verdict = %q, want worse", got.Verdict)
	}
	if got.Unchanged.Total != 1 {
		t.Errorf("unchanged = %d, want 1", got.Unchanged.Total)
	}
	if got.Introduced.BySeverity.Critical != 1 {
		t.Errorf("introduced critical = %d, want 1", got.Introduced.BySeverity.Critical)
	}
}

// One critical resolved must outweigh any number of lows introduced. A linear
// scale gets this wrong, and getting it wrong tells a release manager to ship
// the worse release.
func TestCompareOneCriticalOutweighsManyLows(t *testing.T) {
	old := []Finding{finding("CVE-CRIT", SeverityCritical, "openssl", true)}
	var lows []Finding
	for i := 0; i < 50; i++ {
		lows = append(lows, finding("CVE-LOW-"+string(rune('a'+i%26))+string(rune('a'+i/26)), SeverityLow, "pkg", false))
	}

	got := Compare(CompareInput{
		A: []Report{report("main", "sha256:a", old...)},
		B: []Report{report("main", "sha256:b", lows...)},
	})
	if got.Verdict != VerdictBetter {
		t.Fatalf("verdict = %q (score %d), want better", got.Verdict, got.NetScore)
	}
}

func TestCompareSeverityChange(t *testing.T) {
	a := []Report{report("main", "sha256:a", finding("CVE-1", SeverityMedium, "openssl", false))}
	b := []Report{report("main", "sha256:b", finding("CVE-1", SeverityCritical, "openssl", false))}

	got := Compare(CompareInput{A: a, B: b})
	if got.Verdict != VerdictWorse {
		t.Fatalf("verdict = %q, want worse", got.Verdict)
	}
	if got.SeverityIncreased.Total != 1 {
		t.Fatalf("severityIncreased = %d, want 1", got.SeverityIncreased.Total)
	}
	// The re-grade contributes the difference, not the whole new weight.
	if want := SeverityCritical.Weight() - SeverityMedium.Weight(); got.NetScore != want {
		t.Errorf("netScore = %d, want %d", got.NetScore, want)
	}
	if len(got.Changes) != 1 || got.Changes[0].FromSeverity != SeverityMedium || got.Changes[0].ToSeverity != SeverityCritical {
		t.Errorf("change = %+v, want medium -> critical", got.Changes)
	}
}

func TestCompareRemediationChangeIsNotSeverityChange(t *testing.T) {
	a := []Report{report("main", "sha256:a", finding("CVE-1", SeverityHigh, "openssl", false))}
	b := []Report{report("main", "sha256:b", finding("CVE-1", SeverityHigh, "openssl", true))}

	got := Compare(CompareInput{A: a, B: b})
	if got.RemediationChanged.Total != 1 {
		t.Fatalf("remediationChanged = %d, want 1", got.RemediationChanged.Total)
	}
	if got.Verdict != VerdictUnchanged {
		t.Errorf("verdict = %q, want unchanged: a fix becoming available is not a posture change", got.Verdict)
	}
}

// Two releases do not contain the same artifacts. Ten images in the base, two
// in the patch: the eight untouched ones must not read as removals.
func TestCompareArtifactDelta(t *testing.T) {
	var a, b []Report
	for _, n := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		a = append(a, report(n, "sha256:"+n))
		b = append(b, report(n, "sha256:"+n))
	}
	a = append(a, report("patched", "sha256:old", finding("CVE-1", SeverityHigh, "openssl", true)))
	b = append(b, report("patched", "sha256:new"))
	b = append(b, report("brandnew", "sha256:brandnew", finding("CVE-2", SeverityLow, "libxml", false)))

	got := Compare(CompareInput{A: a, B: b})

	if got.ArtifactSummary.Common != 8 {
		t.Errorf("common = %d, want 8", got.ArtifactSummary.Common)
	}
	if got.ArtifactSummary.Upgraded != 1 {
		t.Errorf("upgraded = %d, want 1", got.ArtifactSummary.Upgraded)
	}
	if got.ArtifactSummary.Added != 1 {
		t.Errorf("added = %d, want 1", got.ArtifactSummary.Added)
	}
	if got.ArtifactSummary.Removed != 0 {
		t.Errorf("removed = %d, want 0", got.ArtifactSummary.Removed)
	}
	// The finding on the added artifact is introduced, and says so.
	for _, ch := range got.Changes {
		if ch.CVE == "CVE-2" && ch.ArtifactChange != ArtifactAdded {
			t.Errorf("CVE-2 artifactChange = %q, want added", ch.ArtifactChange)
		}
	}
}

// A renamed artifact is one artifact, not a removal plus an addition. Without
// the digest rescue every finding it carries is counted twice, once each way.
func TestCompareRenameIsNotRemovalPlusAddition(t *testing.T) {
	a := []Report{report("old-name", "sha256:same", finding("CVE-1", SeverityHigh, "openssl", true))}
	b := []Report{report("new-name", "sha256:same", finding("CVE-1", SeverityHigh, "openssl", true))}

	got := Compare(CompareInput{A: a, B: b})
	if got.Resolved.Total != 0 || got.Introduced.Total != 0 {
		t.Fatalf("resolved=%d introduced=%d, want 0 and 0", got.Resolved.Total, got.Introduced.Total)
	}
	if got.Unchanged.Total != 1 {
		t.Errorf("unchanged = %d, want 1", got.Unchanged.Total)
	}
}

// A finding that left one artifact and appeared on another has moved, not been
// resolved. Counting it as resolved lets repackaging read as an improvement.
func TestCompareMovedFindingIsNotResolved(t *testing.T) {
	a := []Report{report("gone", "sha256:gone", finding("CVE-1", SeverityCritical, "openssl", true))}
	b := []Report{report("stayed", "sha256:stayed", finding("CVE-1", SeverityCritical, "openssl", true))}

	got := Compare(CompareInput{A: a, B: b})
	if got.Resolved.Total != 0 {
		t.Errorf("resolved = %d, want 0: the finding moved artifact", got.Resolved.Total)
	}
	if got.Verdict == VerdictBetter {
		t.Errorf("verdict = better, but nothing was fixed")
	}
}

// Removal counts as resolution only when the new release's coverage is
// complete. With an unscanned artifact in B, "the CVE is gone" may only mean
// "the CVE is where nobody looked".
func TestCompareRemovalNeedsCompleteCoverage(t *testing.T) {
	a := []Report{
		report("kept", "sha256:kept"),
		report("dropped", "sha256:dropped", finding("CVE-1", SeverityCritical, "openssl", true)),
	}

	complete := []Report{report("kept", "sha256:kept")}
	got := Compare(CompareInput{A: a, B: complete})
	if got.Resolved.Total != 1 || got.RemovedArtifact.Total != 0 {
		t.Fatalf("complete coverage: resolved=%d removedArtifact=%d, want 1 and 0",
			got.Resolved.Total, got.RemovedArtifact.Total)
	}
	if !got.Changes[0].ViaRemoval {
		t.Errorf("resolution should be marked as via removal")
	}

	partial := []Report{
		report("kept", "sha256:kept"),
		{Artifact: ArtifactRef{Name: "unknown", Digest: "sha256:unknown"}, Status: StatusNotScanned, Provider: "jfrog-xray"},
	}
	got = Compare(CompareInput{A: a, B: partial})
	if got.Resolved.Total != 0 || got.RemovedArtifact.Total != 1 {
		t.Fatalf("partial coverage: resolved=%d removedArtifact=%d, want 0 and 1",
			got.Resolved.Total, got.RemovedArtifact.Total)
	}
	if got.Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want inconclusive", got.Verdict)
	}
}

func TestCompareInconclusiveStates(t *testing.T) {
	scanned := []Report{report("main", "sha256:a", finding("CVE-1", SeverityHigh, "openssl", true))}
	disabled := []Report{{
		Artifact: ArtifactRef{Name: "main", Digest: "sha256:b"},
		Status:   StatusDisabled,
		Provider: "jfrog-xray",
		Message:  "Xray is not enabled for repository jfrog-prod",
	}}

	for _, tc := range []struct {
		name string
		a, b []Report
	}{
		{"new release not scanned", scanned, disabled},
		{"base release not scanned", disabled, scanned},
		{"neither scanned", disabled, disabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(CompareInput{A: tc.a, B: tc.b, NameA: "A", NameB: "B"})
			if got.Verdict != VerdictInconclusive {
				t.Fatalf("verdict = %q, want inconclusive", got.Verdict)
			}
			if len(got.Caveats) == 0 {
				t.Error("inconclusive with no caveat explaining why")
			}
			if !strings.Contains(got.Explanation, "cannot be determined") {
				t.Errorf("explanation = %q, want it to say the answer is not known", got.Explanation)
			}
		})
	}
}

// No difference over partial coverage is not "unchanged": it may be that the
// difference is in the part nobody scanned.
func TestCompareNoDifferenceOverPartialCoverageIsInconclusive(t *testing.T) {
	unscanned := Report{Artifact: ArtifactRef{Name: "other", Digest: "sha256:o"}, Status: StatusNotScanned}
	a := []Report{report("main", "sha256:a", finding("CVE-1", SeverityHigh, "openssl", true)), unscanned}
	b := []Report{report("main", "sha256:a", finding("CVE-1", SeverityHigh, "openssl", true)), unscanned}

	got := Compare(CompareInput{A: a, B: b})
	if got.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %q, want inconclusive", got.Verdict)
	}

	full := []Report{report("main", "sha256:a", finding("CVE-1", SeverityHigh, "openssl", true))}
	got = Compare(CompareInput{A: full, B: full})
	if got.Verdict != VerdictUnchanged {
		t.Fatalf("complete coverage: verdict = %q, want unchanged", got.Verdict)
	}
}

func TestSummarizeCoverageAndUniqueCounts(t *testing.T) {
	shared := finding("CVE-1", SeverityHigh, "openssl", true)
	reports := []Report{
		report("one", "sha256:1", shared),
		report("two", "sha256:2", shared),
		{Artifact: ArtifactRef{Name: "sig"}, Status: StatusUnsupported},
		{Artifact: ArtifactRef{Name: "late"}, Status: StatusNotScanned},
	}

	p := Summarize(reports)
	if p.Counts.Total != 2 {
		t.Errorf("counts.total = %d, want 2: the same CVE in two images is two things to fix", p.Counts.Total)
	}
	if p.UniqueCounts.Total != 1 {
		t.Errorf("uniqueCounts.total = %d, want 1", p.UniqueCounts.Total)
	}
	if p.Coverage.Scanned != 2 || p.Coverage.NotScanned != 1 || p.Coverage.Unsupported != 1 {
		t.Errorf("coverage = %+v", p.Coverage)
	}
	if p.Coverage.Scannable() != 3 {
		t.Errorf("scannable = %d, want 3: a signature is not an artifact Xray declined to scan", p.Coverage.Scannable())
	}
	if p.Coverage.Complete() {
		t.Error("coverage reported complete with an unscanned artifact")
	}
}

func TestFingerprintIsStableAndSensitive(t *testing.T) {
	a := []Report{report("one", "sha256:1", finding("CVE-1", SeverityHigh, "openssl", true))}
	b := []Report{report("one", "sha256:1", finding("CVE-1", SeverityHigh, "openssl", true))}
	if FingerprintReports(a) != FingerprintReports(b) {
		t.Error("identical reports produced different fingerprints")
	}

	c := []Report{report("one", "sha256:1", finding("CVE-1", SeverityCritical, "openssl", true))}
	if FingerprintReports(a) == FingerprintReports(c) {
		t.Error("a re-graded finding produced the same fingerprint")
	}
}

func TestParseSeverity(t *testing.T) {
	for in, want := range map[string]Severity{
		"Critical": SeverityCritical, "HIGH": SeverityHigh, "Moderate": SeverityMedium,
		"negligible": SeverityLow, "": SeverityUnknown, "banana": SeverityUnknown,
	} {
		if got := ParseSeverity(in); got != want {
			t.Errorf("ParseSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisabledProviderAnswersForEveryArtifact(t *testing.T) {
	d := Disabled{ProviderName: "jfrog-xray", Reason: "Xray is not enabled for repository jfrog-prod"}
	if d.Enabled() {
		t.Fatal("disabled provider reported itself enabled")
	}
	got, err := d.Scan(t.Context(), []ArtifactRef{{Name: "a"}, {Name: "b"}}, ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d reports, want 2", len(got))
	}
	for _, r := range got {
		if r.Status != StatusDisabled || r.Message == "" {
			t.Errorf("report = %+v, want a disabled status carrying the reason", r)
		}
	}
}

// The same bytes have one answer.
//
// A stored result is keyed by (scope, artifact), and two releases of one
// product are not always discovered in the same repository - so a comparison
// can hold a perfectly good scan of a digest on one side and nothing on the
// other, for bytes that are identical. Refusing to compare them produced a
// warning with no action in it: there is nothing to fix, because a digest is a
// hash of content and matching digests are the same software.
func TestCompareUsesOneSidesScanWhenTheDigestsMatch(t *testing.T) {
	shared := report("cfx-amf", "sha256:aaa",
		finding("CVE-2024-3094", SeverityCritical, "xz-utils", true),
		finding("CVE-2024-6387", SeverityHigh, "openssh", true),
	)
	// The same image, never scanned under this release's scope.
	unscanned := Report{
		Artifact: ArtifactRef{Name: "cfx-amf", Digest: "sha256:aaa", Tag: "t", Kind: "image"},
		Status:   StatusNotScanned,
		Provider: "jfrog-xray",
		Message:  "This artifact has not been included in a vulnerability sync yet.",
	}

	c := Compare(CompareInput{A: []Report{shared}, B: []Report{unscanned}})

	if c.ArtifactSummary.NotComparable != 0 {
		t.Fatalf("NotComparable = %d: identical bytes were refused a comparison",
			c.ArtifactSummary.NotComparable)
	}
	if len(c.Artifacts) != 1 || !c.Artifacts[0].Comparable {
		t.Fatalf("artifact delta = %+v, want one comparable pair", c.Artifacts)
	}
	// Nothing changed, because nothing did: it is one artifact.
	if c.Introduced.Total != 0 || c.Resolved.Total != 0 {
		t.Errorf("introduced %d / resolved %d, want none either way",
			c.Introduced.Total, c.Resolved.Total)
	}
	if c.Unchanged.Total != 2 {
		t.Errorf("unchanged = %d, want the two findings carried by both", c.Unchanged.Total)
	}
	for _, caveat := range c.Caveats {
		if strings.Contains(caveat, "no scan result yet") {
			t.Errorf("caveat still warns about a gap that does not exist: %q", caveat)
		}
	}
}

// It borrows only FROM a scan, and never invents one.
func TestCompareDoesNotInventAResultForAnUnscannedPair(t *testing.T) {
	unscanned := func() Report {
		return Report{
			Artifact: ArtifactRef{Name: "cfx-amf", Digest: "sha256:aaa", Tag: "t", Kind: "image"},
			Status:   StatusNotScanned,
			Provider: "jfrog-xray",
		}
	}

	c := Compare(CompareInput{A: []Report{unscanned()}, B: []Report{unscanned()}})

	if c.ArtifactSummary.NotComparable != 1 {
		t.Fatalf("NotComparable = %d, want 1: neither side has a result to share",
			c.ArtifactSummary.NotComparable)
	}
	if len(c.Changes) != 0 {
		t.Errorf("classified %d changes out of two unscanned artifacts", len(c.Changes))
	}
}

// Two different builds of one image is the real gap, and it stays reported -
// with a sentence a reader can act on.
func TestCompareStillReportsADifferentDigestWithNoScan(t *testing.T) {
	scanned := report("cfx-amf", "sha256:aaa", finding("CVE-2024-3094", SeverityCritical, "xz", true))
	other := Report{
		Artifact: ArtifactRef{Name: "cfx-amf", Digest: "sha256:bbb", Tag: "t2", Kind: "image"},
		Status:   StatusNotScanned,
		Provider: "jfrog-xray",
	}
	// A second image both releases DO have results for, so this is a gap in a
	// comparison rather than a release nobody has scanned - which is a
	// different sentence and gets one of its own.
	alsoA := report("cfx-smf", "sha256:ccc", finding("CVE-2024-6387", SeverityHigh, "openssh", true))
	alsoB := report("cfx-smf", "sha256:ccc", finding("CVE-2024-6387", SeverityHigh, "openssh", true))

	c := Compare(CompareInput{A: []Report{scanned, alsoA}, B: []Report{other, alsoB}})

	if c.ArtifactSummary.NotComparable != 1 {
		t.Fatalf("NotComparable = %d, want 1: different bytes need their own scan",
			c.ArtifactSummary.NotComparable)
	}
	var said string
	for _, caveat := range c.Caveats {
		if strings.Contains(caveat, "no scan result yet") {
			said = caveat
		}
	}
	if said == "" {
		t.Fatal("nothing told the reader an image was left out")
	}
	// Grammar, and an action. It read "1 artifacts ... could not be compared"
	// and stopped there.
	if strings.Contains(said, "1 images") || strings.Contains(said, "1 artifacts") {
		t.Errorf("caveat is ungrammatical for one image: %q", said)
	}
	if !strings.Contains(said, "Syncing") {
		t.Errorf("caveat states a problem without its fix: %q", said)
	}
}
