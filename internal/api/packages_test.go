package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// fakeDiscoverer stands in for the loop, so handler tests do not need a
// registry.
type fakeDiscoverer struct {
	running    bool
	result     discovery.ScanResult
	err        error
	lastSource string
	calls      int
	started    int
	progress   []discovery.SourceProgress
	products   []string
}

func (f *fakeDiscoverer) Running() bool { return f.running }

func (f *fakeDiscoverer) Trigger(_ context.Context, _, source string) (discovery.ScanResult, error) {
	f.calls++
	f.lastSource = source
	return f.result, f.err
}

func (f *fakeDiscoverer) TriggerProduct(_ context.Context, _ string) (discovery.ScanResult, error) {
	f.calls++
	f.lastSource = ""
	return f.result, f.err
}

func (f *fakeDiscoverer) StartProduct(_ string) (int, int, error) {
	f.calls++
	f.started++
	return 1, 0, f.err
}

func (f *fakeDiscoverer) InspectPackage(
	_ context.Context, _ *store.Packages, pkg store.PackageRow, _ string,
) (discovery.InspectResult, error) {
	f.calls++
	return discovery.InspectResult{Package: pkg, Fetched: 2, Artifacts: 3, Blobs: 5}, f.err
}

func (f *fakeDiscoverer) Progress(_ string) []discovery.SourceProgress {
	return f.progress
}

func (f *fakeDiscoverer) Products() []string { return f.products }

const testProductDoc = `
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
      anonymous: true
      default: true
`

// apiHarness wires a Server over a real migrated store.
type apiHarness struct {
	t          *testing.T
	server     *httptest.Server
	store      store.Store
	packages   *store.Packages
	discoverer *fakeDiscoverer
	productID  int64
	repoID     int64
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	return newAPIHarnessWith(t, nil)
}

// newAPIHarnessWith is the same harness with the dependencies adjusted before
// the server is built — for the handful of tests about a dependency's
// behaviour rather than the store's.
func newAPIHarnessWith(t *testing.T, adjust func(*Deps)) *apiHarness {
	t.Helper()
	ctx := t.Context()

	st, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "api.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(ctx, st, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vendor-a.yaml"), []byte(testProductDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := product.NewLoader(dir, product.NewSecretResolver(t.TempDir()))
	res, err := loader.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(res.Valid) != 1 {
		t.Fatalf("expected 1 product, got %d valid / %d invalid", len(res.Valid), len(res.Invalid))
	}

	products := product.NewRegistry()
	products.Swap(res)

	rec, err := catalog.NewCatalog(st).Reconcile(ctx, res.Valid)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ref := rec.Products["vendor-a"]

	disc := &fakeDiscoverer{running: true}
	packages := store.NewPackages(st)

	deps := Deps{
		Products:  products,
		Store:     st,
		Packages:  packages,
		Discovery: disc,
		Component: "coordinator",
	}
	if adjust != nil {
		adjust(&deps)
	}
	srv := NewServer(deps)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &apiHarness{
		t: t, server: ts, store: st, packages: packages, discoverer: disc,
		productID: ref.ID, repoID: ref.Repositories["vendor"],
	}
}

