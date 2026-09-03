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
	// offered is every POST /images, and submitted only the ones that created a
	// record. Anchore returns the record it already holds for a digest it knows,
	// so those two differ - and the difference is what makes a second press a
	// no-op in Anchore rather than one this Coordinator has to arrange.
	offered    []string
	submitted  []string
	associated map[string]bool
	// rejectSubmit makes POST /images answer 400 with this detail, the way a
	// real Anchore does when it cannot pull from the registry.
	rejectSubmit string
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
		f.offered = append(f.offered, digest)
		if f.rejectSubmit != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": f.rejectSubmit})
			return
		}
		// Already known: hand back what is held and start nothing. This is what
		// makes re-offering an image safe, and it is why the Coordinator no
		// longer lists the account's images to decide what to send.
		if status, known := f.images[digest]; known && status != "" {
			write([]map[string]any{{"image_digest": digest, "analysis_status": status}})
			return
		}
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
		Enabled:     true,
		Registry:    "internal.example.com",
		Repository:  "vendor/app",
		Grouping:    true,
		Submit:      true,
		Concurrency: 4,
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

// scanOptions is a detailed scan of a named release, which is what a sync
// makes: the release identity is what Anchore's Application/Version grouping
// is built from.
func scanOptions() security.ScanOptions {
	return security.ScanOptions{
		Detail:  true,
		Release: security.ReleaseRef{Product: "cfx-5000", Version: "25.7.2131", Label: "cfx-5000 25.7.2131"},
	}
}

// registerOptions is a registration of a named release, which is what the
// Replicate button makes.
func registerOptions() security.RegisterOptions {
	return security.RegisterOptions{
		Release: security.ReleaseRef{Product: "cfx-5000", Version: "25.7.2131", Label: "cfx-5000 25.7.2131"},
	}
}

func imageRef(name, digest string) security.ArtifactRef {
	return security.ArtifactRef{
		Name: name, Tag: "1.0", Digest: digest,
		Repository: "vendor/app/" + name, Registry: "internal.example.com", Kind: "image",
	}
}

// THE TWO ACTS, and the seam between them.
//
// Register tells Anchore the release exists and returns; it does not wait for
// analysis, and it associates every submitted image immediately so the release
// is legible in Anchore's own interface straight away. Scan reads whatever has
// finished. Neither blocks on the other.
func TestRegisterSubmitsAndGroupsWithoutWaiting(t *testing.T) {
	f := newFake()
	// Deliberately still analysing. The whole point is that this does not stop
	// the release being grouped.
	f.analyzeAfter["sha256:aaa"] = 1000

	p := newProvider(t, f, nil)
	refs := []security.ArtifactRef{imageRef("app", "sha256:aaa")}

	reg, err := p.Register(context.Background(), refs, registerOptions())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Submitted != 1 {
		t.Errorf("submitted = %d, want the one unknown image", reg.Submitted)
	}
	if !f.associated["sha256:aaa"] {
		t.Error("an image still analysing was not associated - the release is invisible in Anchore until it finishes")
	}
	if reg.State != security.RegistrationComplete {
		t.Errorf("state = %q, want registered", reg.State)
	}
	if reg.Application == "" || reg.Version == "" {
		t.Errorf("the registration did not record the application version: %+v", reg)
	}
	// Nothing analysed yet, and the registration says so rather than pretending.
	if reg.Analysed != 0 {
		t.Errorf("analysed = %d, want 0 - nothing has finished", reg.Analysed)
	}
}

// A second press starts no second analysis, and reports that it did not.
//
// The idempotence lives in ANCHORE, not here: every image is offered every
// time, and Anchore returns the record it already holds. The Coordinator used
// to list the account's images first and send only what was missing, which put
// an unbounded listing on the critical path of every replication and aborted
// the whole run when it failed.
func TestRegisterIsIdempotent(t *testing.T) {
	f := newFake()
	p := newProvider(t, f, nil)
	refs := []security.ArtifactRef{imageRef("app", "sha256:aaa")}

	if _, err := p.Register(context.Background(), refs, registerOptions()); err != nil {
		t.Fatalf("first register: %v", err)
	}
	analysedOnce := len(f.submitted)

	reg, err := p.Register(context.Background(), refs, registerOptions())
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if len(f.offered) != 2 {
		t.Errorf("the image was offered %d times, want 2 - every image is offered every "+
			"time and Anchore decides what that means", len(f.offered))
	}
	if len(f.submitted) != analysedOnce {
		t.Errorf("the second press started a second analysis: %v", f.submitted)
	}
	if reg.AlreadyKnown != 1 {
		t.Errorf("a no-op press reported alreadyKnown=%d, want 1 - a silent no-op reads "+
			"as a broken button", reg.AlreadyKnown)
	}
	if reg.State != security.RegistrationComplete {
		t.Errorf("state = %q, want registered", reg.State)
	}
}

