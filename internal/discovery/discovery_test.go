package discovery

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// harness wires a fake registry, a migrated SQLite store, a reconciled catalog
// and a Scanner — the same path the Coordinator takes, minus the leader gate.
type harness struct {
	t        *testing.T
	reg      *fakeregistry.Registry
	store    store.Store
	packages *store.Packages
	scanner  *Scanner
	product  *product.Product
	repoID   int64
}

const testRepoPath = "vendor-a/platform"

func newHarness(t *testing.T, doc string) *harness {
	t.Helper()
	ctx := t.Context()

	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	s, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "discovery.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := store.Migrate(ctx, s, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The registry host is only known once the fake server is listening, so the
	// document carries a placeholder.
	doc = strings.ReplaceAll(doc, "REGISTRY_HOST", reg.Host())

	p := loadProduct(t, doc)

	cat := catalog.NewCatalog(s)
	res, err := cat.Reconcile(ctx, []*product.Product{p})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ref := res.Products[p.Metadata.Name]

	packages := store.NewPackages(s)

	client, err := generic.New(generic.Config{
		Registry:   reg.Host(),
		Repository: testRepoPath,
		PlainHTTP:  true,
		// A fast retry policy: a test must not spend the production backoff
		// schedule proving that a persistent failure eventually gives up.
		Transport: transport.Config{
			MaxRetryAttempts: 3,
			RetryBaseDelay:   time.Millisecond,
			RetryMaxDelay:    5 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	scanner, err := NewScanner(ScannerConfig{
		Packages:   packages,
		Product:    p,
		ProductID:  ref.ID,
		SourceName: "vendor",
		NewClient:  func(string) (registry.Source, error) { return client, nil },
		RepoIDs:    ref.Repositories,
	})
	if err != nil {
		t.Fatalf("build scanner: %v", err)
	}

	return &harness{
		t: t, reg: reg, store: s, packages: packages,
		scanner: scanner, product: p, repoID: ref.Repositories["vendor"],
	}
}

func loadProduct(t *testing.T, doc string) *product.Product {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "product.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := t.TempDir()

	loader := product.NewLoader(dir, product.NewSecretResolver(secretsDir))
	res, err := loader.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(res.Invalid) > 0 {
		t.Fatalf("config invalid: %v", res.Invalid[0].Err)
	}
	if len(res.Valid) != 1 {
		t.Fatalf("expected 1 product, got %d", len(res.Valid))
	}
	return res.Valid[0]
}

func (h *harness) scan() ScanResult {
	h.t.Helper()
	res, err := h.scanner.Scan(h.t.Context())
	if err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	for _, te := range res.TagErrors {
		h.t.Errorf("unexpected tag error: %v", te)
	}
	return res
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// baseDoc is a minimal product with one source and one target.
const baseDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      repository: vendor-a/platform
      anonymous: true
      discovery:
        enabled: true
        interval: 15m
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repository: mirror/vendor-a
      anonymous: true
      default: true
`

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestScanRecordsDiscoveredPackages(t *testing.T) {
	h := newHarness(t, baseDoc)

	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("layer-a"))
	h.reg.AddImage(testRepoPath, "v1.1.0", fakeregistry.NewLayer("layer-b"))

	res := h.scan()

	if res.TagsListed != 2 || res.New != 2 {
		t.Fatalf("expected 2 tags listed and 2 new, got listed=%d new=%d", res.TagsListed, res.New)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages`); got != 2 {
		t.Errorf("expected 2 package rows, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE state = 'discovered'`); got != 2 {
		t.Errorf("expected both packages discovered, got %d", got)
	}

	// Each image has one config blob and one layer.
	if got := h.count(`SELECT COUNT(*) FROM blobs`); got != 4 {
		t.Errorf("expected 4 distinct blobs, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts`); got != 2 {
		t.Errorf("expected 2 artifacts, got %d", got)
	}
}

// A re-scan must find nothing new. This is the property that makes a full scan
// every fifteen minutes forever a sane design rather than a duplicate factory.
func TestRescanIsIdempotent(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("layer-a"))

	first := h.scan()
	if first.New != 1 {
		t.Fatalf("first scan: expected 1 new, got %d", first.New)
	}

	for i := range 3 {
		res := h.scan()
		if res.New != 0 {
			t.Fatalf("re-scan %d: expected 0 new, got %d", i+1, res.New)
		}
	}

	if got := h.count(`SELECT COUNT(*) FROM packages`); got != 1 {
		t.Errorf("expected 1 package row after 4 scans, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts`); got != 1 {
		t.Errorf("expected 1 artifact row after 4 scans, got %d", got)
	}
}

// A re-scan must not fetch manifest bodies for packages already recorded.
// Without this the "cheap steady state" claim in docs/design/07 §2 is false.
func TestRescanDoesNotRefetchManifests(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("layer-a"))

	h.scan()
	after := h.reg.ManifestCalls.Load()

	h.scan()
	delta := h.reg.ManifestCalls.Load() - after

	// One HEAD per tag to resolve it, and nothing more. A GET would show up as
	// an extra call here.
	if delta != 1 {
		t.Errorf("expected 1 manifest request on re-scan (the HEAD), got %d", delta)
	}
}

func TestSupersessionOnRepushedTag(t *testing.T) {
	h := newHarness(t, baseDoc)

	first := h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("original"))
	h.scan()

	// Same tag, different content: this is the one situation supersession
	// covers.
	second := h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("replaced"))
	if first == second {
		t.Fatal("re-push produced the same digest; the test proves nothing")
	}

	res := h.scan()
	if res.New != 1 {
		t.Fatalf("expected the re-push to be 1 new package, got %d", res.New)
	}
	if res.Superseded != 1 {
		t.Fatalf("expected 1 superseded package, got %d", res.Superseded)
	}

	// Two rows: the history is preserved, not overwritten.
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE tag = 'v1.0.0'`); got != 2 {
		t.Errorf("expected 2 rows for the re-pushed tag, got %d", got)
	}

	var state string
	var supersededBy sql.NullInt64
	err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT state, superseded_by FROM packages WHERE manifest_digest = ?`, first,
	).Scan(&state, &supersededBy)
	if err != nil {
		t.Fatalf("read old package: %v", err)
	}
	if state != "superseded" {
		t.Errorf("expected old package superseded, got %q", state)
	}
	if !supersededBy.Valid {
		t.Error("expected superseded_by to point at the new package")
	}

	if got := h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'PackageSuperseded'`); got != 1 {
		t.Errorf("expected 1 PackageSuperseded audit event, got %d", got)
	}
}

// The property the design doc stresses hardest: different tags are independent
// software versions and NEVER supersede each other.
func TestDifferentTagsNeverSupersede(t *testing.T) {
	h := newHarness(t, baseDoc)

	h.reg.AddImage(testRepoPath, "v2.13.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage(testRepoPath, "v2.14.0", fakeregistry.NewLayer("b"))
	h.reg.AddImage(testRepoPath, "v2.14.1", fakeregistry.NewLayer("c"))

	res := h.scan()
	if res.New != 3 {
		t.Fatalf("expected 3 new packages, got %d", res.New)
	}
	if res.Superseded != 0 {
		t.Fatalf("expected 0 superseded, got %d", res.Superseded)
	}

	if got := h.count(`SELECT COUNT(*) FROM packages WHERE state = 'superseded'`); got != 0 {
		t.Errorf("expected no superseded packages, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE state = 'discovered'`); got != 3 {
		t.Errorf("expected 3 coexisting discovered packages, got %d", got)
	}

	// And adding a fourth version leaves the first three alone.
	h.reg.AddImage(testRepoPath, "v2.15.0", fakeregistry.NewLayer("d"))
	h.scan()

	if got := h.count(`SELECT COUNT(*) FROM packages WHERE state = 'discovered'`); got != 4 {
		t.Errorf("expected 4 coexisting packages after the new release, got %d", got)
	}
}

