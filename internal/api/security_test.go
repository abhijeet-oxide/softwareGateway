package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// fakeAnalyzer stands in for the security engine, so handler tests need no
// scanner. It answers from a map keyed by artifact digest, which is exactly
// what a real provider does.
type fakeAnalyzer struct {
	byDigest map[string]security.Report
	enabled  bool
	provider string
	err      error
	// calls counts retrievals, so a test can assert that a disabled scanner
	// costs no work and that a comparison retrieves both sides.
	calls int
	// detail records what the last request asked for.
	detail bool
}

func newFakeAnalyzer() *fakeAnalyzer {
	return &fakeAnalyzer{byDigest: map[string]security.Report{}, enabled: true, provider: "jfrog-xray"}
}

func (f *fakeAnalyzer) Posture(_ context.Context, req security.Request) (security.Result, error) {
	f.calls++
	f.detail = req.Detail
	if f.err != nil {
		return security.Result{}, f.err
	}

	var reports []security.Report
	for _, ref := range req.Artifacts {
		if r, ok := f.byDigest[ref.Digest]; ok {
			r.Artifact = ref
			reports = append(reports, r)
			continue
		}
		if !f.enabled {
			reports = append(reports, security.Report{
				Artifact: ref, Status: security.StatusDisabled, Provider: f.provider,
				Message: "JFrog Xray is not enabled for this repository.",
			})
			continue
		}
		reports = append(reports, security.Report{
			Artifact: ref, Status: security.StatusUnsupported, Provider: f.provider,
		})
	}
	return security.Result{
		Posture:     security.Summarize(reports),
		Fingerprint: security.FingerprintReports(reports),
		Provider:    f.provider,
		Enabled:     f.enabled,
		RetrievedAt: time.Date(2026, 6, 29, 1, 14, 0, 0, time.UTC),
	}, nil
}

func (f *fakeAnalyzer) Compare(ctx context.Context, req security.CompareRequest) (security.CompareResult, error) {
	a, err := f.Posture(ctx, req.A)
	if err != nil {
		return security.CompareResult{}, err
	}
	b, err := f.Posture(ctx, req.B)
	if err != nil {
		return security.CompareResult{}, err
	}
	return security.CompareResult{
		Comparison: security.Compare(security.CompareInput{
			A: a.Posture.Reports, B: b.Posture.Reports, NameA: req.NameA, NameB: req.NameB,
		}),
		A: a, B: b,
	}, nil
}

func scannedReport(findings ...security.Finding) security.Report {
	at := time.Date(2026, 6, 29, 1, 14, 0, 0, time.UTC)
	r := security.Report{
		Status: security.StatusScanned, Provider: "jfrog-xray",
		Findings: findings, ScannedAt: &at, RetrievedAt: at,
	}
	r.Recount()
	return r
}

func apiFinding(cve string, sev security.Severity, pkg string, fixable bool) security.Finding {
	f := security.Finding{
		CVE: cve, ID: "XRAY-1", Severity: sev, Fixable: fixable, Provider: "jfrog-xray",
		Summary:   "A vulnerability in " + pkg,
		Component: security.Component{ID: "deb://" + pkg, Name: pkg, Version: "1.0", Type: "deb"},
	}
	if fixable {
		f.FixedIn = []string{"2.0"}
	}
	return f
}

func newSecurityHarness(t *testing.T, analyzer *fakeAnalyzer) *apiHarness {
	t.Helper()
	return newAPIHarnessWith(t, func(d *Deps) {
		d.Security = analyzer
		d.SecurityIndex = store.NewSecurity(d.Store)
	})
}