// seedPackage inserts a package directly, so handler tests do not depend on
// discovery.
func (h *apiHarness) seedPackage(tag, digest string) int64 {
	h.t.Helper()

	tx, err := h.store.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertPackage(h.t.Context(), tx, store.PackageRow{
		ProductID:      h.productID,
		SourceRepoID:   h.repoID,
		Tag:            tag,
		ManifestDigest: digest,
		MediaType:      "application/vnd.oci.image.manifest.v1+json",
		TotalBytes:     int64Ptr(123456789),
		ArtifactCount:  1,
		BlobCount:      intPtr(2),
	})
	if err != nil {
		h.t.Fatalf("seed package: %v", err)
	}
	if _, err := h.packages.InsertArtifact(h.t.Context(), tx, store.ArtifactRow{
		PackageID: id,
		Digest:    digest,
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		SizeBytes: 512,
		Raw:       []byte(`{"schemaVersion":2}`),
	}); err != nil {
		h.t.Fatalf("seed artifact: %v", err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *apiHarness) get(path string, out any) int {
	h.t.Helper()
	resp, err := http.Get(h.server.URL + path) //nolint:noctx // test client
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func (h *apiHarness) post(path, body string, out any) int {
	h.t.Helper()
	resp, err := http.Post(h.server.URL+path, "application/json", strings.NewReader(body)) //nolint:noctx // test client
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

const digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestListPackagesEmpty(t *testing.T) {
	h := newAPIHarness(t)

	var resp v1.ListPackagesResponse
	if code := h.get("/api/v1/products/vendor-a/packages", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	// An empty array, never null: a client iterating the field must not have to
	// nil-check it.
	if resp.Packages == nil {
		t.Error("packages must serialise as [] rather than null")
	}
	if len(resp.Packages) != 0 {
		t.Errorf("expected no packages, got %d", len(resp.Packages))
	}
}

func TestListAndGetPackage(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("v1.0.0", digestA)
	h.seedPackage("v1.1.0", digestB)

	var list v1.ListPackagesResponse
	if code := h.get("/api/v1/products/vendor-a/packages", &list); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(list.Packages) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(list.Packages))
	}

	// AIP-141: a byte count crosses the wire as a string, because JSON numbers
	// are doubles and lose precision above 2^53.
	if list.Packages[0].TotalBytes == nil || *list.Packages[0].TotalBytes != "123456789" {
		t.Errorf("expected totalBytes as a string, got %q", derefBytes(list.Packages[0].TotalBytes))
	}
	// AIP-126: enums are SCREAMING_SNAKE_CASE on the wire.
	if list.Packages[0].State != v1.PackageDiscovered {
		t.Errorf("expected DISCOVERED, got %q", list.Packages[0].State)
	}
	if !strings.HasPrefix(list.Packages[0].Name, "products/vendor-a/packages/") {
		t.Errorf("expected an AIP-122 resource name, got %q", list.Packages[0].Name)
	}

	var one v1.Package
	if code := h.get("/api/v1/products/vendor-a/packages/v1.0.0", &one); code != http.StatusOK {
		t.Fatalf("expected 200 fetching by tag, got %d", code)
	}
	if one.Tag != "v1.0.0" {
		t.Errorf("expected v1.0.0, got %q", one.Tag)
	}

	// By digest too, since that is the stable identity.
	var byDigest v1.Package
	if code := h.get("/api/v1/products/vendor-a/packages/"+digestB, &byDigest); code != http.StatusOK {
		t.Fatalf("expected 200 fetching by digest, got %d", code)
	}
	if byDigest.Tag != "v1.1.0" {
		t.Errorf("expected the v1.1.0 package by digest, got %q", byDigest.Tag)
	}
}

func TestGetPackageNotFound(t *testing.T) {
	h := newAPIHarness(t)

	if code := h.get("/api/v1/products/vendor-a/packages/nope", nil); code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown package, got %d", code)
	}
	if code := h.get("/api/v1/products/no-such-product/packages", nil); code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown product, got %d", code)
	}
}

func TestListPackagesFilters(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("v1.0.0", digestA)
	h.seedPackage("v1.1.0", digestB)

	var byTag v1.ListPackagesResponse
	h.get("/api/v1/products/vendor-a/packages?tag=v1.0.0", &byTag)
	if len(byTag.Packages) != 1 || byTag.Packages[0].Tag != "v1.0.0" {
		t.Errorf("tag filter did not narrow the result: %+v", byTag.Packages)
	}

	var byState v1.ListPackagesResponse
	h.get("/api/v1/products/vendor-a/packages?state=DISCOVERED", &byState)
	if len(byState.Packages) != 2 {
		t.Errorf("expected 2 discovered packages, got %d", len(byState.Packages))
	}

	var none v1.ListPackagesResponse
	h.get("/api/v1/products/vendor-a/packages?state=SUPERSEDED", &none)
	if len(none.Packages) != 0 {
		t.Errorf("expected no superseded packages, got %d", len(none.Packages))
	}

	if code := h.get("/api/v1/products/vendor-a/packages?state=NONSENSE", nil); code != http.StatusBadRequest {
		t.Errorf("expected 400 for an invalid state, got %d", code)
	}
	if code := h.get("/api/v1/products/vendor-a/packages?pageSize=-1", nil); code != http.StatusBadRequest {
		t.Errorf("expected 400 for a negative pageSize, got %d", code)
	}
	if code := h.get("/api/v1/products/vendor-a/packages?pageToken=abc", nil); code != http.StatusBadRequest {
		t.Errorf("expected 400 for a malformed pageToken, got %d", code)
	}
}

func TestListPackagesPagination(t *testing.T) {
	h := newAPIHarness(t)
	for i := range 5 {
		h.seedPackage("v1."+string(rune('0'+i))+".0",
			"sha256:"+strings.Repeat(string(rune('a'+i)), 64))
	}

	var page1 v1.ListPackagesResponse
	h.get("/api/v1/products/vendor-a/packages?pageSize=2", &page1)
	if len(page1.Packages) != 2 {
		t.Fatalf("expected 2 on the first page, got %d", len(page1.Packages))
	}
	if page1.NextPageToken == "" {
		t.Fatal("expected a nextPageToken with more results outstanding")
	}

	var page2 v1.ListPackagesResponse
	h.get("/api/v1/products/vendor-a/packages?pageSize=2&pageToken="+page1.NextPageToken, &page2)
	if len(page2.Packages) != 2 {
		t.Fatalf("expected 2 on the second page, got %d", len(page2.Packages))
	}

	var page3 v1.ListPackagesResponse
	h.get("/api/v1/products/vendor-a/packages?pageSize=2&pageToken="+page2.NextPageToken, &page3)
	if len(page3.Packages) != 1 {
		t.Fatalf("expected 1 on the last page, got %d", len(page3.Packages))
	}
	// The last page must not promise another. Claiming one that turns out empty
	// is how a paging client ends up in an extra round trip forever.
	if page3.NextPageToken != "" {
		t.Errorf("expected no token on the last page, got %q", page3.NextPageToken)
	}

	// No package appears twice across pages.
	seen := map[string]bool{}
	for _, p := range append(append(page1.Packages, page2.Packages...), page3.Packages...) {
		if seen[p.PackageID] {
			t.Errorf("package %s appeared on more than one page", p.PackageID)
		}
		seen[p.PackageID] = true
	}
}

func TestListArtifacts(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("v1.0.0", digestA)

	var resp v1.ListArtifactsResponse
	if code := h.get("/api/v1/products/vendor-a/packages/v1.0.0/artifacts", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(resp.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(resp.Artifacts))
	}
	if resp.Artifacts[0].SizeBytes != "512" {
		t.Errorf("expected sizeBytes as a string, got %q", resp.Artifacts[0].SizeBytes)
	}
	if resp.Artifacts[0].Digest != digestA {
		t.Errorf("expected the artifact digest, got %q", resp.Artifacts[0].Digest)
	}
}

func TestDiscoverPackages(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.result = discovery.ScanResult{
		TagsListed: 12, TagsAdmitted: 8, New: 2, Superseded: 1, Requests: 1,
	}

	var resp v1.DiscoverPackagesResponse
	code := h.post("/api/v1/products/vendor-a/packages:discover", "", &resp)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.PackagesDiscovered != 2 || resp.TagsListed != 12 || resp.RequestsCreated != 1 {
		t.Errorf("scan result not reported faithfully: %+v", resp)
	}
	if h.discoverer.calls != 1 {
		t.Errorf("expected 1 trigger, got %d", h.discoverer.calls)
	}
	// An empty body means "every source".
	if h.discoverer.lastSource != "" {
		t.Errorf("expected a whole-product scan, got source %q", h.discoverer.lastSource)
	}
}

func TestDiscoverPackagesWithSource(t *testing.T) {
	h := newAPIHarness(t)

	var resp v1.DiscoverPackagesResponse
	code := h.post("/api/v1/products/vendor-a/packages:discover", `{"source":"vendor"}`, &resp)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if h.discoverer.lastSource != "vendor" {
		t.Errorf("expected the named source to be scanned, got %q", h.discoverer.lastSource)
	}
}

// A follower must say WHY it is not scanning, not 404 as though the feature
// were missing.
func TestDiscoverOnFollowerIsAPrecondition(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.running = false

	code := h.post("/api/v1/products/vendor-a/packages:discover", "", nil)
	if code != http.StatusConflict && code != http.StatusPreconditionFailed {
		t.Errorf("expected a precondition failure, got %d", code)
	}
	if h.discoverer.calls != 0 {
		t.Error("a follower must not trigger a scan")
	}
}

func TestDiscoverPropagatesScanFailure(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.err = errors.New("registry unreachable")

	code := h.post("/api/v1/products/vendor-a/packages:discover", "", nil)
	if code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the registry is down, got %d", code)
	}
}

