package security

import (
	"context"
	"testing"
	"time"
)

// The merge is the feature. These tests are about what it must NOT lose.

func srcFinding(cve, component, severity string, opts ...func(*Finding)) Finding {
	f := Finding{
		CVE:      cve,
		Severity: Severity(severity),
		Component: Component{
			ID: "deb://" + component, Name: component, Version: "1.0", Type: "deb",
		},
	}
	for _, o := range opts {
		o(&f)
	}
	return f
}

func from(provider string, findings ...Finding) []Report {
	for i := range findings {
		findings[i].Provider = provider
		findings[i].Sources = []string{provider}
	}
	return []Report{{
		Artifact: ArtifactRef{Name: "app", Digest: "sha256:aaa", Kind: "image"},
		Status:   StatusScanned,
		Provider: provider,
		Findings: findings,
	}}
}

// One CVE on one package from two scanners is ONE row that knows both.
func TestMergeCollapsesTheSameFinding(t *testing.T) {
	merged := MergeReports(
		from("jfrog-xray", srcFinding("CVE-2024-1", "openssl", "high")),
		from("anchore", srcFinding("CVE-2024-1", "openssl", "critical")),
	)
	if len(merged) != 1 {
		t.Fatalf("expected one report, got %d", len(merged))
	}
	if got := len(merged[0].Findings); got != 1 {
		t.Fatalf("expected one merged finding, got %d", got)
	}
	f := merged[0].Findings[0]
	if f.Severity != SeverityCritical {
		t.Errorf("severity = %q, want the worse of the two", f.Severity)
	}
	if len(f.Sources) != 2 {
		t.Errorf("sources = %v, want both scanners", f.Sources)
	}
}

// The enrichment: what one scanner knows and the other does not must survive.
func TestMergeKeepsWhatEitherScannerKnew(t *testing.T) {
	merged := MergeReports(
		from("jfrog-xray", srcFinding("CVE-2024-1", "openssl", "high", func(f *Finding) {
			f.CVSSVector = "CVSS:3.1/AV:N"
			f.CVSSScore = 7.5
		})),
		from("anchore", srcFinding("CVE-2024-1", "openssl", "high", func(f *Finding) {
			f.Description = "A use-after-free in the TLS handshake."
			f.FixedIn = []string{"1.1.1n-1"}
			f.Fixable = true
			f.KEV = true
			f.KEVSource = "anchore"
			f.EPSS = &EPSS{Score: 0.9, Percentile: 0.98}
		})),
	)
	f := merged[0].Findings[0]
	if f.CVSSVector == "" {
		t.Error("lost the CVSS vector one scanner supplied")
	}
	if f.Description == "" {
		t.Error("lost the description the other scanner supplied")
	}
	if !f.Fixable || len(f.FixedIn) != 1 {
		t.Error("lost the fix version - the one direction of error that costs somebody an upgrade")
	}
	if !f.KEV || f.KEVSource != "anchore" {
		t.Errorf("lost the known-exploited flag or its claimant: %+v", f)
	}
	if f.EPSS == nil {
		t.Error("lost the EPSS score")
	}
}

// A CVE on two different packages is two things to upgrade, not one.
func TestMergeDoesNotCollapseAcrossPackages(t *testing.T) {
	merged := MergeReports(
		from("jfrog-xray",
			srcFinding("CVE-2024-1", "openssl", "high"),
			srcFinding("CVE-2024-1", "libssl3", "high")),
	)
	if got := len(merged[0].Findings); got != 2 {
		t.Fatalf("expected two findings for one CVE on two packages, got %d", got)
	}
}

// One scanner having analysed an image and the other not must not report the
// image as unscanned.
func TestMergeTakesTheBestStatus(t *testing.T) {
	analysed := from("anchore", srcFinding("CVE-2024-1", "openssl", "high"))
	pending := []Report{{
		Artifact: ArtifactRef{Name: "app", Digest: "sha256:aaa", Kind: "image"},
		Status:   StatusNotScanned,
		Provider: "jfrog-xray",
		Message:  "JFrog Xray has not indexed it yet.",
	}}

	merged := MergeReports(pending, analysed)
	if merged[0].Status != StatusScanned {
		t.Fatalf("status = %q, want scanned - one scanner has a complete answer", merged[0].Status)
	}
	if len(merged[0].Findings) != 1 {
		t.Errorf("lost the findings of the scanner that did answer")
	}
}

// One scanner is the common case and must cost nothing.
func TestMergeOfOneSourceIsIdentity(t *testing.T) {
	in := from("jfrog-xray", srcFinding("CVE-2024-1", "openssl", "high"))
	out := MergeReports(in)
	if len(out) != 1 || len(out[0].Findings) != 1 {
		t.Fatalf("a single source was altered by the merge: %+v", out)
	}
}

