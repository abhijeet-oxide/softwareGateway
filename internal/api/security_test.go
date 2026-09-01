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

// The product the harness loads declares a JFrog target with Xray on, because
// that is the shape of a real estate: the vendor publishes, and the scanner
// runs where the release LANDS.
const securityProductDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: vendor-a/platform
      anonymous: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      type: jfrog
      credentialsRef:
        secretName: jfrog
      default: true
      xrayEnabled: true
`

// fakeSyncer stands in for the background job, so handler tests need no
// scanner. It records what it was asked to sync and writes the result the real
// syncer would.
type fakeSyncer struct {
	packages *store.PackageSecurity
	// reports is what a sync "finds", keyed by artifact digest.
	reports map[string]security.Report
	// started counts claims, and lastArtifacts what the last one was given.
	started       int
	lastArtifacts []security.ArtifactRef
	lastScope     security.Scope
	// leaveRunning holds the row in `syncing` instead of completing it.
	leaveRunning bool
	// cancelled counts Stop requests.
	cancelled int
	// storeHandle lets the fake write through the real store, so the read
	// paths under test are the production ones.
	storeHandle store.Store
	// documents is what the sync "captured" from the scanner, keyed by artifact
	// digest then kind. Written through the real store for the same reason the
	// reports are.
	documents map[string]map[security.DocumentKind][]byte
}

func (f *fakeSyncer) Start(ctx context.Context, req security.SyncRequest) (security.SyncStatus, error) {
	if err := f.packages.Claim(ctx, req.PackageID, "test", time.Minute); err != nil {
		return security.SyncAlreadyRunning, err
	}
	f.started++
	f.lastArtifacts = req.Artifacts
	f.lastScope = req.Scope
	if f.leaveRunning {
		return security.SyncStarted, nil
	}

	var reports []security.Report
	for _, ref := range req.Artifacts {
		if r, ok := f.reports[ref.Digest]; ok {
			r.Artifact = ref
			reports = append(reports, r)
			continue
		}
		reports = append(reports, security.Report{
			Artifact: ref, Status: security.StatusNotScanned, Provider: "jfrog-xray",
			Message: "not indexed", RetrievedAt: time.Now().UTC(),
		})
	}

	// The real syncer writes both tiers in one pass; so does this, through the
	// same store, so the read paths under test are the production ones.
	sec := store.NewSecurity(f.storeHandle)
	if err := sec.Save(ctx, req.Scope, reports, true,
		security.CacheTTL{Summary: time.Hour, Detail: time.Hour}); err != nil {
		return security.SyncStarted, err
	}
	var docs []security.Document
	for _, ref := range req.Artifacts {
		for kind, body := range f.documents[ref.Digest] {
			docs = append(docs, security.Document{
				Artifact: ref, Kind: kind, Provider: "jfrog-xray",
				ContentType: "application/json", Payload: body,
				Available: true, SourceBytes: len(body), FetchedAt: time.Now().UTC(),
			})
		}
	}
	if len(docs) > 0 {
		if err := sec.SaveDocuments(ctx, req.Scope, docs, time.Hour); err != nil {
			return security.SyncStarted, err
		}
	}

	posture := security.Summarize(reports)
	return security.SyncStarted, f.packages.Record(ctx, security.PackageResult{
		PackageID:   req.PackageID,
		Provider:    "jfrog-xray",
		Repository:  req.Scope.Repository,
		Role:        req.Scope.Role,
		Posture:     posture,
		Fingerprint: security.FingerprintReports(reports),
	})
}

func (f *fakeSyncer) Progress(int64) (*security.SyncProgress, bool) { return nil, false }
func (f *fakeSyncer) Running(int64) bool                            { return false }

func (f *fakeSyncer) Cancel(ctx context.Context, packageID int64) (bool, error) {
	f.cancelled++
	return f.packages.Stop(ctx, packageID)
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

type securityHarness struct {
	*apiHarness
	syncer *fakeSyncer
}

func newSecurityHarness(t *testing.T) *securityHarness {
	t.Helper()

	syncer := &fakeSyncer{
		reports:   map[string]security.Report{},
		documents: map[string]map[security.DocumentKind][]byte{},
	}
	h := newAPIHarnessWith(t, func(d *Deps) {
		pkgSec := store.NewPackageSecurity(d.Store)
		syncer.packages = pkgSec
		syncer.storeHandle = d.Store

		d.SecuritySync = syncer
		d.SecurityStore = harnessSecurityStore{pkgSec, store.NewSecurity(d.Store)}
		d.SecurityIndex = store.NewSecurity(d.Store)
	}, securityProductDoc)

	return &securityHarness{apiHarness: h, syncer: syncer}
}

// harnessSecurityStore joins the two halves the API asks for, exactly as the
// composition root does.
type harnessSecurityStore struct {
	*store.PackageSecurity
	reports *store.Security
}

func (s harnessSecurityStore) ReportsFor(
	ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
) ([]security.Report, error) {
	return s.reports.ReportsFor(ctx, scope, refs)
}

func (s harnessSecurityStore) LoadDocuments(
	ctx context.Context, scope security.Scope,
	refs []security.ArtifactRef, kinds []security.DocumentKind,
) (map[string]map[security.DocumentKind]security.Document, error) {
	return s.reports.LoadDocuments(ctx, scope, refs, kinds)
}

func (s harnessSecurityStore) DocumentSummaries(
	ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
) (map[string][]security.DocumentSummary, error) {
	return s.reports.DocumentSummaries(ctx, scope, refs)
}

// A release nobody has synced is not a clean release. It is the state the whole
// feature exists to keep distinct, and the response has to offer the sync.
func TestPackageSecurityNeverSynced(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)

	var resp v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.State != "not_synced" {
		t.Fatalf("state = %q, want not_synced", resp.State)
	}
	if resp.Sync.State != "" {
		t.Errorf("sync.state = %q, want the empty never-synced state", resp.Sync.State)
	}
	if !resp.Sync.CanSync {
		t.Errorf("canSync = false with a JFrog target and xray on: %q", resp.Sync.Reason)
	}
	if resp.Counts.Total != 0 {
		t.Errorf("counts = %d, want 0", resp.Counts.Total)
	}
	// The scanner answers for the TARGET, not the vendor source.
	if resp.Repository != "internal" {
		t.Errorf("repository = %q, want the JFrog target", resp.Repository)
	}
}

// The scanner runs where the release LANDS. Scoping to the vendor source finds
// no scanner at all, on an estate where Xray is switched on and working.
func TestSyncAsksTheTargetNotTheSource(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)

	var resp v1.SyncSecurityResponse
	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, &resp); code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", code)
	}
	if !resp.Started || resp.Status != "started" {
		t.Fatalf("response = %+v, want a started sync", resp)
	}
	if h.syncer.lastScope.Repository != "internal" {
		t.Errorf("scope repository = %q, want the JFrog target", h.syncer.lastScope.Repository)
	}
	if h.syncer.lastScope.Role != "target" {
		t.Errorf("scope role = %q, want target", h.syncer.lastScope.Role)
	}
	if len(h.syncer.lastArtifacts) == 0 {
		t.Error("the sync was given no artifacts")
	}
}

func TestSyncThenReadStoredCounts(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true),
		apiFinding("CVE-2024-0001", security.SeverityLow, "zlib", false),
	)

	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil); code != http.StatusAccepted {
		t.Fatalf("sync: expected 202, got %d", code)
	}

	var resp v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security?detail=true", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Sync.State != "synced" {
		t.Fatalf("sync.state = %q, want synced", resp.Sync.State)
	}
	if resp.Counts.Total != 2 || resp.Counts.BySeverity.Critical != 1 {
		t.Errorf("counts = %+v", resp.Counts)
	}
	if resp.Counts.Fixable != 1 || resp.Counts.NonFixable != 1 {
		t.Errorf("fixability = %d fixable / %d not", resp.Counts.Fixable, resp.Counts.NonFixable)
	}
	if resp.Sync.SyncedAt == "" {
		t.Error("no syncedAt, so nobody can tell how stale this is")
	}

	// The detail read serves findings out of the index - no scanner involved.
	var findings int
	for _, rep := range resp.Reports {
		findings += len(rep.Findings)
	}
	if findings != 2 {
		t.Errorf("got %d findings from storage, want 2", findings)
	}
}

// Pressing the button twice must not start two syncs.
func TestSyncIsClaimedOnce(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.leaveRunning = true

	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil); code != http.StatusAccepted {
		t.Fatalf("first sync: got %d", code)
	}

	var second v1.SyncSecurityResponse
	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, &second); code != http.StatusAccepted {
		t.Fatalf("second sync: got %d", code)
	}
	if second.Started {
		t.Error("the second call started a second sync")
	}
	if second.Status != "already_running" {
		t.Errorf("status = %q, want already_running", second.Status)
	}
	if h.syncer.started != 1 {
		t.Errorf("started %d syncs, want 1", h.syncer.started)
	}

	// And the release reports itself as syncing, which is what the listing and
	// the detail page render instead of a stale count.
	var resp v1.PackageSecurityResponse
	h.get("/api/v1/products/vendor-a/packages/25.7.2131/security", &resp)
	if resp.State != "syncing" || resp.Sync.State != "syncing" {
		t.Errorf("state = %q / %q, want syncing", resp.State, resp.Sync.State)
	}
}

// A sync somebody started can be stopped, and the release goes back to where it
// was rather than being marked failed: nothing failed, the run did not happen.
func TestSyncCanBeStopped(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.leaveRunning = true

	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil); code != http.StatusAccepted {
		t.Fatal("the sync did not start")
	}

	var stop v1.CancelSecuritySyncResponse
	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:cancelSecuritySync", `{}`, &stop); code != http.StatusOK {
		t.Fatalf("stopping the sync: got %d", code)
	}
	if !stop.Stopped {
		t.Error("Stopped is false for a sync that was running")
	}
	if stop.Sync.State == "syncing" {
		t.Error("the release still reports itself as syncing after being stopped")
	}

	// And it can be synced again at once - the claim is gone, not waiting out a
	// stale window.
	var again v1.SyncSecurityResponse
	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, &again); code != http.StatusAccepted {
		t.Fatalf("re-syncing after a stop: got %d", code)
	}
	if !again.Started {
		t.Error("a stopped release refused a new sync")
	}

	// Stopping again is not a failure: there is simply nothing to stop.
	var idle v1.CancelSecuritySyncResponse
	h.syncer.leaveRunning = false
	if code := h.post("/api/v1/products/vendor-a/packages/25.7.2131:cancelSecuritySync", `{}`, &idle); code != http.StatusOK {
		t.Fatalf("stopping an idle release: got %d", code)
	}
}

// A product with no scanner anywhere says so, and says which knob turns one on.
func TestSyncRefusedWithoutAScanner(t *testing.T) {
	h := newSecurityHarness(t)
	// The default harness product has no xray block at all.
	plain := newAPIHarnessWith(t, func(d *Deps) {
		pkgSec := store.NewPackageSecurity(d.Store)
		d.SecuritySync = &fakeSyncer{packages: pkgSec, reports: map[string]security.Report{}, storeHandle: d.Store}
		d.SecurityStore = harnessSecurityStore{pkgSec, store.NewSecurity(d.Store)}
	})
	plain.seedPackage("25.7.2131", digestA)
	_ = h

	if code := plain.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil); code != http.StatusConflict {
		t.Fatalf("expected a failed precondition (409), got %d", code)
	}

	var resp v1.PackageSecurityResponse
	plain.get("/api/v1/products/vendor-a/packages/25.7.2131/security", &resp)
	if resp.Sync.CanSync {
		t.Error("canSync is true with no scanner configured anywhere")
	}
	// The key as the product document actually spells it. `xray.enabled` was
	// the nested form this configuration had before it was flattened, and a
	// message naming a setting that no longer exists is worse than one naming
	// none.
	if !strings.Contains(resp.Sync.Reason, "xrayEnabled") {
		t.Errorf("reason = %q, want it to name the setting", resp.Sync.Reason)
	}
}

func TestCompareSecurityFromStoredData(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2100", digestA)
	h.seedPackage("25.7.2131", digestB)

	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-1", security.SeverityHigh, "openssl", true),
		apiFinding("CVE-2", security.SeverityHigh, "zlib", true),
		apiFinding("CVE-3", security.SeverityHigh, "curl", true),
	)
	h.syncer.reports[digestB] = scannedReport(
		apiFinding("CVE-9", security.SeverityLow, "libxml", false),
	)
	h.post("/api/v1/products/vendor-a/packages/25.7.2100:syncSecurity", `{}`, nil)
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	var resp v1.SecurityComparisonResponse
	code := h.post("/api/v1/products/vendor-a/packages/25.7.2100:compareSecurity",
		`{"against":"25.7.2131"}`, &resp)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Verdict != "better" {
		t.Fatalf("verdict = %q (score %d), want better", resp.Verdict, resp.NetScore)
	}
	if !strings.Contains(resp.Explanation, "resolves 3 high") {
		t.Errorf("explanation = %q", resp.Explanation)
	}
	if len(resp.Changes) == 0 {
		t.Error("no changes: the simple view must expand into the breakdown behind it")
	}
	// Both ends carry their sync state, so the interface can offer the sync
	// rather than only reporting a verdict.
	if resp.A.Sync.State != "synced" || resp.B.Sync.State != "synced" {
		t.Errorf("ends = %q / %q, want both synced", resp.A.Sync.State, resp.B.Sync.State)
	}
}

// Comparing against a release nobody synced is inconclusive, and offers the fix.
func TestCompareSecurityAgainstAnUnsyncedRelease(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2100", digestA)
	h.seedPackage("25.7.2131", digestB)
	h.syncer.reports[digestA] = scannedReport(apiFinding("CVE-1", security.SeverityHigh, "openssl", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2100:syncSecurity", `{}`, nil)

	var resp v1.SecurityComparisonResponse
	h.post("/api/v1/products/vendor-a/packages/25.7.2100:compareSecurity", `{"against":"25.7.2131"}`, &resp)

	if resp.Verdict != "inconclusive" {
		t.Fatalf("verdict = %q, want inconclusive", resp.Verdict)
	}
	if resp.B.Sync.State != "" {
		t.Errorf("b.sync.state = %q, want the never-synced state", resp.B.Sync.State)
	}
	if !resp.B.Sync.CanSync {
		t.Error("the unsynced side does not offer a sync")
	}
}

func TestSecuritySearchNavigatesToReleases(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	var resp v1.SecuritySearchResponse
	if code := h.get("/api/v1/products/vendor-a/security/search?q=CVE-2024-3094", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Kind != "cve" {
		t.Errorf("kind = %q, want cve: a pasted CVE id says what it is", resp.Kind)
	}
	if len(resp.Hits) == 0 {
		t.Fatal("a synced release is not in the search index")
	}
	hit := resp.Hits[0]
	if hit.Component.Name != "openssl" {
		t.Errorf("component = %+v", hit.Component)
	}
	if len(hit.Releases) != 1 || hit.Releases[0].Tag != "25.7.2131" {
		t.Errorf("releases = %+v, want the release shipping this artifact", hit.Releases)
	}
	if resp.Searched.Note == "" {
		t.Error("the response does not say what the search covered")
	}
}

// Search only knows what a sync recorded, and must not pretend otherwise.
func TestSecuritySearchFindsNothingBeforeASync(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)

	var resp v1.SecuritySearchResponse
	h.get("/api/v1/products/vendor-a/security/search?q=CVE-2024-3094", &resp)
	if len(resp.Hits) != 0 {
		t.Errorf("got %d hits before any sync", len(resp.Hits))
	}
	if !strings.Contains(resp.Searched.Note, "sync") {
		t.Errorf("note = %q, want it to explain that a sync is what fills the index", resp.Searched.Note)
	}
}

func TestSecurityExportCSVCarriesEveryRowsWholeAddress(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	body, header := h.getRaw("/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=csv&view=detailed")
	if !strings.HasPrefix(header.Get("Content-Type"), "text/csv") {
		t.Fatalf("content type = %q", header.Get("Content-Type"))
	}
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
	for i, name := range rows[0] {
		index[name] = i
	}
	for _, column := range []string{"Product", "Release", "Image", "CVE", "Severity", "Package", "Fixable"} {
		if _, ok := index[column]; !ok {
			t.Errorf("export has no %q column, so a row cannot be acted on out of context", column)
		}
	}
	if rows[1][index["CVE"]] != "CVE-2024-3094" || rows[1][index["Release"]] != "25.7.2131" {
		t.Errorf("row = %v", rows[1])
	}
}

func TestSecurityExportXLSXIsAWorkbook(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

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
	for _, part := range []string{"[Content_Types].xml", "xl/workbook.xml", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml"} {
		if !found[part] {
			t.Errorf("workbook is missing %s", part)
		}
	}
}

// A workbook opens on a summary page and holds every table behind it.
//
// Both halves matter and the balance was got wrong twice. The original export
// was a twenty-six-row field/value sheet and one flat findings sheet, and
// nobody exports a spreadsheet to read a headline; removing the summary
// entirely went one step too far, because a workbook that opens on row one of
// ninety thousand findings gives a reader nowhere to stand.
func TestSecurityExportWorkbookCarriesTheTablesOnScreen(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	body, _ := h.getRaw(
		"/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=xlsx&view=detailed")
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}

	var workbook string
	for _, f := range zr.File {
		if f.Name != "xl/workbook.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		workbook = string(raw)
	}
	for _, sheet := range []string{"Summary", "Unique CVEs", "All findings", "Images"} {
		if !strings.Contains(workbook, sheet) {
			t.Errorf("workbook has no %q sheet: %s", sheet, workbook)
		}
	}
	// First, because it is the page a reader lands on.
	if summary, findings := strings.Index(workbook, "Summary"),
		strings.Index(workbook, "All findings"); summary > findings {
		t.Error("the summary is not the workbook's first sheet")
	}

	// And the sheets are formatted: a header row that stays put, columns wide
	// enough to read, and a style table for them to point at. Without these the
	// first thing anybody does with an export is spend two minutes fixing it.
	var findingsSheet string
	styled := false
	for _, f := range zr.File {
		if f.Name != "xl/worksheets/sheet3.xml" && f.Name != "xl/styles.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == "xl/styles.xml" {
			styled = len(raw) > 0
			continue
		}
		findingsSheet = string(raw)
	}
	if !styled {
		t.Error("the workbook has no style table, so every cell renders as the default")
	}
	if !strings.Contains(findingsSheet, "state=\"frozen\"") {
		t.Error("the header row is not frozen: scrolling a real export loses the column names")
	}
	if !strings.Contains(findingsSheet, "<cols>") {
		t.Error("no column widths: every column opens eight characters wide")
	}
	if !strings.Contains(findingsSheet, "autoFilter") {
		t.Error("no filter row on a table somebody is going to filter")
	}
}

// A CSV holds one table, and which one is the caller's to choose.
//
// It used to be "the last sheet assembled", which handed back the Problems tab
// to somebody who exported the vulnerabilities they were looking at.
func TestSecurityExportCSVHonoursTheRequestedTable(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	for table, wantColumn := range map[string]string{
		"unique": "Images affected",
		"images": "Vulnerabilities",
	} {
		body, _ := h.getRaw("/api/v1/products/vendor-a/packages/25.7.2131/security/export" +
			"?format=csv&view=detailed&table=" + table)
		rows, err := csv.NewReader(
			strings.NewReader(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))).ReadAll()
		if err != nil || len(rows) == 0 {
			t.Fatalf("table=%s: parse csv: %v", table, err)
		}
		if !strings.Contains(strings.Join(rows[0], ","), wantColumn) {
			t.Errorf("table=%s gave columns %v, want one called %q", table, rows[0], wantColumn)
		}
	}
}

// The bundle is the tables BESIDE the scanner's own responses, laid out one
// directory per kind, per image, per tag.
//
// Kind first because that is how it is consumed: somebody forwarding a
// vulnerability report to a customer sends `vulnerabilities/`.
func TestSecurityExportBundleCarriesTheScannersOwnResponses(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.syncer.documents[digestA] = map[security.DocumentKind][]byte{
		security.DocumentVulnerabilities: []byte(`{"artifacts":[{"general":{"name":"cfx"}}]}`),
		security.DocumentSBOM:            []byte(`{"bomFormat":"CycloneDX"}`),
	}
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	body, header := h.getRaw(
		"/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=zip&view=detailed")
	if header.Get("Content-Type") != "application/zip" {
		t.Fatalf("content type = %q", header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}

	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	joined := strings.Join(names, "\n")

	if !strings.Contains(joined, "README.txt") {
		t.Error("the bundle has no README, so its recipient has to ask what it is")
	}
	if !strings.Contains(joined, "tables/all-findings.csv") {
		t.Errorf("the bundle has no tables: %v", names)
	}
	for _, dir := range []string{"vulnerabilities/", "sbom/"} {
		if !strings.Contains(joined, dir) {
			t.Errorf("the bundle has no %s directory: %v", dir, names)
		}
	}

	// And the body inside is the scanner's, byte for byte. A bundle carrying
	// this platform's reading of the scanner's answer would be the whole point
	// of the bundle, lost.
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "sbom/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"bomFormat":"CycloneDX"}` {
			t.Errorf("%s = %q, want the scanner's own body unaltered", f.Name, raw)
		}
	}
}