func TestDiscoverUnknownSource(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.err = discovery.ErrNoSuchSource

	code := h.post("/api/v1/products/vendor-a/packages:discover", `{"source":"nope"}`, nil)
	if code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown source, got %d", code)
	}
}

func TestDiscoverRejectsUnknownFields(t *testing.T) {
	h := newAPIHarness(t)

	// A typo'd field must be rejected rather than silently ignored: silently
	// scanning every source when the caller asked for one is worse than an
	// error.
	code := h.post("/api/v1/products/vendor-a/packages:discover", `{"sourcee":"vendor"}`, nil)
	if code != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown field, got %d", code)
	}
}

// Routes must be absent, not broken, when nothing backs them.
func TestPackageRoutesAbsentWithoutAStore(t *testing.T) {
	srv := NewServer(Deps{Products: product.NewRegistry(), Component: "coordinator"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/products/vendor-a/packages") //nolint:noctx // test client
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 when no package store is configured, got %d", resp.StatusCode)
	}
}

func int64Ptr(v int64) *int64 { return &v }
func intPtr(v int) *int       { return &v }

func derefBytes(v *v1.Int64String) string {
	if v == nil {
		return "<not measured>"
	}
	return string(*v)
}

// seedPackageIn inserts a package in a repository OTHER than the product's
// configured one, so the multi-repository case can be tested. Discovery creates
// these rows when a source enumerates a registry's catalog.
func (h *apiHarness) seedPackageIn(repoPath, tag, digest string) int64 {
	h.t.Helper()

	tx, err := h.store.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	repoID, err := h.packages.EnsureRepository(h.t.Context(), tx, h.productID,
		"source", "vendor/"+repoPath, "registry.example.com", repoPath, "generic", "discovery", "")
	if err != nil {
		h.t.Fatalf("ensure repository %s: %v", repoPath, err)
	}

	id, err := h.packages.InsertPackage(h.t.Context(), tx, store.PackageRow{
		ProductID:      h.productID,
		SourceRepoID:   repoID,
		Tag:            tag,
		ManifestDigest: digest,
		MediaType:      "application/vnd.oci.image.index.v1+json",
		ArtifactCount:  1,
	})
	if err != nil {
		h.t.Fatalf("seed package %s:%s: %v", repoPath, tag, err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// A vendor's version tag appears in many of a product's repositories. The
// handler must refuse rather than pick, and must say what to type instead —
// an error that states the problem without the fix is one the user then has to
// go and research.
func TestGetPackageRefusesAnAmbiguousTag(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackageIn("orbs/cfx-5000-k8s", "orb_23.8.1076", digestA)
	h.seedPackageIn("orbs/cfx-5000-db", "orb_23.8.1076", digestB)

	var prob v1.Problem
	code := h.get("/api/v1/products/vendor-a/packages/orb_23.8.1076", &prob)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an ambiguous reference, got %d", code)
	}
	if prob.Code != v1.CodeInvalidArgument {
		t.Errorf("code = %s, want %s", prob.Code, v1.CodeInvalidArgument)
	}
	// The message must name every candidate AND show a reference that works,
	// because an error that states the problem without the fix is one the user
	// then has to go and research.
	for _, want := range []string{
		"orbs/cfx-5000-k8s",
		"orbs/cfx-5000-db",
		"orbs/cfx-5000-db:orb_23.8.1076",
	} {
		if !strings.Contains(prob.Detail, want) {
			t.Errorf("the message must contain %q so the caller can act on it: %q", want, prob.Detail)
		}
	}
}

// A repository with no slash can be scoped in the path segment itself. This is
// the form the router can actually route: a slash in a path segment does not
// survive %2F decoding, which is why the client moves a slashed repository into
// the query string instead — see splitPackageRef.
func TestGetPackageAcceptsAScopedReferenceInThePath(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackageIn("cfx-5000-k8s", "orb_23.8.1076", digestA)
	h.seedPackageIn("cfx-5000-db", "orb_23.8.1076", digestB)

	var pkg v1.Package
	code := h.get("/api/v1/products/vendor-a/packages/cfx-5000-db:orb_23.8.1076", &pkg)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for a scoped reference, got %d", code)
	}
	if pkg.ManifestDigest != digestB {
		t.Errorf("resolved to %s, want the cfx-5000-db package", pkg.ManifestDigest)
	}
}

// The query parameter is the other way to scope, for a caller that has the
// repository in a variable rather than in the string it was handed.
func TestGetPackageAcceptsARepositoryQuery(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackageIn("orbs/cfx-5000-k8s", "orb_23.8.1076", digestA)
	h.seedPackageIn("orbs/cfx-5000-db", "orb_23.8.1076", digestB)

	var pkg v1.Package
	code := h.get(
		"/api/v1/products/vendor-a/packages/orb_23.8.1076?repository=orbs/cfx-5000-k8s", &pkg)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if pkg.ManifestDigest != digestA {
		t.Errorf("resolved to %s, want the cfx-5000-k8s package", pkg.ManifestDigest)
	}
}

// The fleet-wide scan. It must never block and must never fail the whole call
// because one product could not be started.
func TestDiscoverAllReportsEveryProduct(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.products = []string{"vendor-a"}

	var resp v1.DiscoverAllResponse
	if code := h.post("/api/v1/products:discover", "", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(resp.Products) != 1 || resp.Products[0].Product != "vendor-a" {
		t.Fatalf("expected one product row for vendor-a, got %+v", resp.Products)
	}
	if resp.Started != 1 {
		t.Errorf("started = %d, want 1", resp.Started)
	}
	// Started, not waited for: a fleet-wide scan that blocked would hold an HTTP
	// request open for as long as the slowest vendor takes.
	if h.discoverer.started != 1 {
		t.Errorf("the loop was asked to start %d scans, want 1", h.discoverer.started)
	}
}

func TestDiscoverAllWithNoProducts(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.products = nil

	var resp v1.DiscoverAllResponse
	if code := h.post("/api/v1/products:discover", "", &resp); code != http.StatusOK {
		t.Fatalf("nothing to scan is not an error, got %d", code)
	}
	if len(resp.Products) != 0 || resp.Started != 0 {
		t.Errorf("expected an empty result, got %+v", resp)
	}
}

// A comparison that outlives its deadline says so, in terms a caller can act
// on.
//
// The failure mode this exists for is not an error: a comparison of a real
// release is hundreds of manifests read live from two registries, and when
// those registries are slow the request simply does not come back. A caller
// cannot tell that from a hang, and there is no progress channel to tell them
// otherwise — so it stops, and the reply names the limit and the one knob that
// makes the work smaller.
func TestCompareThatRunsOutOfTimeSaysSo(t *testing.T) {
	// Shortened for the test. The constant is minutes, because the work is
	// real; what is being tested is the shape of the answer when it runs out.
	original := compareDeadline
	compareDeadline = 50 * time.Millisecond
	t.Cleanup(func() { compareDeadline = original })

	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Comparer = comparerFunc(func(
			ctx context.Context, _ string, _, _ ComparePoint, _ int64, _ compare.ProgressFunc,
		) (compare.Report, error) {
			<-ctx.Done()
			return compare.Report{}, ctx.Err()
		})
	})
	h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("a", 64))

	var problem v1.Problem
	status := h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:compare", "{}", &problem)

	if status == http.StatusOK {
		t.Fatalf("a comparison that never returned answered 200")
	}
	if problem.Code != v1.CodeDeadlineExceeded {
		t.Errorf("code = %q, want %q", problem.Code, v1.CodeDeadlineExceeded)
	}
	if !strings.Contains(problem.Detail, "fileBudgetBytes") {
		t.Errorf("the refusal does not say what to do about it: %s", problem.Detail)
	}
}