// The question a second scanner exists to make askable.
func TestCompareSourcesFindsWhatOnlyOneSaw(t *testing.T) {
	cmp := CompareSources([]SourceReports{
		{Provider: "jfrog-xray", Reports: from("jfrog-xray",
			srcFinding("CVE-2024-1", "openssl", "high"),
			srcFinding("CVE-2024-2", "zlib", "medium"))},
		{Provider: "anchore", Reports: from("anchore",
			srcFinding("CVE-2024-1", "openssl", "high"),
			srcFinding("CVE-2024-3", "curl", "critical", func(f *Finding) { f.KEV = true }))},
	})

	if cmp.SharedCount != 1 {
		t.Errorf("shared = %d, want 1", cmp.SharedCount)
	}
	if got := cmp.OnlyIn["jfrog-xray"]; len(got) != 1 || got[0] != "CVE-2024-2" {
		t.Errorf("only-in-Xray = %v, want [CVE-2024-2]", got)
	}
	if got := cmp.OnlyIn["anchore"]; len(got) != 1 || got[0] != "CVE-2024-3" {
		t.Errorf("only-in-Anchore = %v, want [CVE-2024-3]", got)
	}
	// The number that decides whether a scanner stays switched on.
	if got := cmp.KEVOnlyIn["anchore"]; len(got) != 1 {
		t.Errorf("Anchore's exclusive KEVs = %v, want one", got)
	}
	if cmp.Counts["anchore"].KEVOnly != 1 {
		t.Errorf("anchore KEVOnly = %d, want 1", cmp.Counts["anchore"].KEVOnly)
	}
}

// A scanner that found nothing new but explained several thousand findings
// better has still earned its place, and the comparison has to be able to say
// so.
func TestCompareSourcesCountsEnrichment(t *testing.T) {
	cmp := CompareSources([]SourceReports{
		{Provider: "jfrog-xray", Reports: from("jfrog-xray",
			srcFinding("CVE-2024-1", "openssl", "high"))},
		{Provider: "anchore", Reports: from("anchore",
			srcFinding("CVE-2024-1", "openssl", "high", func(f *Finding) {
				f.Description = "the paragraph Xray did not have"
				f.KEV = true
			}))},
	})
	if cmp.Counts["anchore"].Only != 0 {
		t.Fatalf("anchore reported nothing exclusive, got Only=%d", cmp.Counts["anchore"].Only)
	}
	if cmp.Counts["anchore"].Enriched != 1 {
		t.Errorf("enriched = %d, want 1 - the defence of a scanner whose Only count is zero",
			cmp.Counts["anchore"].Enriched)
	}
}

// KEV is counted per ADVISORY at the release level, and per occurrence in the
// work estimate. Both, because they answer different questions.
func TestPostureCountsKEVsDistinctly(t *testing.T) {
	base := srcFinding("CVE-2024-1", "openssl", "high", func(f *Finding) { f.KEV = true })
	reports := []Report{
		{Artifact: ArtifactRef{Name: "a", Digest: "sha256:a"}, Status: StatusScanned,
			Provider: "anchore", Findings: []Finding{base}},
		{Artifact: ArtifactRef{Name: "b", Digest: "sha256:b"}, Status: StatusScanned,
			Provider: "anchore", Findings: []Finding{base}},
	}
	for i := range reports {
		reports[i].Recount()
	}

	p := Summarize(reports)
	if p.KEVs != 1 {
		t.Errorf("distinct KEVs = %d, want 1 - one advisory in two images is one advisory", p.KEVs)
	}
	if p.Counts.KEV != 2 {
		t.Errorf("occurrences = %d, want 2 - two places to fix it", p.Counts.KEV)
	}
}

// Every scanner is asked, and one refusing must not lose the other's answer.
func TestPosturesSurvivesOneScannerRefusing(t *testing.T) {
	svc := NewService(multiResolver{
		providers: map[string]Provider{
			"good": multiStubProvider{name: "good", findings: 3},
			"bad":  nil, // resolves to an error
		},
	}, nil, nil)

	res, err := svc.Postures(context.Background(), MultiRequest{
		Providers: []string{"good", "bad"},
		Request: Request{
			Scope:     Scope{Product: "p", Repository: "r"},
			Artifacts: []ArtifactRef{{Name: "app", Digest: "sha256:aaa", Kind: "image"}},
			Detail:    true,
		},
	})
	if err != nil {
		t.Fatalf("postures: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Errorf("expected the refusing scanner to be recorded, got %v", res.Failures)
	}
	if res.Posture.Counts.Total != 3 {
		t.Errorf("lost the working scanner's findings: %d", res.Posture.Counts.Total)
	}
	if !res.Enabled() {
		t.Error("a release one scanner answered for reported no scanner enabled")
	}
}

type multiResolver struct{ providers map[string]Provider }

func (m multiResolver) ProviderFor(_ context.Context, scope Scope) (Provider, error) {
	p, ok := m.providers[scope.Provider]
	if !ok || p == nil {
		return nil, context.DeadlineExceeded
	}
	return p, nil
}

func (m multiResolver) ProvidersFor(context.Context, string, string) ([]string, error) {
	out := make([]string, 0, len(m.providers))
	for name := range m.providers {
		out = append(out, name)
	}
	return out, nil
}

type multiStubProvider struct {
	name     string
	findings int
}

func (s multiStubProvider) Name() string  { return s.name }
func (s multiStubProvider) Enabled() bool { return true }

func (s multiStubProvider) Scan(_ context.Context, refs []ArtifactRef, _ ScanOptions) ([]Report, error) {
	out := make([]Report, 0, len(refs))
	for _, ref := range refs {
		r := Report{Artifact: ref, Status: StatusScanned, Provider: s.name, RetrievedAt: time.Now().UTC()}
		for i := range s.findings {
			r.Findings = append(r.Findings, Finding{
				CVE:       "CVE-2024-" + string(rune('1'+i)),
				Severity:  SeverityHigh,
				Component: Component{ID: "deb://pkg", Name: "pkg", Version: "1.0", Type: "deb"},
				Provider:  s.name,
			})
		}
		r.Recount()
		out = append(out, r)
	}
	return out, nil
}