// Anchore refusing to PULL is not Anchore being unreachable.
//
// It answers `400 cannot fetch image digest/manifest from registry` when it
// cannot reach the registry the image is in. That classified as unsupported,
// which had no case, so it was reported as "Anchore could not be reached" - the
// exact inverse of what happened, sending an operator to check the one path
// that is demonstrably fine.
func TestPullFailureIsNotReportedAsUnreachable(t *testing.T) {
	f := newFake()
	f.rejectSubmit = "cannot fetch image digest/manifest from registry"
	p := newProvider(t, f, nil)

	reg, err := p.Register(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:aaa")}, registerOptions())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Submitted != 0 {
		t.Fatalf("submitted = %d, want 0 - Anchore rejected it", reg.Submitted)
	}

	why := reg.FirstFailure()
	if strings.Contains(why, "could not be reached") {
		t.Errorf("a rejection was reported as unreachable, which sends an operator to the "+
			"wrong system: %q", why)
	}
	// ANCHORE'S OWN SENTENCE, kept whole. The remedy is derived separately and
	// named the registry - see registrationRemedy in internal/api - so this
	// string stays quotable evidence rather than a paragraph.
	if !strings.Contains(why, "cannot fetch image digest/manifest from registry") {
		t.Errorf("the scanner's own message was not kept: %q", why)
	}
	if !IsPullFailure(why) {
		t.Errorf("a pull failure was not recognised as one, so no remedy is offered: %q", why)
	}
}

// Nothing accepted means there is nothing to attach, so the application step is
// skipped entirely.
//
// It was not: a release whose every image Anchore refused still went on to find
// or create an application and a version, which is four requests to group an
// empty set - and a transcript that ended "application ... is ready" directly
// under "154 of 154 images failed".
func TestApplicationIsNotCreatedWhenNothingWasAccepted(t *testing.T) {
	f := newFake()
	f.rejectSubmit = "cannot fetch image digest/manifest from registry"
	p := newProvider(t, f, nil)

	reg, err := p.Register(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:aaa")}, registerOptions())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Application != "" || reg.Version != "" {
		t.Errorf("an application was created for a release with nothing in it: %+v", reg)
	}
	if n := f.calls["POST /applications"]; n != 0 {
		t.Errorf("POST /applications called %d times, want 0", n)
	}
	if reg.State != security.RegistrationFailed {
		t.Errorf("state = %q, want failed", reg.State)
	}
}

// A sync NEVER submits. An image Anchore was never told about is reported as
// such, with the remedy named - not quietly registered by a read.
func TestScanNeverSubmits(t *testing.T) {
	f := newFake()
	p := newProvider(t, f, nil)

	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:zzz")}, scanOptions())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(f.submitted) != 0 {
		t.Errorf("a read submitted an image: %v", f.submitted)
	}
	if reports[0].Status != security.StatusNotScanned {
		t.Errorf("status = %q, want not_scanned", reports[0].Status)
	}
	if reports[0].Message == "" {
		t.Error("an unregistered image must say so")
	}
}

// Register then Scan is the whole flow, and the results arrive on the read.
func TestRegisterThenScanReadsResults(t *testing.T) {
	f := newFake()
	f.vulns["sha256:aaa"] = kevResponse

	p := newProvider(t, f, nil)
	refs := []security.ArtifactRef{imageRef("app", "sha256:aaa")}

	if _, err := p.Register(context.Background(), refs, registerOptions()); err != nil {
		t.Fatalf("register: %v", err)
	}
	reports, err := p.Scan(context.Background(), refs, scanOptions())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reports[0].Status != security.StatusScanned {
		t.Fatalf("status = %q (%s)", reports[0].Status, reports[0].Message)
	}
	if len(reports[0].Findings) != 2 {
		t.Errorf("expected two findings, got %d", len(reports[0].Findings))
	}
}

// A release that has not been transferred has nothing for Anchore to pull, and
// that is a transfer to run rather than anything to do with Anchore.
func TestRegisterReportsUnreplicatedImages(t *testing.T) {
	f := newFake()
	p := newProvider(t, f, func(s *Settings) { s.Registry = "" })

	ref := imageRef("app", "sha256:ddd")
	ref.Registry = ""
	reg, err := p.Register(context.Background(), []security.ArtifactRef{ref}, registerOptions())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Expected != 0 || len(reg.Failed) != 1 {
		t.Fatalf("expected the image to be reported unusable: %+v", reg)
	}
	if reg.Message == "" {
		t.Error("a release with nothing to register must say why")
	}
}

// RegistrationFor asks Anchore rather than trusting our own record, because our
// record cannot know somebody deleted the application there.
func TestRegistrationForReadsAnchore(t *testing.T) {
	f := newFake()
	p := newProvider(t, f, nil)
	refs := []security.ArtifactRef{imageRef("app", "sha256:aaa")}

	before, err := p.RegistrationFor(context.Background(), refs, registerOptions())
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	if before.Associated != 0 || before.State == security.RegistrationComplete {
		t.Errorf("an unregistered release read as registered: %+v", before)
	}

	if _, err := p.Register(context.Background(), refs, registerOptions()); err != nil {
		t.Fatalf("register: %v", err)
	}
	after, err := p.RegistrationFor(context.Background(), refs, registerOptions())
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	if after.Associated != 1 || after.State != security.RegistrationComplete {
		t.Errorf("a registered release read as %+v", after)
	}
}

// The field that justifies the integration, and the sort order that acts on it.
func TestKEVIsReadAndSortsFirst(t *testing.T) {
	f := newFake()
	f.images["sha256:aaa"] = AnalysisAnalyzed
	f.vulns["sha256:aaa"] = kevResponse

	p := newProvider(t, f, nil)
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:aaa")}, scanOptions())
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

	p := newProvider(t, f, nil)
	reports, err := p.Scan(context.Background(),
		[]security.ArtifactRef{imageRef("app", "sha256:bbb")}, scanOptions())
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
		[]security.ArtifactRef{imageRef("app", "sha256:ccc")}, scanOptions())
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
		[]security.ArtifactRef{ref}, scanOptions())
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
		[]security.ArtifactRef{chart}, scanOptions())
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
		scanOptions()); err != nil {
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