// comparerFunc adapts a function to the Comparer interface.
type comparerFunc func(
	context.Context, string, ComparePoint, ComparePoint, int64, compare.ProgressFunc,
) (compare.Report, error)

func (f comparerFunc) Compare(
	ctx context.Context, product string, a, b ComparePoint, budget int64,
	progress compare.ProgressFunc,
) (compare.Report, error) {
	return f(ctx, product, a, b, budget, progress)
}

// A comparison reports where it has got to, while it is still getting there —
// and gives up when it stops getting anywhere.
//
// Both halves in one test because they are one mechanism: the report is the
// response, so there is nothing to hand an id back in; the caller mints a
// token, sends it, and polls a second endpoint. That side channel is what makes
// a four-minute request distinguishable from a hung one, and it is also what
// the server itself reads to tell slow from stopped.
func TestAComparisonReportsProgressAndGivesUpWhenItStops(t *testing.T) {
	// Shortened for the test. In production this is minutes: a large release
	// read over a slow link advances continuously and never approaches it.
	stallWas := compareStall
	compareStall = 2 * time.Second
	t.Cleanup(func() { compareStall = stallWas })

	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Comparer = comparerFunc(func(
			ctx context.Context, _ string, _, _ ComparePoint, _ int64, progress compare.ProgressFunc,
		) (compare.Report, error) {
			progress(compare.Progress{
				Key: "a", Side: "vendor", Phase: compare.PhaseManifests,
				Done: 143, Total: 261, Estimated: true,
			})
			progress(compare.Progress{
				Key: "b", Side: "internal", Phase: compare.PhaseNames, Done: 12, Total: 261,
			})
			// And then nothing more: a registry that has stopped answering.
			<-ctx.Done()
			return compare.Report{}, ctx.Err()
		})
	})
	h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("a", 64))

	var (
		problem v1.Problem
		status  int
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		status = h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:compare",
			`{"progressToken":"tok-1"}`, &problem)
	}()

	var progress v1.CompareProgressResponse
	deadline := time.Now().Add(5 * time.Second)
	for {
		if h.get("/api/v1/comparisons/tok-1", &progress) == http.StatusOK &&
			len(progress.Sides) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the comparison never reported any progress")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Sorted by END, so a bar built from this does not reorder between polls —
	// and keyed by it, because the two sides of a version comparison are the
	// same place and share a label.
	if progress.Sides[0].Key != "a" || progress.Sides[1].Key != "b" {
		t.Errorf("sides came out %s, %s — want a stable order",
			progress.Sides[0].Key, progress.Sides[1].Key)
	}
	if vendor := progress.Sides[0]; vendor.Done != 143 || vendor.Total != 261 || !vendor.Estimated {
		t.Errorf("vendor progress = %+v, want 143 of 261 and estimated", vendor)
	}

	<-done
	if status == http.StatusOK {
		t.Fatal("a comparison that stopped advancing answered 200")
	}
	// Named as a stall, not as a timeout. They are different diagnoses and send
	// a reader to different places: one says the release is large, the other
	// says a registry went silent.
	if !strings.Contains(problem.Detail, "stopped making progress") {
		t.Errorf("the refusal does not say it stalled: %s", problem.Detail)
	}
}

// A poll for a comparison nobody is running is a 404, and that is an ANSWER:
// progress lives in the memory of the replica running it, so a caller that
// misses gets an honest "no position" rather than a fabricated one.
func TestProgressForAnUnknownComparisonIsNotFound(t *testing.T) {
	h := newAPIHarness(t)

	var problem v1.Problem
	if status := h.get("/api/v1/comparisons/nobody-asked", &problem); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}