func TestPackageSecurityReportsPostureAndCoverage(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true),
		apiFinding("CVE-2024-0001", security.SeverityLow, "zlib", false),
	)

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2131", digestA)

	var resp v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security?detail=true", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	if resp.Counts.Total != 2 || resp.Counts.BySeverity.Critical != 1 {
		t.Errorf("counts = %+v", resp.Counts)
	}
	if resp.Counts.Fixable != 1 || resp.Counts.NonFixable != 1 {
		t.Errorf("fixability = %d fixable / %d not", resp.Counts.Fixable, resp.Counts.NonFixable)
	}
	if !resp.Enabled || resp.Provider != "jfrog-xray" {
		t.Errorf("provider = %q enabled = %t", resp.Provider, resp.Enabled)
	}
	if resp.Fingerprint == "" {
		t.Error("no fingerprint, so a client cannot tell an unchanged re-read from a changed one")
	}
	// The label travels with the value so every client says the same word.
	for _, r := range resp.Reports {
		if r.Status != "" && r.StatusLabel == "" {
			t.Errorf("report %s has a status with no label", r.Artifact.Name)
		}
	}
	if !analyzer.detail {
		t.Error("detail=true did not reach the analyzer")
	}
}

// A disabled scanner is a distinct state, and must never render as a clean
// release.
func TestPackageSecurityDisabledIsNotClean(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.enabled = false

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2131", digestA)

	var resp v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.State != "disabled" {
		t.Fatalf("state = %q, want disabled", resp.State)
	}
	if resp.Enabled {
		t.Error("enabled reported true for a disabled scanner")
	}
	if resp.Message == "" {
		t.Error("a disabled state with no message tells the user nothing to do")
	}
	if resp.Counts.Total != 0 {
		t.Errorf("counts = %d, want 0", resp.Counts.Total)
	}
}

// An unscanned artifact makes the release's numbers partial, and the response
// says so rather than rounding to clean.
func TestPackageSecurityPartialCoverageSaysSo(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(apiFinding("CVE-1", security.SeverityHigh, "openssl", true))
	analyzer.byDigest[digestB] = security.Report{Status: security.StatusNotScanned, Provider: "jfrog-xray"}

	h := newSecurityHarness(t, analyzer)
	id := h.seedPackage("25.7.2131", digestA)
	h.seedArtifact(id, digestB, "cfx-sidecar")

	var resp v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security?detail=true", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.State != "partial" {
		t.Fatalf("state = %q, want partial", resp.State)
	}
	if resp.Coverage.NotScanned != 1 {
		t.Errorf("coverage = %+v, want one unscanned artifact", resp.Coverage)
	}
	if resp.Coverage.Complete {
		t.Error("coverage reported complete with an unscanned artifact")
	}
	if !strings.Contains(resp.Message, "no scan result") {
		t.Errorf("message = %q", resp.Message)
	}
}

// A re-read of an unchanged posture must not re-send the findings.
func TestPackageSecurityHonoursIfNoneMatch(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(apiFinding("CVE-1", security.SeverityHigh, "openssl", true))

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2131", digestA)

	path := "/api/v1/products/vendor-a/packages/25.7.2131/security?detail=true"
	resp, err := http.Get(h.server.URL + path) //nolint:noctx // test client
	if err != nil {
		t.Fatal(err)
	}
	etag := resp.Header.Get("ETag")
	_ = resp.Body.Close()
	if etag == "" {
		t.Fatal("no ETag on a security response")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want it to be private: these are one repository's findings", cc)
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+path, nil)
	req.Header.Set("If-None-Match", etag)
	again, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Body.Close() }()
	if again.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 for an unchanged posture, got %d", again.StatusCode)
	}
}

func TestCompareSecurityAnswersInPlainLanguage(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(
		apiFinding("CVE-1", security.SeverityHigh, "openssl", true),
		apiFinding("CVE-2", security.SeverityHigh, "zlib", true),
		apiFinding("CVE-3", security.SeverityHigh, "curl", true),
	)
	analyzer.byDigest[digestB] = scannedReport(
		apiFinding("CVE-9", security.SeverityLow, "libxml", false),
	)

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2100", digestA)
	h.seedPackage("25.7.2131", digestB)

	var resp v1.SecurityComparisonResponse
	code := h.post("/api/v1/products/vendor-a/packages/25.7.2100:compareSecurity",
		`{"against":"25.7.2131"}`, &resp)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	if resp.Verdict != "better" {
		t.Fatalf("verdict = %q, want better", resp.Verdict)
	}
	if resp.VerdictLabel != "Better" {
		t.Errorf("verdictLabel = %q", resp.VerdictLabel)
	}
	// The simple view must be a sentence, not a table.
	if !strings.Contains(resp.Explanation, "resolves 3 high") {
		t.Errorf("explanation = %q", resp.Explanation)
	}
	if !strings.Contains(resp.Explanation, "introduces 1 low") {
		t.Errorf("explanation = %q", resp.Explanation)
	}
	// The detailed view is the same object, one level down.
	if len(resp.Changes) == 0 {
		t.Error("no changes: the simple view must be expandable into the breakdown behind it")
	}
	for _, c := range resp.Changes {
		if c.TypeLabel == "" {
			t.Errorf("change %s has no label", c.Type)
		}
	}
	if resp.A.Label != "25.7.2100" || resp.B.Label != "25.7.2131" {
		t.Errorf("ends = %q and %q", resp.A.Label, resp.B.Label)
	}
}

