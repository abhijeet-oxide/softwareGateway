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
	artifacts map[string]xrayArtifact // by bare sha256 hex
	// status, when non-zero, is returned instead of a summary.
	status int
	body   string
	// authUser and authPass are what the server demands.
	authUser, authPass string
	// maxChecksums records the largest batch the client asked for.
	maxChecksums atomic.Int32
}

func newFakeXray(t *testing.T) *fakeXray {
	t.Helper()
	f := &fakeXray{artifacts: map[string]xrayArtifact{}, authUser: "svc", authPass: "token"}
	mux := http.NewServeMux()

	mux.HandleFunc("/xray/api/v1/system/ping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "pong"})
	})

	mux.HandleFunc("/xray/api/v1/summary/artifact", func(w http.ResponseWriter, r *http.Request) {
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

		var resp artifactSummaryResponse
		for _, sum := range req.Checksums {
			if a, ok := f.artifacts[sum]; ok {
				resp.Artifacts = append(resp.Artifacts, a)
				continue
			}
			resp.Errors = append(resp.Errors, xraySummaryErr{
				Identifier: sum, Error: "Artifact doesn't exist or not indexed in Xray",
			})
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
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
		CVEs: []struct {
			CVE          string `json:"cve"`
			CVSSV2Score  any    `json:"cvss_v2_score"`
			CVSSV3Score  any    `json:"cvss_v3_score"`
			CVSSV2Vector string `json:"cvss_v2_vector"`
			CVSSV3Vector string `json:"cvss_v3_vector"`
		}{{CVE: "CVE-2024-3094", CVSSV3Score: 9.8, CVSSV3Vector: "AV:N/AC:L"}},
		Components: map[string]struct {
			FixedVersions []string   `json:"fixed_versions"`
			ImpactPaths   [][]string `json:"impact_paths"`
		}{
			"deb://openssl:1.1.1n-0+deb11u3": {FixedVersions: []string{"1.1.1n-0+deb11u4"}},
		},
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
	issue.CVEs = append(issue.CVEs, struct {
		CVE          string `json:"cve"`
		CVSSV2Score  any    `json:"cvss_v2_score"`
		CVSSV3Score  any    `json:"cvss_v3_score"`
		CVSSV2Vector string `json:"cvss_v2_vector"`
		CVSSV3Vector string `json:"cvss_v3_vector"`
	}{CVE: "CVE-2024-0001"})
	issue.Components["deb://zlib:1.2.11"] = struct {
		FixedVersions []string   `json:"fixed_versions"`
		ImpactPaths   [][]string `json:"impact_paths"`
	}{}

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