func TestIndexChildrenAreListedNotFetched(t *testing.T) {
	h := newHarness(t, baseDoc)

	shared := fakeregistry.NewLayer("shared-base-layer")
	amd := h.reg.AddImage(testRepoPath, "", shared, fakeregistry.NewLayer("amd64-layer"))
	arm := h.reg.AddImage(testRepoPath, "", shared, fakeregistry.NewLayer("arm64-layer"))
	h.reg.AddIndex(testRepoPath, "v3.0.0",
		[]string{amd, arm}, []string{"linux/amd64", "linux/arm64"})

	res := h.scan()
	if res.New != 1 {
		t.Fatalf("expected 1 new package, got %d", res.New)
	}

	// The index plus the two children it lists — recorded WITHOUT a request
	// each, from the descriptors the index already carries.
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts`); got != 3 {
		t.Errorf("expected 3 artifacts (index + the 2 it lists), got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE depth = 1`); got != 2 {
		t.Errorf("expected 2 child artifacts at depth 1, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE parent_id IS NOT NULL`); got != 2 {
		t.Errorf("expected 2 artifacts with a parent, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE platform = 'linux/amd64'`); got != 1 {
		t.Errorf("expected the amd64 platform recorded, got %d", got)
	}

	// Exactly one manifest was fetched: the tag's own. The children have the
	// vendor's word for their digests, and the record says so rather than
	// implying we verified bytes we never saw.
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE raw IS NOT NULL`); got != 1 {
		t.Errorf("expected 1 fetched artifact, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE raw IS NULL`); got != 2 {
		t.Errorf("expected 2 listed-but-unfetched artifacts, got %d", got)
	}

	// No blobs, and no claimed size: the layers live inside child manifests
	// nobody fetched. Recording zero would be a number an operator would trust.
	if got := h.count(`SELECT COUNT(*) FROM blobs`); got != 0 {
		t.Errorf("expected no blobs for an index package, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE blob_count IS NULL AND total_bytes IS NULL`); got != 1 {
		t.Errorf("expected the package's size to be recorded as unknown, got %d rows", got)
	}
}

// A package whose root is a plain image manifest IS fully measured from that
// one fetch: its config and layer descriptors are inside the bytes we hold.
func TestSingleManifestPackageIsFullyMeasured(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0",
		fakeregistry.NewLayer("a"), fakeregistry.NewLayer("b"))

	if res := h.scan(); res.New != 1 {
		t.Fatalf("expected 1 new package, got %d", res.New)
	}

	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE raw IS NOT NULL`); got != 1 {
		t.Errorf("expected the manifest to be fetched, got %d", got)
	}
	// One config plus two layers.
	if got := h.count(`SELECT COUNT(*) FROM blobs`); got != 3 {
		t.Errorf("expected 3 blobs, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE blob_count = 3 AND total_bytes > 0`); got != 1 {
		t.Errorf("expected a measured size, got %d rows", got)
	}
}