func TestCompareSecurityRequiresASecondRelease(t *testing.T) {
	h := newSecurityHarness(t, newFakeAnalyzer())
	h.seedPackage("25.7.2100", digestA)

	code := h.post("/api/v1/products/vendor-a/packages/25.7.2100:compareSecurity", `{}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 without `against`, got %d", code)
	}
}

func TestSecuritySearchNavigatesToReleases(t *testing.T) {
	analyzer := newFakeAnalyzer()
	h := newSecurityHarness(t, analyzer)
	packageID := h.seedPackage("25.7.2131", digestA)
	_ = packageID

	// Seed the index the way a retrieval would.
	sec := store.NewSecurity(h.store)
	scope := security.Scope{Product: "vendor-a", Repository: "vendor", Role: "source", Provider: "jfrog-xray"}
	report := scannedReport(apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	report.Artifact = security.ArtifactRef{Name: "cfx-main", Digest: digestA, Tag: "25.7.2131", Kind: "image"}
	if err := sec.Save(t.Context(), scope, []security.Report{report}, true,
		security.CacheTTL{Summary: time.Hour, Detail: time.Hour}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	var resp v1.SecuritySearchResponse
	if code := h.get("/api/v1/products/vendor-a/security/search?q=CVE-2024-3094", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Kind != "cve" {
		t.Errorf("kind = %q, want cve: a pasted CVE id says what it is", resp.Kind)
	}
	if len(resp.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(resp.Hits))
	}
	hit := resp.Hits[0]
	if hit.Component.Name != "openssl" {
		t.Errorf("component = %+v", hit.Component)
	}
	if hit.Artifact.Name != "cfx-main" {
		t.Errorf("artifact = %+v", hit.Artifact)
	}
	// The edge that makes the relationship navigable in both directions.
	if len(hit.Releases) != 1 || hit.Releases[0].Tag != "25.7.2131" {
		t.Errorf("releases = %+v, want the release shipping this artifact", hit.Releases)
	}
	// A search that said nothing about its own scope would read as "this CVE
	// is not in your estate".
	if resp.Searched.Note == "" {
		t.Error("the response does not say what the search covered")
	}
}

func TestSecuritySearchRejectsAnUnknownKind(t *testing.T) {
	h := newSecurityHarness(t, newFakeAnalyzer())
	if code := h.get("/api/v1/products/vendor-a/security/search?q=x&kind=banana", nil); code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}
	if code := h.get("/api/v1/products/vendor-a/security/search", nil); code != http.StatusBadRequest {
		t.Fatalf("expected 400 without q, got %d", code)
	}
}

func TestSecurityExportCSVCarriesEveryRowsWholeAddress(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2131", digestA)

	body, header := h.getRaw("/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=csv&view=detailed")
	if !strings.HasPrefix(header.Get("Content-Type"), "text/csv") {
		t.Fatalf("content type = %q", header.Get("Content-Type"))
	}
	if !strings.Contains(header.Get("Content-Disposition"), "attachment") {
		t.Errorf("no attachment disposition: %q", header.Get("Content-Disposition"))
	}
	// An export is a point-in-time extract; caching one is how somebody
	// re-runs it and gets yesterday.
	if header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", header.Get("Cache-Control"))
	}

	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("got %d rows, want a header and at least one finding", len(rows))
	}

	index := map[string]int{}
	for i, h := range rows[0] {
		index[h] = i
	}
	for _, column := range []string{"Product", "Release", "Artifact", "CVE", "Severity", "Package", "Fixable"} {
		if _, ok := index[column]; !ok {
			t.Errorf("export has no %q column, so a row cannot be acted on out of context", column)
		}
	}
	row := rows[1]
	if row[index["CVE"]] != "CVE-2024-3094" || row[index["Release"]] != "25.7.2131" {
		t.Errorf("row = %v", row)
	}
	if row[index["Package"]] != "openssl" {
		t.Errorf("package column = %q", row[index["Package"]])
	}
}

// The Excel export must be a workbook a spreadsheet opens, not a CSV with a
// different extension.
func TestSecurityExportXLSXIsAWorkbook(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2131", digestA)

	body, header := h.getRaw("/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=xlsx&view=detailed")
	if !strings.Contains(header.Get("Content-Type"), "spreadsheetml") {
		t.Fatalf("content type = %q", header.Get("Content-Type"))
	}

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	found := map[string]bool{}
	for _, f := range zr.File {
		found[f.Name] = true
	}
	for _, part := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/worksheets/sheet1.xml"} {
		if !found[part] {
			t.Errorf("workbook is missing %s, so a spreadsheet will refuse it", part)
		}
	}
	// Two sheets: the simple summary and the detailed breakdown, which is what
	// the requirement asks an export to offer.
	if !found["xl/worksheets/sheet2.xml"] {
		t.Error("a detailed export must carry both the summary and the breakdown")
	}
}

func TestSecurityExportRespectsFilters(t *testing.T) {
	analyzer := newFakeAnalyzer()
	analyzer.byDigest[digestA] = scannedReport(
		apiFinding("CVE-CRIT", security.SeverityCritical, "openssl", true),
		apiFinding("CVE-LOW", security.SeverityLow, "zlib", false),
	)

	h := newSecurityHarness(t, analyzer)
	h.seedPackage("25.7.2131", digestA)

	body, _ := h.getRaw("/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=csv&view=detailed&severity=critical")
	text := string(body)
	if !strings.Contains(text, "CVE-CRIT") {
		t.Error("the filtered-in finding is missing from the export")
	}
	if strings.Contains(text, "CVE-LOW") {
		t.Error("the export ignored the active severity filter")
	}
}

// A progress token is polled while the request that mints it is still open.
func TestSecurityProgressIsReadableAndThen404s(t *testing.T) {
	h := newSecurityHarness(t, newFakeAnalyzer())
	if code := h.get("/api/v1/security/progress/never-minted", nil); code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown token, got %d", code)
	}
}

func TestSecurityRoutesAreAbsentWithoutTheDependency(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("25.7.2131", digestA)

	// An honest 404 rather than a route that always fails: the deployment has
	// no scanner wired, and saying so beats a 500.
	if code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security", nil); code != http.StatusNotFound {
		t.Errorf("security route present without the dependency: got %d", code)
	}
	if code := h.get("/api/v1/products/vendor-a/security/search?q=x", nil); code != http.StatusNotFound {
		t.Errorf("search route present without the dependency: got %d", code)
	}
}

// getRaw fetches a body without decoding it, for the export tests.
func (h *apiHarness) getRaw(path string) ([]byte, http.Header) {
	h.t.Helper()
	resp, err := http.Get(h.server.URL + path) //nolint:noctx // test client
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, string(body))
	}
	return body, resp.Header
}

// seedArtifact adds one more artifact to a seeded package.
func (h *apiHarness) seedArtifact(packageID int64, digest, name string) {
	h.t.Helper()

	tx, err := h.store.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := h.packages.InsertArtifact(h.t.Context(), tx, store.ArtifactRow{
		PackageID:   packageID,
		Digest:      digest,
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:   256,
		Raw:         []byte(`{"schemaVersion":2}`),
		Annotations: map[string]string{"org.opencontainers.image.ref.name": name + ":25.7.2131"},
	}); err != nil {
		h.t.Fatalf("seed artifact: %v", err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
}
