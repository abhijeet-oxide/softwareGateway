package anchore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// fakeAnchore is enough of Anchore Enterprise to drive the provider through
// every phase it has: submission, analysis, grouping and retrieval.
//
// Written rather than mocked because the interesting behaviour is the
// SEQUENCE - an image that is unknown, then analysing, then analysed - and a
// mock that returns a canned answer cannot express a state that changes.
type fakeAnchore struct {
	mu sync.Mutex
	// images maps digest to the analysis status it reports next. A status of
	// "" means Anchore has never heard of it.
	images map[string]string
	// analyzeAfter is how many polls an image spends analysing before it is
	// analysed. Zero means it is analysed as soon as it is submitted.
	analyzeAfter map[string]int
	submitted    []string
	associated   map[string]bool
	apps         map[string]string
	versions     map[string]string
	vulns        map[string]string
	calls        map[string]int
}

func newFake() *fakeAnchore {
	return &fakeAnchore{
		images:       map[string]string{},
		analyzeAfter: map[string]int{},
		associated:   map[string]bool{},
		apps:         map[string]string{},
		versions:     map[string]string{},
		vulns:        map[string]string{},
		calls:        map[string]int{},
	}
}

func (f *fakeAnchore) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAnchore) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v2")
	f.calls[r.Method+" "+path]++

	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case path == "/account":
		write(map[string]string{"name": "admin", "state": "enabled"})

	case path == "/images" && r.Method == http.MethodGet:
		items := []map[string]any{}
		for digest, status := range f.images {
			if status == "" {
				continue
			}
			// Each poll advances an image that is still analysing.
			if status == AnalysisAnalyzing {
				if left := f.analyzeAfter[digest]; left > 0 {
					f.analyzeAfter[digest] = left - 1
				} else {
					f.images[digest] = AnalysisAnalyzed
					status = AnalysisAnalyzed
				}
			}
			items = append(items, map[string]any{
				"image_digest":    digest,
				"analysis_status": status,
				"image_detail": []map[string]any{{
					"fulltag":     "internal.example.com/app:1.0",
					"analyzed_at": "2026-02-01T10:00:00Z",
				}},
			})
		}
		write(map[string]any{"items": items})

	case path == "/images" && r.Method == http.MethodPost:
		var req analysisRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		digest := digestOfPull(req.Source.Digest.PullString)
		f.submitted = append(f.submitted, digest)
		status := AnalysisAnalyzing
		if f.analyzeAfter[digest] == 0 {
			status = AnalysisAnalyzed
		}
		f.images[digest] = status
		write([]map[string]any{{"image_digest": digest, "analysis_status": status}})

	case path == "/applications" && r.Method == http.MethodGet:
		var out []map[string]string
		for name, id := range f.apps {
			out = append(out, map[string]string{"application_id": id, "name": name})
		}
		write(out)

	case path == "/applications" && r.Method == http.MethodPost:
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := "app-" + req["name"]
		f.apps[req["name"]] = id
		write(map[string]string{"application_id": id, "name": req["name"]})

	case strings.HasSuffix(path, "/versions") && r.Method == http.MethodGet:
		var out []map[string]string
		for name, id := range f.versions {
			out = append(out, map[string]string{"application_version_id": id, "version_name": name})
		}
		write(out)

	case strings.HasSuffix(path, "/versions") && r.Method == http.MethodPost:
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := "ver-" + req["version_name"]
		f.versions[req["version_name"]] = id
		write(map[string]string{"application_version_id": id, "version_name": req["version_name"]})

	case strings.HasSuffix(path, "/artifacts") && r.Method == http.MethodGet:
		var items []map[string]any
		for digest := range f.associated {
			items = append(items, map[string]any{
				"artifact_association_metadata": map[string]string{"association_id": "assoc-" + digest},
				"image":                         map[string]string{"image_digest": digest},
			})
		}
		write(map[string]any{"associated_image_artifacts": items})

	case strings.HasSuffix(path, "/artifacts") && r.Method == http.MethodPost:
		var req associationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.associated[req.ArtifactKeys["image_digest"]] = true
		write(map[string]any{})

	case strings.Contains(path, "/vuln/"):
		digest := strings.TrimPrefix(path, "/images/")
		digest = digest[:strings.Index(digest, "/vuln/")]
		body, ok := f.vulns[digest]
		if !ok {
			body = `{"image_digest":"` + digest + `","vulnerabilities":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))

	default:
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}
}

func digestOfPull(pull string) string {
	if i := strings.Index(pull, "@"); i >= 0 {
		return pull[i+1:]
	}
	return pull
}

func newProvider(t *testing.T, f *fakeAnchore, mutate func(*Settings)) *Provider {
	t.Helper()
	srv := f.server(t)
	settings := Settings{
		Enabled:      true,
		Registry:     "internal.example.com",
		Repository:   "vendor/app",
		Application:  "cfx-5000",
		Version:      "25.7.2131",
		Submit:       true,
		Concurrency:  4,
		AnalysisWait: 2 * time.Second,
		PollInterval: 10 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&settings)
	}
	p, err := New(Config{
		Endpoint: srv.URL, Username: "u", Password: "p", RequestTimeout: 5 * time.Second,
	}, settings)
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}
	provider, ok := p.(*Provider)
	if !ok {
		t.Fatalf("expected a live provider, got %T", p)
	}
	return provider
}

func imageRef(name, digest string) security.ArtifactRef {
	return security.ArtifactRef{
		Name: name, Tag: "1.0", Digest: digest,
		Repository: "vendor/app/" + name, Registry: "internal.example.com", Kind: "image",
	}
}

// The whole point of the integration: an image Anchore has never heard of is
// submitted, waited for, read, and grouped - in one sync.
func TestFirstSyncSubmitsWaitsAndReads(t *testing.T) {
	f := newFake()
	f.analyzeAfter["sha256:aaa"] = 1
	f.vulns["sha256:aaa"] = kevResponse

	p := newProvider(t, f, nil)
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:aaa")},
		security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("expected one report, got %d", len(reports))
	}
	r := reports[0]
	if r.Status != security.StatusScanned {
		t.Fatalf("expected the image to be scanned, got %q (%s)", r.Status, r.Message)
	}
	if len(f.submitted) != 1 {
		t.Errorf("expected the unknown image to be submitted once, got %d submissions", len(f.submitted))
	}
	if len(r.Findings) != 2 {
		t.Fatalf("expected two findings, got %d", len(r.Findings))
	}
	if !f.associated["sha256:aaa"] {
		t.Error("the analysed image was not associated with the application version")
	}
}

// The field that justifies the integration, and the sort order that acts on it.
func TestKEVIsReadAndSortsFirst(t *testing.T) {
	f := newFake()
	f.images["sha256:aaa"] = AnalysisAnalyzed
	f.vulns["sha256:aaa"] = kevResponse

	p := newProvider(t, f, nil)
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:aaa")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	findings := reports[0].Findings
	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %d", len(findings))
	}
	// The KEV is a MEDIUM and the other is a CRITICAL. Sorted, the exploited
	// one still comes first - that is the whole rule.
	if !findings[0].KEV {
		t.Errorf("expected the known-exploited finding first, got %s (%s)",
			findings[0].Identifier(), findings[0].Severity)
	}
	if findings[0].CVE != "CVE-2024-0001" {
		t.Errorf("expected CVE-2024-0001 first, got %q", findings[0].CVE)
	}
	if findings[0].EPSS == nil || findings[0].EPSS.Score < 0.9 {
		t.Errorf("expected the EPSS score to be carried, got %+v", findings[0].EPSS)
	}
	if reports[0].Counts.KEV != 1 {
		t.Errorf("expected one KEV counted, got %d", reports[0].Counts.KEV)
	}
}

// "None" is a string, not a version, and reading it as one turns every
// unfixable finding in a release into a fixable one.
func TestFixNoneIsNotAFixedVersion(t *testing.T) {
	for _, in := range []string{"", "None", "none", "N/A"} {
		if got := fixVersions(in); got != nil {
			t.Errorf("fixVersions(%q) = %v, want nothing", in, got)
		}
	}
	if got := fixVersions("1.2.3, 2.0.1"); len(got) != 2 {
		t.Errorf("fixVersions of two versions = %v, want both", got)
	}
}

// An image Anchore is still working on is NOT an image with no vulnerabilities.
func TestUnanalysedImageIsNotReportedClean(t *testing.T) {
	f := newFake()
	f.images["sha256:bbb"] = AnalysisAnalyzing
	f.analyzeAfter["sha256:bbb"] = 1000

	p := newProvider(t, f, func(s *Settings) { s.AnalysisWait = 30 * time.Millisecond })
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:bbb")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reports[0].Status != security.StatusNotScanned {
		t.Fatalf("expected not_scanned for an image still analysing, got %q", reports[0].Status)
	}
	if reports[0].Message == "" {
		t.Error("an unscanned image must say why")
	}
}

// A failed analysis is terminal and must not read as "waiting".
func TestFailedAnalysisIsUnavailable(t *testing.T) {
	f := newFake()
	f.images["sha256:ccc"] = AnalysisFailed

	p := newProvider(t, f, nil)
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:ccc")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reports[0].Status != security.StatusUnavailable {
		t.Fatalf("expected unavailable for a failed analysis, got %q", reports[0].Status)
	}
}

// A release that has not been replicated has nothing for Anchore to pull, and
// that is a transfer problem rather than a scanning one.
func TestUnreplicatedImageIsReportedMissing(t *testing.T) {
	f := newFake()
	p := newProvider(t, f, func(s *Settings) { s.Registry = "" })

	ref := imageRef("app", "sha256:ddd")
	ref.Registry = ""
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{ref}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !reports[0].Missing {
		t.Fatalf("expected the image to be reported missing, got %q (%s)",
			reports[0].Status, reports[0].Message)
	}
}

// Charts and signatures are not things Anchore declines to scan; they are
// things it has nothing to scan in.
func TestNonImagesAreUnsupported(t *testing.T) {
	f := newFake()
	p := newProvider(t, f, nil)

	chart := imageRef("chart", "sha256:eee")
	chart.Kind = "chart"
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{chart}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reports[0].Status != security.StatusUnsupported {
		t.Fatalf("expected unsupported for a chart, got %q", reports[0].Status)
	}
}

// A re-synced release must not resubmit images Anchore already holds.
func TestKnownImagesAreNotResubmitted(t *testing.T) {
	f := newFake()
	f.images["sha256:aaa"] = AnalysisAnalyzed
	f.vulns["sha256:aaa"] = kevResponse

	p := newProvider(t, f, nil)
	if _, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:aaa")},
		security.ScanOptions{Detail: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(f.submitted) != 0 {
		t.Errorf("an already-analysed image was submitted again: %v", f.submitted)
	}
}

// The endpoint an operator pastes may or may not already carry the version
// prefix, and appending a second one 404s every call.
func TestEndpointResolution(t *testing.T) {
	cases := map[string]string{
		"https://anchore.example.com":     "https://anchore.example.com/v2",
		"https://anchore.example.com/":    "https://anchore.example.com/v2",
		"https://anchore.example.com/v2":  "https://anchore.example.com/v2",
		"https://anchore.example.com/v2/": "https://anchore.example.com/v2",
		"anchore.example.com":             "https://anchore.example.com/v2",
	}
	for in, want := range cases {
		got, err := resolveEndpoint(in)
		if err != nil {
			t.Fatalf("resolveEndpoint(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("resolveEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// The component identity is what a merge across scanners aligns on, so it must
// carry the ecosystem and NOT the version.
func TestComponentIdentityExcludesVersion(t *testing.T) {
	c := componentOf(packageVuln{
		PackageName: "openssl", PackageVersion: "1.1.1n-0+deb11u3", PackageType: "dpkg",
	})
	if c.ID != "deb://openssl" {
		t.Errorf("component id = %q, want deb://openssl", c.ID)
	}
	if c.Version != "1.1.1n-0+deb11u3" {
		t.Errorf("component version = %q, want the version to survive on its own field", c.Version)
	}
}

// Two scanners disagreeing about one CVE is preserved, not resolved away.
func TestSeverityKeepsEveryObservation(t *testing.T) {
	severity, observations := severityFor(packageVuln{
		Severity: "Medium", Feed: "vulnerabilities", FeedGroup: "debian:11",
		NVDData:    []nvdData{{Source: "nvd", CVSSV3: &cvssScore{BaseScore: f64(9.8)}}},
		VendorData: []vendorData{{Source: "debian", CVSSV3: &cvssScore{BaseScore: f64(5.0)}}},
	})
	if severity != security.SeverityMedium {
		t.Errorf("severity = %q, want the reported grade", severity)
	}
	if len(observations) != 3 {
		t.Fatalf("expected three observations, got %d: %+v", len(observations), observations)
	}
}

func f64(v float64) *float64 { return &v }

// A vulnerability response with one known-exploited medium and one ordinary
// critical - the case the sort order exists for.
const kevResponse = `{
  "image_digest": "sha256:aaa",
  "vulnerability_type": "all",
  "vulnerabilities": [
    {
      "vuln": "CVE-2024-9999", "severity": "Critical", "fix": "None",
      "package": "libfoo-1.0", "package_name": "libfoo", "package_version": "1.0",
      "package_type": "dpkg",
      "nvd_data": [{"id": "CVE-2024-9999", "is_kev": false,
                    "cvss_v3": {"base_score": 9.8}}]
    },
    {
      "vuln": "CVE-2024-0001", "severity": "Medium", "fix": "2.0.1",
      "package": "libbar-1.2", "package_name": "libbar", "package_version": "1.2",
      "package_type": "dpkg",
      "nvd_data": [{"id": "CVE-2024-0001", "is_kev": true,
                    "cvss_v3": {"base_score": 5.4},
                    "epss": {"epss": 0.94, "percentile": 0.99}}]
    }
  ]
}`