func TestTagFiltersAreAppliedBeforeResolve(t *testing.T) {
	doc := strings.Replace(baseDoc, `        interval: 15m`, `        interval: 15m
        tagFilters:
          include: ["^v[0-9]+\\.[0-9]+\\.[0-9]+$"]
          exclude: ["-rc"]`, 1)

	h := newHarness(t, doc)

	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage(testRepoPath, "v1.1.0", fakeregistry.NewLayer("b"))
	h.reg.AddImage(testRepoPath, "latest", fakeregistry.NewLayer("c"))
	h.reg.AddImage(testRepoPath, "nightly-2026-01-01", fakeregistry.NewLayer("d"))

	before := h.reg.ManifestCalls.Load()
	res := h.scan()

	if res.TagsListed != 4 {
		t.Fatalf("expected 4 tags listed, got %d", res.TagsListed)
	}
	if res.TagsAdmitted != 2 {
		t.Fatalf("expected 2 tags admitted, got %d", res.TagsAdmitted)
	}
	if res.New != 2 {
		t.Fatalf("expected 2 new packages, got %d", res.New)
	}

	// Filtering must bound SCAN COST, not just what is stored: a filtered tag
	// costs no request at all. Two admitted tags means two HEADs and two GETs.
	if calls := h.reg.ManifestCalls.Load() - before; calls != 4 {
		t.Errorf("expected 4 manifest requests for 2 admitted tags, got %d", calls)
	}
}

