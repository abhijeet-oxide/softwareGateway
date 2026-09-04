package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The manifest these tests are evidence for. Shaped like helm's own output -
// a separator, a source marker, then the object - because the line offsets are
// the whole point and helm's leading lines are what makes them what they are.
const evidenceChart = `---
# Source: alpha/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: main
          image: registry.example/api:1.0
          securityContext:
            runAsUser: 0
`

type evidenceHarness struct {
	*apiHarness
	packageID int64
	runID     string
}

func newEvidenceHarness(t *testing.T) *evidenceHarness {
	t.Helper()
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.ComplianceStore = d.Packages
		d.ComplianceEvidence = d.Packages
	})
	pkg := h.seedPackage("25.7.2131", digestA)

	const runID = "run-evidence-1"
	if err := h.packages.StartComplianceRun(t.Context(), runID, pkg, "manual"); err != nil {
		t.Fatal(err)
	}

	// One finding whose locus resolves, and one whose locus is about a field
	// that is not there - which is half of every real run.
	results := []store.ComplianceResultRow{
		{Seq: 0, CheckID: "SEC-01", CheckTitle: "Containers do not run as root",
			Severity: "critical", Outcome: "fail", Determinacy: "fixed",
			Chart: "alpha", ChartVersion: "1.0.0",
			SourceFile: "alpha/templates/deployment.yaml", RenderedLine: 2,
			Kind: "Deployment", Name: "api", Container: "main",
			Locus:   "spec.template.spec.containers[0].securityContext.runAsUser",
			Message: "runs as root"},
		{Seq: 1, CheckID: "RES-02", CheckTitle: "Every container declares a memory limit",
			Severity: "critical", Outcome: "fail", Determinacy: "fixed",
			Chart: "alpha", ChartVersion: "1.0.0",
			SourceFile: "alpha/templates/deployment.yaml", RenderedLine: 2,
			Kind: "Deployment", Name: "api", Container: "main",
			Locus:   "spec.template.spec.containers[0].resources.limits.memory",
			Message: "no memory limit"},
		// A check that could not be decided, addressed to a chart that never
		// rendered - so there is nothing to show, and saying why matters.
		{Seq: 2, CheckID: "SEC-01", Severity: "critical", Outcome: "error",
			Chart: "beta", Message: "the chart did not render"},
	}
	rendered := []store.ComplianceRenderedRow{
		{Seq: 0, Chart: "alpha", ChartVersion: "1.0.0", Content: evidenceChart,
			Lines: strings.Count(evidenceChart, "\n"), Bytes: len(evidenceChart)},
		{Seq: 1, SourceFile: "manifests/namespace.yaml",
			Content: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: apps\n",
			Lines:   4, Bytes: 55},
	}
	if err := h.packages.FinishComplianceRun(t.Context(),
		store.ComplianceRunRow{
			ID: runID, PackageID: pkg, State: store.ComplianceComplete, Verdict: "fail",
			HelmVersion: "v3.16.3", KubeVersion: "1.30.0", BundleDigest: "sha256:rulebook",
		},
		[]store.ComplianceChartRow{
			{Name: "alpha", Version: "1.0.0", Status: "ok", Resources: 1},
			{Name: "beta", Version: "2.0.0", Status: "failed", Error: "template error"},
		},
		results, rendered); err != nil {
		t.Fatal(err)
	}
	return &evidenceHarness{apiHarness: h, packageID: pkg, runID: runID}
}

