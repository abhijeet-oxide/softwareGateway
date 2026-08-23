package artifactory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// fakeXray is enough of Xray's summary API to exercise the boundary.
type fakeXray struct {
	*httptest.Server
	calls     atomic.Int32
	v1Calls   atomic.Int32
	v2Calls   atomic.Int32
	artifacts map[string]xrayArtifact // by bare sha256 hex
	// rawArtifacts are payloads captured from a real Xray, served verbatim.
	// Round-tripping our own structs would only prove they round-trip.
	rawArtifacts map[string]string
	// status, when non-zero, is returned instead of a summary.
	status int
	body   string
	// v2Status, when non-zero, is returned for the v2 path only - which is how
	// a platform that predates v2 behaves.
	v2Status int
	// authUser and authPass are what the server demands.
	authUser, authPass string
	// maxChecksums records the largest batch the client asked for.
	maxChecksums atomic.Int32
	// timeoutOver refuses any request asking about more than this many
	// checksums, which is what a scanner too slow for a large batch looks
	// like from the client's side.
	timeoutOver int
	// stallOver starts answering a larger request and then stops sending, which
	// is what a slow Xray actually does: the headers arrive at once and the
	// megabytes of issues do not.
	stallOver int
	stallFor  time.Duration
}

func newFakeXray(t *testing.T) *fakeXray {
	t.Helper()
	f := &fakeXray{
		artifacts:    map[string]xrayArtifact{},
		rawArtifacts: map[string]string{},
		authUser:     "svc", authPass: "token",
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/xray/api/v1/system/ping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "pong"})
	})

	mux.HandleFunc(summaryPathV1, func(w http.ResponseWriter, r *http.Request) {
		f.v1Calls.Add(1)
		f.summary(w, r)
	})
	mux.HandleFunc(summaryPathV2, func(w http.ResponseWriter, r *http.Request) {
		f.v2Calls.Add(1)
		if f.v2Status != 0 {
			w.WriteHeader(f.v2Status)
			_, _ = w.Write([]byte(`{"errors":[{"status":404,"message":"not found"}]}`))
			return
		}
		f.summary(w, r)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeXray) summary(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	user, pass, ok := r.BasicAuth()
	if !ok || user != f.authUser || pass != f.authPass {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"status":403,"message":"not entitled for Xray"}]}`))
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}

	var req artifactSummaryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if n := int32(len(req.Checksums)); n > f.maxChecksums.Load() {
		f.maxChecksums.Store(n)
	}
	if f.timeoutOver > 0 && len(req.Checksums) > f.timeoutOver {
		// 504 classifies as unavailable rather than timeout, so the
		// gateway-timeout status is what makes this a deadline the client
		// recognises as worth splitting.
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"errors":[{"status":504,"message":"timeout"}]}`))
		return
	}
	if f.stallOver > 0 && len(req.Checksums) > f.stallOver {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"artifacts":[`))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-time.After(f.stallFor):
		case <-r.Context().Done():
		}
		return
	}

	var artifacts []json.RawMessage
	var errs []xraySummaryErr
	for _, sum := range req.Checksums {
		if raw, ok := f.rawArtifacts[sum]; ok {
			artifacts = append(artifacts, json.RawMessage(raw))
			continue
		}
		if a, ok := f.artifacts[sum]; ok {
			b, _ := json.Marshal(a)
			artifacts = append(artifacts, b)
			continue
		}
		errs = append(errs, xraySummaryErr{
			Identifier: sum, Error: "Artifact doesn't exist or not indexed in Xray",
		})
	}
	_ = json.NewEncoder(w).Encode(struct {
		Artifacts []json.RawMessage `json:"artifacts"`
		Errors    []xraySummaryErr  `json:"errors"`
	}{artifacts, errs})
}

// index registers an artifact with one high issue against openssl.
func (f *fakeXray) index(sha string, issues ...xrayIssue) {
	f.artifacts[sha] = xrayArtifact{
		General: xrayGeneral{SHA256: sha, Name: "image", PkgType: "Docker", ScanTime: "2026-06-29T01:14:00Z"},
		Issues:  issues,
	}
}

func opensslIssue() xrayIssue {
	return xrayIssue{
		IssueID:     "XRAY-123456",
		Summary:     "OpenSSL denial of service",
		Description: "A vulnerability in OpenSSL could allow an attacker to cause a denial of service.",
		IssueType:   "security",
		Severity:    "High",
		Created:     "2024-03-29T00:00:00Z",
		CVEs:        []xrayCVE{{CVE: "CVE-2024-3094", CVSSV3: "9.8/CVSS:3.1/AV:N/AC:L"}},
		Components: []xrayComponent{{
			ComponentID:   "12:openssl",
			Version:       "1.1.1n-0+deb11u3",
			PkgType:       "deb",
			FixedVersions: []string{"1.1.1n-0+deb11u4"},
		}},
	}
}

func testProvider(t *testing.T, f *fakeXray, mutate func(*XraySettings)) security.Provider {
	t.Helper()
	settings := XraySettings{Enabled: true, RequestTimeout: 5 * time.Second, BatchSize: 2, Concurrency: 2}
	if mutate != nil {
		mutate(&settings)
	}
	p, err := NewXrayProvider(XrayConfig{
		Endpoint:       f.URL,
		Username:       "svc",
		Password:       "token",
		RequestTimeout: settings.RequestTimeout,
		BatchSize:      settings.BatchSize,
	}, settings)
	if err != nil {
		t.Fatalf("NewXrayProvider: %v", err)
	}
	return p
}

func ref(name, sha string) security.ArtifactRef {
	return security.ArtifactRef{Name: name, Digest: "sha256:" + sha, Tag: "25.7", Kind: "image"}
}

func TestXrayScanNormalizesFindings(t *testing.T) {
	f := newFakeXray(t)
	f.index("aaa", opensslIssue())

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}

	r := reports[0]
	if r.Status != security.StatusScanned {
		t.Fatalf("status = %q, want scanned", r.Status)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(r.Findings))
	}
	got := r.Findings[0]
	if got.CVE != "CVE-2024-3094" {
		t.Errorf("cve = %q", got.CVE)
	}
	if got.Severity != security.SeverityHigh {
		t.Errorf("severity = %q, want high", got.Severity)
	}
	if !got.Fixable || len(got.FixedIn) != 1 {
		t.Errorf("fixable = %t fixedIn = %v, want fixable with one version", got.Fixable, got.FixedIn)
	}
	if got.Provider != "jfrog-xray" {
		t.Errorf("provider = %q", got.Provider)
	}
	// The version is data, not identity: it must not travel in the key that
	// two releases are compared on.
	if got.Component.ID != "deb://openssl" {
		t.Errorf("component id = %q, want deb://openssl", got.Component.ID)
	}
	if got.Component.Version != "1.1.1n-0+deb11u3" {
		t.Errorf("component version = %q", got.Component.Version)
	}
	if got.CVSSScore != 9.8 {
		t.Errorf("cvss = %v, want 9.8", got.CVSSScore)
	}
	if r.Counts.Total != 1 || r.Counts.Fixable != 1 || r.Counts.BySeverity.High != 1 {
		t.Errorf("counts = %+v", r.Counts)
	}
	if r.ScannedAt == nil {
		t.Error("scannedAt not read from the summary")
	}
}

// One artifact, captured verbatim from a JFrog Xray 3.x summary/artifact
// response with the prose trimmed.
//
// # Why a literal and not a struct
//
// Because the bug it pins was a struct that no longer matched the wire.
// `components` was declared as a map keyed by component id, which a current
// Xray sends as an ARRAY of objects with the version and the fixed versions as
// fields of their own - so it decoded to nothing, every finding was emitted
// with no package and no fix, and the interface showed a dash in the Package
// column of 5,749 rows and 0% in the Fixable card. A fixture built from our own
// types would have round-tripped perfectly and proved nothing.
const realXrayArtifactV2 = `{
  "general": {
    "name": "cbur-agent:1.5.7-alpine-24",
    "component_id": "cbur-agent:1.5.7-alpine-24",
    "pkg_type": "Docker",
    "path": "default/apm0014228-oci-stage/cbur-agent/1.5.7-alpine-24/",
    "sha256": "aaa"
  },
  "issues": [
    {
      "issue_id": "XRAY-1000454",
      "summary": "Issue summary: Parsing a crafted DER-encoded ASN.1 structure ...",
      "description": "An integer truncation in OpenSSL's ASN.1 decoder ...",
      "issue_type": "security",
      "severity": "High",
      "provider": "JFrog",
      "cves": [
        {
          "cve": "CVE-2026-34180",
          "cwe": ["CWE-125"],
          "cvss_v3": "7.5/CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H"
        }
      ],
      "created": "2026-06-10T00:00:00.000Z",
      "impact_path": [
        "default/apm0014228-oci-stage/cbur-agent/1.5.7-alpine-24/sha256__0b37.tar.gz/3.23:libcrypto3:3.5.5-r0",
        "default/apm0014228-oci-stage/cbur-agent/1.5.7-alpine-24/sha256__0b37.tar.gz/3.23:libssl3:3.5.5-r0"
      ],
      "components": [
        {
          "component_id": "3.23:libcrypto3",
          "fixed_versions": ["3.5.7-r0"],
          "pkg_type": "alpine",
          "version": "3.5.5-r0"
        },
        {
          "component_id": "3.23:libssl3",
          "fixed_versions": ["3.5.7-r0"],
          "pkg_type": "alpine",
          "version": "3.5.5-r0"
        }
      ],
      "applicability_details": [
        {
          "component_id": "docker://cbur-agent:1.5.7-alpine-24",
          "source_comp_id": "alpine://3.23:libssl3:3.5.5-r0",
          "vulnerability_id": "CVE-2026-34180",
          "result": "not_applicable"
        }
      ]
    }
  ]
}`

// The payload a real Xray sends, parsed into the fields the interface renders.
func TestXrayParsesARealSummaryPayload(t *testing.T) {
	f := newFakeXray(t)
	f.rawArtifacts["aaa"] = realXrayArtifactV2

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("cbur-agent", "aaa")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r := reports[0]
	if r.Status != security.StatusScanned {
		t.Fatalf("status = %q (%s)", r.Status, r.Message)
	}
	// One issue, two components: two things to fix in two packages.
	if len(r.Findings) != 2 {
		t.Fatalf("got %d findings, want 2 (one per component)", len(r.Findings))
	}

	byName := map[string]security.Finding{}
	for _, fi := range r.Findings {
		byName[fi.Component.Name] = fi
	}
	got, ok := byName["libssl3"]
	if !ok {
		t.Fatalf("no finding for libssl3; components were %v", componentNames(r.Findings))
	}
	// The alpine release in "3.23:libssl3" is a qualifier, not the package.
	if got.Component.ID != "alpine://libssl3" {
		t.Errorf("component id = %q, want alpine://libssl3", got.Component.ID)
	}
	if got.Component.Version != "3.5.5-r0" || got.Component.Type != "alpine" {
		t.Errorf("component = %+v", got.Component)
	}
	if !got.Fixable || len(got.FixedIn) != 1 || got.FixedIn[0] != "3.5.7-r0" {
		t.Errorf("fixable = %t fixedIn = %v, want the 3.5.7-r0 fix", got.Fixable, got.FixedIn)
	}
	// "7.5/CVSS:3.1/AV:N/..." is one string carrying both.
	if got.CVSSScore != 7.5 {
		t.Errorf("cvss score = %v, want 7.5", got.CVSSScore)
	}
	if !strings.HasPrefix(got.CVSSVector, "CVSS:3.1/AV:N") {
		t.Errorf("cvss vector = %q", got.CVSSVector)
	}
	if r.Counts.Fixable != 2 {
		t.Errorf("fixable count = %d, want 2 - the Fixable card reads this", r.Counts.Fixable)
	}
}

// A v1 payload has no `components` at all. The package is still named, from the
// impact path, because a finding that says WHICH package is wrong beats one
// that says only that something is.
func TestXrayNamesThePackageWithoutAComponentsArray(t *testing.T) {
	f := newFakeXray(t)
	f.rawArtifacts["bbb"] = `{
	  "general": {"sha256": "bbb", "name": "img", "pkg_type": "Docker"},
	  "issues": [{
	    "issue_id": "XRAY-1000454",
	    "issue_type": "security",
	    "severity": "High",
	    "summary": "OpenSSL over-read",
	    "cves": [{"cve": "CVE-2026-34180", "cvss_v3": "7.5/CVSS:3.1/AV:N"}],
	    "impact_path": ["default/repo/img/1.0/sha256__0b37.tar.gz/3.23:libssl3:3.5.5-r0"]
	  }]
	}`

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("img", "bbb")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(reports[0].Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(reports[0].Findings))
	}
	got := reports[0].Findings[0]
	if got.Component.Name != "libssl3" || got.Component.Version != "3.5.5-r0" {
		t.Errorf("component = %+v, want libssl3 at 3.5.5-r0 from the impact path", got.Component)
	}
	// No fix data in this payload, and none is invented.
	if got.Fixable {
		t.Error("a payload with no fixed_versions must not report a finding as fixable")
	}
}

// v2 is asked first, because v1 carries neither components nor fix versions.
func TestXrayPrefersTheV2Summary(t *testing.T) {
	f := newFakeXray(t)
	f.index("aaa", opensslIssue())

	p := testProvider(t, f, nil)
	if _, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if f.v2Calls.Load() != 1 || f.v1Calls.Load() != 0 {
		t.Errorf("v2 calls = %d, v1 calls = %d; want the newer endpoint only",
			f.v2Calls.Load(), f.v1Calls.Load())
	}
}

// A platform that predates v2 answers 404 for it, and is served by v1 rather
// than reported as unscannable.
func TestXrayFallsBackToTheV1Summary(t *testing.T) {
	f := newFakeXray(t)
	f.v2Status = http.StatusNotFound
	f.index("aaa", opensslIssue())

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if reports[0].Status != security.StatusScanned {
		t.Fatalf("status = %q (%s), want the v1 answer", reports[0].Status, reports[0].Message)
	}
	if f.v1Calls.Load() == 0 {
		t.Error("v1 was never tried")
	}

	// And it is remembered: a second scan does not pay for the 404 again.
	before := f.v2Calls.Load()
	if _, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if f.v2Calls.Load() != before {
		t.Errorf("asked v2 again after it answered 404 (%d -> %d)", before, f.v2Calls.Load())
	}
}

func componentNames(findings []security.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Component.Name)
	}
	return out
}

// An artifact Xray has never indexed is not a clean artifact. This is the
// distinction the whole package exists to preserve.
func TestXrayNotIndexedIsNotClean(t *testing.T) {
	f := newFakeXray(t)
	f.index("aaa", opensslIssue())

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa"), ref("side", "bbb")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	byName := map[string]security.Report{}
	for _, r := range reports {
		byName[r.Artifact.Name] = r
	}
	if byName["side"].Status != security.StatusNotScanned {
		t.Fatalf("unindexed artifact status = %q, want not_scanned", byName["side"].Status)
	}
	if !strings.Contains(byName["side"].Message, "no scan result") {
		t.Errorf("message = %q, want it to say there is no result", byName["side"].Message)
	}
	if byName["side"].Status.Conclusive() {
		t.Error("an unindexed artifact must not be conclusive")
	}
}

// A refusal from Xray must not read as a clean release, and must say what to do.
func TestXrayFailureBecomesUnavailableReports(t *testing.T) {
	f := newFakeXray(t)
	f.authPass = "something else"

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan returned an error for a per-artifact failure: %v", err)
	}
	if reports[0].Status != security.StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", reports[0].Status)
	}
	if !strings.Contains(reports[0].Message, "Xray") {
		t.Errorf("message = %q, want it to name Xray", reports[0].Message)
	}
}

// Signatures are not artifacts Xray declined to scan; they are artifacts with
// nothing to scan in. They must never reach the wire, and must not count
// against coverage.
func TestXraySkipsUnscannableArtifacts(t *testing.T) {
	f := newFakeXray(t)
	p := testProvider(t, f, nil)

	sig := security.ArtifactRef{Name: "main.sig", Digest: "sha256:ccc", Kind: "signature"}
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{sig}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if reports[0].Status != security.StatusUnsupported {
		t.Errorf("status = %q, want unsupported", reports[0].Status)
	}
	if f.calls.Load() != 0 {
		t.Errorf("made %d Xray calls for a signature, want 0", f.calls.Load())
	}
	if security.StatusUnsupported.Counts() {
		t.Error("unsupported artifacts must not count towards coverage")
	}
}

// Xray indexes container images. Everything else in a release is left alone.
//
// The failure this exists for: a release of 261 artifacts, 150 of them Helm
// charts and files, asked Xray about all of them and got "not indexed" for
// every non-image - which is indistinguishable, in a coverage number, from an
// image somebody forgot to index. The page reported 4% scanned and two hundred
// artifacts of work that did not exist.
func TestXrayAsksOnlyAboutImages(t *testing.T) {
	f := newFakeXray(t)
	f.index("aaa", opensslIssue())

	refs := []security.ArtifactRef{
		ref("main", "aaa"),
		{Name: "cfx-chart", Digest: "sha256:bbb", Kind: "chart"},
		{Name: "values.yaml", Digest: "sha256:ccc", Kind: "file"},
		{Name: "unclassified", Digest: "sha256:ddd", MediaType: "application/vnd.oci.image.manifest.v1+json"},
	}

	p := testProvider(t, f, func(s *XraySettings) { s.BatchSize = 10 })
	reports, err := p.Scan(t.Context(), refs, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	want := map[string]security.Status{
		"main":         security.StatusScanned,
		"cfx-chart":    security.StatusUnsupported,
		"values.yaml":  security.StatusUnsupported,
		"unclassified": security.StatusNotScanned, // an image, asked about, unknown to Xray
	}
	for _, r := range reports {
		if got := want[r.Artifact.Name]; r.Status != got {
			t.Errorf("%s = %q, want %q (%s)", r.Artifact.Name, r.Status, got, r.Message)
		}
	}
	// One request, carrying the two images and neither of the other two.
	if got := f.maxChecksums.Load(); got != 2 {
		t.Errorf("asked Xray about %d artifacts, want 2", got)
	}
	for _, r := range reports {
		if r.Status == security.StatusUnsupported && !strings.Contains(r.Message, "container images") {
			t.Errorf("%s says %q, which does not tell a reader what the scanner covers",
				r.Artifact.Name, r.Message)
		}
	}
}

// A release is 157 images. They go out batched and in parallel, and progress
// says so as it happens.
func TestXrayBatchesAndReportsProgress(t *testing.T) {
	f := newFakeXray(t)
	var refs []security.ArtifactRef
	for i := 0; i < 7; i++ {
		sha := strings.Repeat(string(rune('a'+i)), 3)
		f.index(sha, opensslIssue())
		refs = append(refs, ref("image-"+string(rune('a'+i)), sha))
	}

	prog := &recordingProgress{}
	p := testProvider(t, f, func(s *XraySettings) { s.BatchSize = 2 })
	reports, err := p.Scan(t.Context(), refs, security.ScanOptions{Detail: true, Progress: prog})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(reports) != 7 {
		t.Fatalf("got %d reports, want 7", len(reports))
	}
	for _, r := range reports {
		if r.Status != security.StatusScanned {
			t.Fatalf("%s status = %q", r.Artifact.Name, r.Status)
		}
	}
	if got := f.calls.Load(); got != 4 {
		t.Errorf("made %d requests for 7 artifacts at batch size 2, want 4", got)
	}
	if got := f.maxChecksums.Load(); got > 2 {
		t.Errorf("largest batch was %d checksums, want at most 2", got)
	}
	final := prog.last()
	if final.done != 7 || final.total != 7 {
		t.Errorf("final progress = %d/%d, want 7/7", final.done, final.total)
	}
}

// A counts-only read keeps the arithmetic and drops the rows, because a package
// listing renders a number and must not ship a megabyte to do it.
func TestXrayCountsOnlyDropsFindings(t *testing.T) {
	f := newFakeXray(t)
	f.index("aaa", opensslIssue())

	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: false})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(reports[0].Findings) != 0 {
		t.Errorf("got %d findings for a counts-only read, want 0", len(reports[0].Findings))
	}
	if reports[0].Counts.Total != 1 {
		t.Errorf("counts.total = %d, want 1: the arithmetic survives", reports[0].Counts.Total)
	}
}

// Disabled means no requests and no data. Not an empty result: a distinct state.
func TestXrayDisabledMakesNoRequests(t *testing.T) {
	f := newFakeXray(t)
	p, err := NewXrayProvider(XrayConfig{Endpoint: f.URL, Username: "svc", Password: "token"}, XraySettings{Enabled: false})
	if err != nil {
		t.Fatalf("NewXrayProvider: %v", err)
	}
	if p.Enabled() {
		t.Fatal("provider reported itself enabled with xrayEnabled off")
	}
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if reports[0].Status != security.StatusDisabled {
		t.Errorf("status = %q, want disabled", reports[0].Status)
	}
	if f.calls.Load() != 0 {
		t.Errorf("made %d Xray calls while disabled, want 0", f.calls.Load())
	}
}

func TestXrayPing(t *testing.T) {
	f := newFakeXray(t)
	c, err := NewXrayClient(XrayConfig{Endpoint: f.URL, Username: "svc", Password: "token"})
	if err != nil {
		t.Fatalf("NewXrayClient: %v", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestXrayClientRequiresCredentials(t *testing.T) {
	_, err := NewXrayClient(XrayConfig{Endpoint: "https://acme.jfrog.io"})
	if err == nil {
		t.Fatal("built a client with no credentials")
	}
	if registry.ClassOf(err) != registry.ClassAuth && !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error = %v, want it to name the missing credential", err)
	}
}

func TestResolveEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  XrayConfig
		want string
	}{
		{"explicit wins", XrayConfig{Endpoint: "https://acme.jfrog.io", Registry: "acme-docker.jfrog.io"}, "https://acme.jfrog.io"},
		{"derived from registry", XrayConfig{Registry: "acme.jfrog.io"}, "https://acme.jfrog.io"},
		{"bare host gets a scheme", XrayConfig{Endpoint: "acme.jfrog.io"}, "https://acme.jfrog.io"},
		{"trailing slash trimmed", XrayConfig{Endpoint: "https://acme.jfrog.io/"}, "https://acme.jfrog.io"},
		{"plain http for development", XrayConfig{Registry: "localhost:8081", PlainHTTP: true}, "http://localhost:8081"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEndpoint(tc.cfg)
			if err != nil {
				t.Fatalf("resolveEndpoint: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := resolveEndpoint(XrayConfig{}); err == nil {
		t.Error("resolved an endpoint from nothing")
	}
}

func TestParseComponentID(t *testing.T) {
	for _, tc := range []struct {
		in                    string
		id, name, version, ty string
	}{
		{"deb://openssl:1.1.1n-0+deb11u3", "deb://openssl", "openssl", "1.1.1n-0+deb11u3", "deb"},
		{"npm://lodash:4.17.20", "npm://lodash", "lodash", "4.17.20", "npm"},
		{"gav://org.apache.commons:commons-lang3:3.12.0", "gav://org.apache.commons:commons-lang3", "org.apache.commons:commons-lang3", "3.12.0", "gav"},
		{"openssl", "openssl", "openssl", "", ""},
	} {
		got := parseComponentID(tc.in)
		if got.ID != tc.id || got.Name != tc.name || got.Version != tc.version || got.Type != tc.ty {
			t.Errorf("parseComponentID(%q) = %+v", tc.in, got)
		}
	}
}

// License findings are not vulnerabilities and must not inflate the counts.
func TestNormalizeSkipsLicenseIssues(t *testing.T) {
	a := xrayArtifact{Issues: []xrayIssue{
		opensslIssue(),
		{IssueID: "XRAY-999", IssueType: "license", Severity: "High", Summary: "GPL-3.0"},
	}}
	got := normalizeArtifact(a)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
}

// One Xray issue naming two CVEs across two components is four things to fix.
func TestNormalizeExpandsCVEsAcrossComponents(t *testing.T) {
	issue := opensslIssue()
	issue.CVEs = append(issue.CVEs, xrayCVE{CVE: "CVE-2024-0001"})
	issue.Components = append(issue.Components, xrayComponent{
		ComponentID: "12:zlib", Version: "1.2.11", PkgType: "deb",
	})

	got := normalizeArtifact(xrayArtifact{Issues: []xrayIssue{issue}})
	if len(got) != 4 {
		t.Fatalf("got %d findings, want 4 (2 CVEs x 2 components)", len(got))
	}
	keys := map[string]bool{}
	for _, f := range got {
		keys[f.Key()] = true
	}
	if len(keys) != 4 {
		t.Errorf("findings collapsed to %d distinct keys, want 4", len(keys))
	}
	// zlib has no fixed version: it must not claim to be fixable.
	for _, f := range got {
		if f.Component.Name == "zlib" && f.Fixable {
			t.Error("zlib finding claims to be fixable with no fixed version")
		}
	}
}

// The provider must never invent findings for an artifact whose digest we do
// not have.
func TestXrayRefusesArtifactWithoutDigest(t *testing.T) {
	f := newFakeXray(t)
	p := testProvider(t, f, nil)
	reports, err := p.Scan(t.Context(), []security.ArtifactRef{{Name: "nameless", Kind: "image"}}, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if reports[0].Status != security.StatusUnavailable {
		t.Errorf("status = %q, want unavailable", reports[0].Status)
	}
	if f.calls.Load() != 0 {
		t.Errorf("made %d calls for a digestless artifact", f.calls.Load())
	}
}

func TestXrayScanRespectsCancellation(t *testing.T) {
	f := newFakeXray(t)
	f.index("aaa", opensslIssue())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	p := testProvider(t, f, nil)
	if _, err := p.Scan(ctx, []security.ArtifactRef{ref("main", "aaa")}, security.ScanOptions{Detail: true}); err == nil {
		t.Error("Scan ignored a cancelled context")
	}
}

// recordingProgress captures what the interface would have been told.
type recordingProgress struct {
	mu     chan struct{}
	stages []stage
	notes  []string
}

type stage struct {
	name        string
	done, total int
}

func (p *recordingProgress) Stage(name string, done, total int) {
	p.lock()
	defer p.unlock()
	p.stages = append(p.stages, stage{name, done, total})
}

func (p *recordingProgress) Note(s string) {
	p.lock()
	defer p.unlock()
	p.notes = append(p.notes, s)
}

func (p *recordingProgress) last() stage {
	p.lock()
	defer p.unlock()
	if len(p.stages) == 0 {
		return stage{}
	}
	return p.stages[len(p.stages)-1]
}

func (p *recordingProgress) lock() {
	if p.mu == nil {
		p.mu = make(chan struct{}, 1)
	}
	p.mu <- struct{}{}
}

func (p *recordingProgress) unlock() { <-p.mu }

// A batch that times out is SPLIT, not lost.
//
// The failure this exists for: 258 artifacts asked in batches of fifty, a busy
// Xray timing out on six requests, and 209 artifacts reported unavailable
// because a handful of images were slow to summarise.
func TestXraySplitsATimedOutBatch(t *testing.T) {
	f := newFakeXray(t)

	var refs []security.ArtifactRef
	for i := 0; i < 8; i++ {
		sha := strings.Repeat(string(rune('a'+i)), 3)
		f.index(sha, opensslIssue())
		refs = append(refs, ref("image-"+string(rune('a'+i)), sha))
	}
	// The server refuses any request asking about more than two at once, which
	// is what a scanner too slow for a large batch looks like from here.
	f.timeoutOver = 2

	prog := &recordingProgress{}
	p := testProvider(t, f, func(s *XraySettings) { s.BatchSize = 8 })
	reports, err := p.Scan(t.Context(), refs, security.ScanOptions{Detail: true, Progress: prog})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, r := range reports {
		if r.Status != security.StatusScanned {
			t.Fatalf("%s = %q (%s); splitting should have rescued every artifact",
				r.Artifact.Name, r.Status, r.Message)
		}
	}
	if got := prog.last(); got.done != 8 {
		t.Errorf("final progress = %d, want 8", got.done)
	}
}

// A batch whose ANSWER stops arriving is split too.
//
// The failure this exists for, and it is the common one: Xray returns the
// headers for a fifty-checksum summary immediately and then streams megabytes
// of issues, so the deadline lands inside the JSON decoder rather than on the
// request. That was reported as "answered with something this Coordinator could
// not read", which classifies as malformed, which is not worth retrying - so
// the splitting written to rescue exactly this case never ran, and a release
// lost 159 artifacts to it.
func TestXraySplitsABatchWhoseBodyStalls(t *testing.T) {
	f := newFakeXray(t)

	var refs []security.ArtifactRef
	for i := 0; i < 8; i++ {
		sha := strings.Repeat(string(rune('a'+i)), 3)
		f.index(sha, opensslIssue())
		refs = append(refs, ref("image-"+string(rune('a'+i)), sha))
	}
	f.stallOver = 2
	f.stallFor = 5 * time.Second

	p := testProvider(t, f, func(s *XraySettings) {
		s.BatchSize = 8
		s.RequestTimeout = 300 * time.Millisecond
	})
	reports, err := p.Scan(t.Context(), refs, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, r := range reports {
		if r.Status != security.StatusScanned {
			t.Fatalf("%s = %q (%s); a stalled body should have been split, not written off",
				r.Artifact.Name, r.Status, r.Message)
		}
	}
}

// A refused credential will refuse the halves too. Retrying it per artifact
// turns one clear error into fifty and is a way to get an account locked.
func TestXrayDoesNotSplitAnAuthFailure(t *testing.T) {
	f := newFakeXray(t)
	f.authPass = "something else"

	var refs []security.ArtifactRef
	for i := 0; i < 4; i++ {
		refs = append(refs, ref("image-"+string(rune('a'+i)), strings.Repeat(string(rune('a'+i)), 3)))
	}

	p := testProvider(t, f, func(s *XraySettings) { s.BatchSize = 4 })
	reports, err := p.Scan(t.Context(), refs, security.ScanOptions{Detail: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, r := range reports {
		if r.Status != security.StatusUnavailable {
			t.Fatalf("%s = %q, want unavailable", r.Artifact.Name, r.Status)
		}
		if !strings.Contains(r.Message, "credential") {
			t.Errorf("message = %q, want it to name the credential", r.Message)
		}
	}
	// One request, not four: the failure was not worth splitting.
	if got := f.calls.Load(); got != 1 {
		t.Errorf("made %d requests for an auth failure, want 1", got)
	}
}

// The raw error was three restatements of a URL and a Go phrase. Each class of
// failure has a different fix, and the message has to name it.
func TestXrayFailureMessagesNameTheFix(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"auth", &registry.Error{Err: registry.ErrForbidden}, "Xray read permission"},
		{"timeout", &registry.Error{Err: registry.ErrTimeout}, "requestTimeout"},
		{"rate limited", &registry.Error{Err: registry.ErrRateLimited}, "concurrency"},
		{"not found", &registry.Error{Err: registry.ErrNotFound}, "xrayEndpoint"},
		{"bare deadline", context.DeadlineExceeded, "requestTimeout"},
		// A body we cannot parse means Xray ANSWERED. Describing that as
		// unreachable sends somebody to check a network path that is fine.
		{"malformed", &registry.Error{Err: registry.ErrMalformedResponse}, "could not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeXrayFailure(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("message = %q, want it to mention %q", got, tc.want)
			}
			if strings.Contains(got, "context deadline exceeded") {
				t.Errorf("message leaks the Go phrase: %q", got)
			}
			if tc.name == "malformed" && strings.Contains(got, "could not be reached") {
				t.Errorf("a parse failure is not unreachability: %q", got)
			}
		})
	}
}