func TestAutoDownloadRuleFiresExactlyOnce(t *testing.T) {
	doc := baseDoc + `
  autoDownload:
    enabled: true
    rules:
      - name: releases
        tagPattern: "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
        targets: ["internal"]
        priority: 10
      - name: catch-all
        tagPattern: ".*"
        priority: 90
`
	h := newHarness(t, doc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	res := h.scan()
	if res.Requests != 1 {
		t.Fatalf("expected 1 transfer request, got %d", res.Requests)
	}

	// First match wins: the release rule, not the catch-all.
	var ruleName string
	var priority int
	err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT auto_rule_name, priority FROM transfer_requests`).Scan(&ruleName, &priority)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if ruleName != "releases" {
		t.Errorf("expected the first matching rule to win, got %q", ruleName)
	}
	if priority != 10 {
		t.Errorf("expected priority 10 from the matched rule, got %d", priority)
	}

	// Re-scanning must not create a second request. This is the case that
	// matters most: an auto-download rule is the one path that creates tens of
	// gigabytes of work with no human in the loop.
	for range 3 {
		h.scan()
	}
	if got := h.count(`SELECT COUNT(*) FROM transfer_requests`); got != 1 {
		t.Errorf("expected exactly 1 transfer request after 4 scans, got %d", got)
	}
}

func TestNoRulesMeansNoRequests(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	res := h.scan()
	if res.New != 1 {
		t.Fatalf("expected the package to be discovered, got %d new", res.New)
	}
	if res.Requests != 0 {
		t.Errorf("expected no transfer requests without autoDownload, got %d", res.Requests)
	}
}

// A rule that matches nothing must not create work, and must not error.
func TestRuleThatMatchesNothing(t *testing.T) {
	doc := baseDoc + `
  autoDownload:
    enabled: true
    rules:
      - name: only-lts
        tagPattern: "-lts$"
        targets: ["internal"]
`
	h := newHarness(t, doc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	res := h.scan()
	if res.New != 1 || res.Requests != 0 {
		t.Errorf("expected 1 discovery and 0 requests, got new=%d requests=%d", res.New, res.Requests)
	}
}

// autoDownload.enabled: false must disable the rules even when they are
// present — otherwise turning the feature off would require deleting config.
func TestDisabledAutoDownloadCreatesNoRequests(t *testing.T) {
	doc := baseDoc + `
  autoDownload:
    enabled: false
    rules:
      - name: releases
        tagPattern: ".*"
        targets: ["internal"]
`
	h := newHarness(t, doc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	if res := h.scan(); res.Requests != 0 {
		t.Errorf("expected no requests when autoDownload is disabled, got %d", res.Requests)
	}
}

func TestNotificationsAreEnqueuedInTheSameTransaction(t *testing.T) {
	doc := baseDoc + `
  notifications:
    enabled: true
    channels:
      - name: platform-team
        type: email
        email:
          recipients: ["platform@example.com"]
    subscriptions:
      - events: ["PackageDiscovered"]
        channels: ["platform-team"]
`
	h := newHarness(t, doc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))
	h.scan()

	if got := h.count(`SELECT COUNT(*) FROM notifications WHERE event_type = 'PackageDiscovered'`); got != 1 {
		t.Fatalf("expected 1 queued notification, got %d", got)
	}

	// Re-scanning must not re-notify.
	h.scan()
	if got := h.count(`SELECT COUNT(*) FROM notifications`); got != 1 {
		t.Errorf("expected 1 notification after 2 scans, got %d", got)
	}
}

func TestAuditEventIsWritten(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))
	h.scan()

	var detail string
	err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT detail FROM audit_events WHERE event_type = 'PackageDiscovered'`).Scan(&detail)
	if err != nil {
		t.Fatalf("read audit event: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(detail), &parsed); err != nil {
		t.Fatalf("audit detail is not JSON: %v", err)
	}
	if parsed["tag"] != "v1.0.0" {
		t.Errorf("expected the tag in the audit detail, got %v", parsed["tag"])
	}
}

// A tag whose manifest is unreadable must not stop the scan. One bad artifact
// stalling every release behind it is the failure mode this guards.
func TestMalformedManifestDoesNotStopTheScan(t *testing.T) {
	h := newHarness(t, baseDoc)

	h.reg.AddManifest(testRepoPath, "broken", []byte("this is not json"),
		registry.MediaTypeOCIManifest)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage(testRepoPath, "v1.1.0", fakeregistry.NewLayer("b"))

	res, err := h.scanner.Scan(t.Context())
	if err != nil {
		t.Fatalf("a bad tag must not fail the scan: %v", err)
	}

	if len(res.TagErrors) != 1 {
		t.Fatalf("expected 1 tag error, got %d: %v", len(res.TagErrors), res.TagErrors)
	}
	if res.TagErrors[0].Tag != "broken" {
		t.Errorf("expected the broken tag to be named, got %q", res.TagErrors[0].Tag)
	}
	if res.New != 2 {
		t.Errorf("expected the other 2 tags to be discovered anyway, got %d", res.New)
	}
}

// A package must be recorded whole or not at all: the manifest tree is fetched
// before the transaction opens, so a mid-tree failure leaves no partial row.
func TestFailedManifestFetchLeavesNoPartialPackage(t *testing.T) {
	h := newHarness(t, baseDoc)

	amd := h.reg.AddImage(testRepoPath, "", fakeregistry.NewLayer("amd"))
	arm := h.reg.AddImage(testRepoPath, "", fakeregistry.NewLayer("arm"))
	idx := h.reg.AddIndex(testRepoPath, "v1.0.0",
		[]string{amd, arm}, []string{"linux/amd64", "linux/arm64"})

	// Fail every attempt at the tag's OWN manifest. Children are no longer
	// fetched, so this is the only manifest request a scan makes and the only
	// one that can leave a half-written package behind.
	h.reg.FailNext(idx, 500, 100)

	res, err := h.scanner.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.TagErrors) != 1 {
		t.Fatalf("expected the tag to fail, got %d errors", len(res.TagErrors))
	}
	if got := h.count(`SELECT COUNT(*) FROM packages`); got != 0 {
		t.Errorf("expected no package row after a failed tree fetch, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts`); got != 0 {
		t.Errorf("expected no artifact rows after a failed tree fetch, got %d", got)
	}

	// And once the registry recovers, the next full scan completes it. This is
	// the self-healing property: no repair path, just another scan.
	h.reg.ClearFaults()
	res = h.scan()
	if res.New != 1 {
		t.Fatalf("expected recovery to record the package, got %d new", res.New)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts`); got != 3 {
		t.Errorf("expected the full tree after recovery, got %d artifacts", got)
	}
}

// A registry that cannot serve the tag list fails the scan, but the source
// stays configured and the next scan works.
func TestUnreachableRegistryFailsTheScanNotTheSource(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	h.reg.FailNext("tags/list", 503, 100)

	if _, err := h.scanner.Scan(t.Context()); err == nil {
		t.Fatal("expected the scan to fail while the registry is down")
	}

	// Nothing was disabled: the same scanner works once the registry recovers.
	h.reg.ClearFaults()
	if res := h.scan(); res.New != 1 {
		t.Errorf("expected recovery after the outage, got %d new", res.New)
	}
}

// A 429 with Retry-After must be honoured and the scan must still complete.
func TestRateLimitedRegistryIsRetried(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	h.reg.FailNextWithRetryAfter("tags/list", 429, 1, "0")

	res := h.scan()
	if res.New != 1 {
		t.Errorf("expected the scan to succeed after backing off, got %d new", res.New)
	}
}

// A tag deleted between the list and the resolve is not an error.
func TestTagDeletedMidScanIsNotAnError(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddManifest(testRepoPath, "ghost", []byte(`{"schemaVersion":2}`), registry.MediaTypeOCIManifest)
	h.reg.RemoveTag(testRepoPath, "ghost")

	res, err := h.scanner.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.TagErrors) != 0 {
		t.Errorf("a vanished tag must not be an error, got %v", res.TagErrors)
	}
	if res.New != 1 {
		t.Errorf("expected the surviving tag to be discovered, got %d", res.New)
	}
}

func TestPaginatedTagListIsFullyWalked(t *testing.T) {
	reg := fakeregistry.New(fakeregistry.WithPageSize(3))
	t.Cleanup(reg.Close)

	// Rebuild the harness against this paginated registry.
	h := newHarnessWith(t, baseDoc, reg)

	for _, tag := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0", "v1.4.0", "v1.5.0", "v1.6.0"} {
		reg.AddImage(testRepoPath, tag, fakeregistry.NewLayer(tag))
	}

	res := h.scan()
	if res.TagsListed != 7 {
		t.Errorf("expected all 7 tags across pages, got %d", res.TagsListed)
	}
	if res.New != 7 {
		t.Errorf("expected 7 packages, got %d", res.New)
	}
	if reg.TagListCalls.Load() < 3 {
		t.Errorf("expected the tag list to be paginated, got %d calls", reg.TagListCalls.Load())
	}
}

// newHarnessWith is newHarness against a caller-supplied registry.
func newHarnessWith(t *testing.T, doc string, reg *fakeregistry.Registry) *harness {
	t.Helper()
	ctx := t.Context()

	s, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "discovery.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := store.Migrate(ctx, s, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	p := loadProduct(t, strings.ReplaceAll(doc, "REGISTRY_HOST", reg.Host()))

	res, err := catalog.NewCatalog(s).Reconcile(ctx, []*product.Product{p})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ref := res.Products[p.Metadata.Name]

	client, err := generic.New(generic.Config{
		Registry: reg.Host(), Repository: testRepoPath, PlainHTTP: true,
		Transport: transport.Config{
			MaxRetryAttempts: 3, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 5 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	packages := store.NewPackages(s)
	scanner, err := NewScanner(ScannerConfig{
		Packages: packages, Product: p, ProductID: ref.ID, SourceName: "vendor",
		NewClient: func(string) (registry.Source, error) { return client, nil },
		RepoIDs:   ref.Repositories,
	})
	if err != nil {
		t.Fatalf("build scanner: %v", err)
	}

	return &harness{t: t, reg: reg, store: s, packages: packages, scanner: scanner, product: p}
}

// ---------------------------------------------------------------------------
// Unit tests that need no registry
// ---------------------------------------------------------------------------

func TestTagFilterSemantics(t *testing.T) {
	tests := []struct {
		name    string
		filters product.TagFilters
		tag     string
		admit   bool
	}{
		{"no filters admits everything", product.TagFilters{}, "anything", true},
		{"include matches", product.TagFilters{Include: []string{`^v\d`}}, "v1.0.0", true},
		{"include does not match", product.TagFilters{Include: []string{`^v\d`}}, "latest", false},
		{"exclude wins over include",
			product.TagFilters{Include: []string{`.*`}, Exclude: []string{`-rc`}}, "v1.0.0-rc1", false},
		{"exclude alone", product.TagFilters{Exclude: []string{`^nightly`}}, "nightly-1", false},
		{"exclude alone admits others", product.TagFilters{Exclude: []string{`^nightly`}}, "v1.0.0", true},
		{"any include matching is enough",
			product.TagFilters{Include: []string{`^a`, `^b`}}, "beta", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := compileFilters("tagFilters", tc.filters)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if got := f.admits(tc.tag); got != tc.admit {
				t.Errorf("admits(%q) = %v, want %v", tc.tag, got, tc.admit)
			}
		})
	}
}

func TestInvalidPatternIsRejectedAtCompileTime(t *testing.T) {
	if _, err := compileFilters("tagFilters", product.TagFilters{Include: []string{"("}}); err == nil {
		t.Error("expected an unclosed group to be rejected")
	}
	if _, err := compileRules(productWithRules(
		product.Rule{Name: "bad", TagPattern: "["},
	)); err == nil {
		t.Error("expected an invalid rule pattern to be rejected")
	}
}

// The key must not change when targets are listed in a different order — a
// cosmetic YAML reorder must not duplicate every pending request.
func TestFirstMatchWins(t *testing.T) {
	set, err := compileRules(productWithRules(
		product.Rule{Name: "rc", TagPattern: `-rc\d+$`},
		product.Rule{Name: "release", TagPattern: `^v\d+\.\d+\.\d+$`},
		product.Rule{Name: "everything", TagPattern: `.*`},
	))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for tag, want := range map[string]string{
		"v1.0.0-rc1": "rc",
		"v1.0.0":     "release",
		"latest":     "everything",
	} {
		got, ok := set.match(tag)
		if !ok {
			t.Fatalf("%q matched no rule", tag)
		}
		if got.Name != want {
			t.Errorf("%q matched %q, want %q", tag, got.Name, want)
		}
	}
}

func TestPlainHTTPIsLoopbackOnly(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost:5000":         true,
		"127.0.0.1:5000":         true,
		"localhost":              true,
		"registry.example.com":   false,
		"myregistry.azurecr.io":  false,
		"notlocalhost.com:5000":  false,
		"localhost.example.com":  false,
		"192.168.1.10:5000":      false,
		"registry.internal:5000": false,
	} {
		if got := isPlainHTTP(host); got != want {
			t.Errorf("isPlainHTTP(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSubscribedChannelsDeduplicates(t *testing.T) {
	n := product.Notifications{
		Subscriptions: []product.Subscription{
			{Events: []string{"PackageDiscovered"}, Channels: []string{"a", "b"}},
			{Events: []string{"PackageDiscovered", "TransferFailed"}, Channels: []string{"b", "c"}},
			{Events: []string{"TransferFailed"}, Channels: []string{"d"}},
		},
	}

	got := subscribedChannels(n, "PackageDiscovered")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A scanner must refuse to start when the source it names does not exist,
// rather than polling nothing and reporting health.
func TestScannerRejectsUnknownSource(t *testing.T) {
	p := loadProduct(t, strings.ReplaceAll(baseDoc, "REGISTRY_HOST", "registry.example.com"))

	_, err := NewScanner(ScannerConfig{
		Product:    p,
		SourceName: "does-not-exist",
	})
	if err == nil {
		t.Fatal("expected an unknown source name to be rejected")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing source, got: %v", err)
	}
}

// productWithRules wraps rules in the minimum product compileRules now takes.
//
// It takes a product rather than the block because a rule's spelling —
// `download` or the older `autoDownload` — is a property of the document, and
// the compiler reads whichever one is in use.
func productWithRules(rules ...product.Rule) *product.Product {
	return &product.Product{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec:     product.Spec{Download: product.Download{Rules: rules}},
	}
}