func (h *evidenceHarness) text(path string) (int, string, http.Header) {
	h.t.Helper()
	resp, err := http.Get(h.server.URL + path) //nolint:noctx // test client
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

const evidenceBase = "/api/v1/products/vendor-a/packages/25.7.2131/compliance/rendered"

func TestRenderedIndexListsWhatCanBeShown(t *testing.T) {
	h := newEvidenceHarness(t)

	var resp ListRenderedResponse
	if code := h.get(evidenceBase, &resp); code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	if resp.RunID != h.runID || len(resp.Documents) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	// The two ways a document is named, and the key the other endpoints take.
	byKey := map[string]RenderedDocumentView{}
	for _, d := range resp.Documents {
		byKey[d.Document] = d
	}
	if d, ok := byKey["alpha"]; !ok || d.Chart != "alpha" || d.ChartVersion != "1.0.0" {
		t.Errorf("the chart document = %+v", d)
	}
	if d, ok := byKey["manifests/namespace.yaml"]; !ok || d.Chart != "" {
		t.Errorf("the plain manifest = %+v", d)
	}
	if resp.TotalBytes != len(evidenceChart)+55 {
		t.Errorf("TotalBytes = %d", resp.TotalBytes)
	}
	// The index carries no content: it renders beside a coverage table, and
	// the content is megabytes.
	if _, body, _ := h.text(evidenceBase); strings.Contains(body, "runAsUser") {
		t.Error("the index inlined a document's content")
	}
}

// The feature, end to end: a finding, and the line of the manifest it is about.
func TestExcerptPointsAtTheLineTheFindingIsAbout(t *testing.T) {
	h := newEvidenceHarness(t)

	var resp ComplianceExcerptResponse
	if code := h.get(evidenceBase+"/excerpt?seq=0&context=4", &resp); code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	if resp.Document != "alpha" || resp.Chart != "alpha" {
		t.Fatalf("resp = %+v", resp)
	}

	want := lineNumberOf(t, evidenceChart, "runAsUser: 0")
	if resp.FocusLine != want {
		t.Fatalf("FocusLine = %d, want %d", resp.FocusLine, want)
	}
	// And the numbering has to agree with the document, or a line quoted out
	// of the excerpt into a mail points at nothing.
	got := resp.Lines[resp.FocusLine-resp.StartLine]
	if !strings.Contains(got, "runAsUser: 0") {
		t.Fatalf("the line the excerpt calls %d is %q", resp.FocusLine, got)
	}
}

// The absent-field half. There is no line to point at, and the response says so
// with a zero rather than picking one - but it still lands on the container the
// field is missing from.
func TestExcerptForAnAbsentFieldPointsAtNoLine(t *testing.T) {
	h := newEvidenceHarness(t)

	var resp ComplianceExcerptResponse
	if code := h.get(evidenceBase+"/excerpt?seq=1", &resp); code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	if resp.FocusLine != 0 {
		t.Fatalf("FocusLine = %d for a field that is not in the document", resp.FocusLine)
	}
	if want := lineNumberOf(t, evidenceChart, "- name: main"); resp.NearLine != want {
		t.Fatalf("NearLine = %d, want the container at %d", resp.NearLine, want)
	}
	if resp.Locus == "" {
		t.Error("the response did not say what was looked for and not found")
	}
}

// An undecided check is addressed to a chart that never rendered. The reason
// there is nothing to show is the same reason the check could not be decided,
// and saying that is more useful than a bare 404.
func TestExcerptForAChartThatNeverRendered(t *testing.T) {
	h := newEvidenceHarness(t)

	code, body, _ := h.text(evidenceBase + "/excerpt?seq=2")
	if code != http.StatusNotFound {
		t.Fatalf("got %d", code)
	}
	if !strings.Contains(body, "did not render") {
		t.Errorf("the reason was not given: %s", body)
	}
}

func TestExcerptRejectsANonResult(t *testing.T) {
	h := newEvidenceHarness(t)
	for _, q := range []string{"", "?seq=", "?seq=nine", "?seq=-1"} {
		if code, _, _ := h.text(evidenceBase + "/excerpt" + q); code != http.StatusBadRequest {
			t.Errorf("excerpt%s = %d, want 400", q, code)
		}
	}
	if code, _, _ := h.text(evidenceBase + "/excerpt?seq=99"); code != http.StatusNotFound {
		t.Error("a result that does not exist did not 404")
	}
}

func TestRenderedContentServesOneDocumentAsText(t *testing.T) {
	h := newEvidenceHarness(t)

	code, body, hdr := h.text(evidenceBase + "/content?document=alpha")
	if code != http.StatusOK {
		t.Fatalf("got %d: %s", code, body)
	}
	if body != evidenceChart {
		t.Error("the content served is not the content stored")
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/yaml") {
		t.Errorf("Content-Type = %q", ct)
	}
	// Vendor-authored text from our own origin: a browser that sniffed it into
	// HTML would run it.
	if hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the response did not forbid content sniffing")
	}
	if hdr.Get("Content-Disposition") != "" {
		t.Error("a plain read offered itself as a download")
	}

	// A plain manifest is addressed by its path.
	if code, body, _ := h.text(evidenceBase + "/content?document=manifests/namespace.yaml"); code != http.StatusOK ||
		!strings.Contains(body, "kind: Namespace") {
		t.Errorf("the plain manifest: %d %q", code, body)
	}
	if code, _, _ := h.text(evidenceBase + "/content?document=beta"); code != http.StatusNotFound {
		t.Error("a chart that never rendered served something")
	}
}

// The whole release in one file, which is the artifact a vendor conversation
// needs: every document, each named, under a header saying what produced them.
func TestRenderedContentDownloadsTheWholeRelease(t *testing.T) {
	h := newEvidenceHarness(t)

	code, body, hdr := h.text(evidenceBase + "/content?download=1")
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	for _, want := range []string{
		"# chart: alpha 1.0.0",
		"# file: manifests/namespace.yaml (shipped as-is, not rendered)",
		"runAsUser: 0",
		"kind: Namespace",
		// A manifest set with no statement of what produced it is exactly the
		// artifact this feature exists to replace.
		"# run:          " + h.runID,
		"# helm:         v3.16.3",
		"# kubeVersion:  1.30.0",
		"# rulebook:     sha256:rulebook",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the download is missing %q", want)
		}
	}
	if cd := hdr.Get("Content-Disposition"); !strings.Contains(cd, `filename="25.7.2131-rendered.yaml"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// A release nobody has checked has no manifests to show, and that is a
// different answer from a release whose evidence was reclaimed.
func TestRenderedOnAnUncheckedRelease(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.ComplianceStore = d.Packages
		d.ComplianceEvidence = d.Packages
	})
	h.seedPackage("25.7.2131", digestA)

	code, body, _ := (&evidenceHarness{apiHarness: h}).text(evidenceBase)
	if code != http.StatusNotFound {
		t.Fatalf("got %d", code)
	}
	if !strings.Contains(body, "has not been checked") {
		t.Errorf("body = %s", body)
	}
}

func TestEvidenceFilenameIsSafeAndNamed(t *testing.T) {
	cases := map[[2]string]string{
		{"25.7.2131", "alpha"}:      "25.7.2131-alpha-rendered.yaml",
		{"orb_24.7", "sub/chart"}:   "orb_24.7-sub-chart-rendered.yaml",
		{`a"b`, `../../etc/passwd`}: "a-b-etc-passwd-rendered.yaml",
		{"", ""}:                    "release-rendered.yaml",
	}
	for in, want := range cases {
		if got := evidenceFilename(in[0], in[1], in[1] == ""); got != want {
			t.Errorf("evidenceFilename(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// lineNumberOf is the 1-based line a snippet is on.
func lineNumberOf(t *testing.T, doc, snippet string) int {
	t.Helper()
	for i, l := range strings.Split(strings.TrimSuffix(doc, "\n"), "\n") {
		if strings.Contains(l, snippet) {
			return i + 1
		}
	}
	t.Fatalf("no line contains %q", snippet)
	return 0
}