// One image's document, downloaded on its own.
func TestSecurityDocumentDownloadServesOneImagesBody(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.syncer.documents[digestA] = map[security.DocumentKind][]byte{
		security.DocumentSBOM: []byte(`{"bomFormat":"CycloneDX","components":[]}`),
	}
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	body, header := h.getRaw(
		"/api/v1/products/vendor-a/packages/25.7.2131/security/documents/sbom?digest=" + digestA)
	if string(body) != `{"bomFormat":"CycloneDX","components":[]}` {
		t.Errorf("body = %q, want the scanner's own SBOM", body)
	}
	if !strings.Contains(header.Get("Content-Disposition"), "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", header.Get("Content-Disposition"))
	}
	// One repository's findings, under one repository's permissions. A shared
	// cache must never hold them.
	if !strings.Contains(header.Get("Cache-Control"), "private") {
		t.Errorf("Cache-Control = %q, want it private", header.Get("Cache-Control"))
	}
}

// The download URL the response advertises has to be one that resolves.
//
// It carried the CONFIGURED scanner repository in `?repository=`, where every
// route under /packages/{package} reads that parameter as the OCI repository
// path - so the button opened a page reading "no package of product X matches
// Y". Following the advertised link is the only assertion that catches it.
func TestSecurityDocumentURLOnTheResponseResolves(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "openssl", true))
	h.syncer.documents[digestA] = map[security.DocumentKind][]byte{
		security.DocumentSBOM: []byte(`{"bomFormat":"CycloneDX"}`),
	}
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	var resp v1.PackageSecurityResponse
	if code := h.get(
		"/api/v1/products/vendor-a/packages/25.7.2131/security?detail=true", &resp); code != http.StatusOK {
		t.Fatalf("read security: %d", code)
	}

	var sbomURL string
	for _, report := range resp.Reports {
		for _, doc := range report.Documents {
			if doc.Kind == "sbom" {
				sbomURL = doc.URL
			}
		}
	}
	if sbomURL == "" {
		t.Fatal("the response advertises no SBOM download")
	}

	body, header := h.getRaw(sbomURL)
	if string(body) != `{"bomFormat":"CycloneDX"}` {
		t.Errorf("following the advertised URL gave %q", body)
	}
	if !strings.Contains(header.Get("Content-Type"), "json") {
		t.Errorf("content type = %q", header.Get("Content-Type"))
	}
}

// A digest this release does not contain is refused.
//
// Not a 404 for tidiness: the path resolves the artifact from the release's own
// tree so a caller cannot point this Coordinator's JFrog credential at an image
// in a repository they were never entitled to ask about.
func TestSecurityDocumentDownloadRefusesAForeignDigest(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport()
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	code := h.get("/api/v1/products/vendor-a/packages/25.7.2131/security/documents/sbom"+
		"?digest=sha256:"+strings.Repeat("f", 64), nil)
	if code != http.StatusNotFound {
		t.Errorf("got %d for a digest this release does not contain, want 404", code)
	}
}

func TestSecurityRoutesAreAbsentWithoutTheDependency(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("25.7.2131", digestA)

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
