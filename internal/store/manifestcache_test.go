package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// cacheHarness is a migrated store with one product and one repository, so a
// test can seed packages without going near discovery.
type cacheHarness struct {
	t         *testing.T
	packages  *Packages
	productID int64
	repoID    int64
	// n makes each seeded digest distinct.
	n int
}

func newCacheHarness(t *testing.T) *cacheHarness {
	t.Helper()
	st := openTestStore(t)
	p := NewPackages(st)

	ctx := t.Context()
	var productID int64
	err := st.DB().QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config)
		 VALUES ('vendor-a', 'hash', '{}') RETURNING id`).Scan(&productID)
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	repoID, err := p.EnsureRepository(ctx, tx, productID, "source", "vendor",
		"registry.example.com", "orbs/cfx-5000-k8s", "generic", "config", "cfx-5000-k8s")
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	return &cacheHarness{t: t, packages: p, productID: productID, repoID: repoID}
}

// seed writes a package whose tree has been fully walked, with `bodies`
// manifests each carrying `size` bytes of cached body.
func (h *cacheHarness) seed(tag, digest string, bodies int, size int) int64 {
	h.t.Helper()
	ctx := h.t.Context()

	tx, err := h.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertPackage(ctx, tx, PackageRow{
		ProductID: h.productID, SourceRepoID: h.repoID, Tag: tag,
		ManifestDigest: digest, MediaType: "application/vnd.oci.image.index.v1+json",
		DisplayTag: strings.TrimPrefix(tag, "orb_"),
	})
	if err != nil {
		h.t.Fatalf("insert package: %v", err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}

	tree := ExpandedTree{}
	total := int64(0)
	for i := range bodies {
		raw := []byte(strings.Repeat("x", size))
		tree.Artifacts = append(tree.Artifacts, ExpandedArtifact{
			Row: ArtifactRow{
				Digest:    digestOf(digest, i),
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				SizeBytes: int64(size),
				Depth:     min(i, 1),
				Raw:       raw,
			},
			Parent: parentOf(i),
		})
		total += int64(size)
	}
	count := 0
	tree.TotalBytes, tree.BlobCount = &total, &count

	if err := h.packages.RecordExpandedTree(ctx, id, tree); err != nil {
		h.t.Fatalf("record tree: %v", err)
	}
	return id
}

func digestOf(base string, i int) string {
	// A distinct, well-formed digest per artifact; the content is irrelevant to
	// what these tests measure.
	return base[:len(base)-2] + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
}

func parentOf(i int) int {
	if i == 0 {
		return -1
	}
	return 0
}

// The headline property of the whole design: a package whose manifest bodies
// were reclaimed is STILL FULLY KNOWN. If this stops holding, eviction buys
// back a full registry walk and the cache can never actually be reclaimed.
func TestSweepingLeavesThePackageFullyDescribed(t *testing.T) {
	h := newCacheHarness(t)
	id := h.seed("orb_23.8.1076", strings.Repeat("1", 64), 4, 1000)
	ctx := t.Context()

	before, complete, err := h.packages.ReadExpandedTree(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("a freshly recorded tree should be complete")
	}

	res, err := h.packages.SweepManifestCache(ctx, ManifestCachePolicy{BudgetBytes: 1})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.EvictedManifests == 0 {
		t.Fatal("sweep with a 1-byte budget reclaimed nothing")
	}

	after, stillComplete, err := h.packages.ReadExpandedTree(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !stillComplete {
		t.Error("the tree stopped being complete after its bodies were reclaimed; " +
			"completeness must follow fetched_at, not the cached bytes")
	}
	if len(after.Artifacts) != len(before.Artifacts) {
		t.Errorf("artifacts %d -> %d: sweeping must not remove rows",
			len(before.Artifacts), len(after.Artifacts))
	}
	if before.TotalBytes == nil || after.TotalBytes == nil || *after.TotalBytes != *before.TotalBytes {
		t.Errorf("measured size changed across a sweep: %v -> %v", before.TotalBytes, after.TotalBytes)
	}
	for _, a := range after.Artifacts {
		if !a.Row.Fetched {
			t.Errorf("artifact %s lost its fetched mark", a.Row.Digest)
		}
		if a.Row.Cached {
			t.Errorf("artifact %s still holds bytes after a 1-byte budget sweep", a.Row.Digest)
		}
	}
}

// The budget is a ceiling, not a target: a cache already inside it is left
// entirely alone.
func TestSweepLeavesACacheInsideItsBudget(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 3, 100)
	ctx := t.Context()

	res, err := h.packages.SweepManifestCache(ctx, ManifestCachePolicy{BudgetBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifests() != 0 {
		t.Errorf("reclaimed %d manifest(s) while inside the budget", res.Manifests())
	}

	stats, err := h.packages.ManifestCacheStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CachedManifests != 3 {
		t.Errorf("cached manifests = %d, want 3", stats.CachedManifests)
	}
	if stats.CachedBytes != 300 {
		t.Errorf("cached bytes = %d, want 300", stats.CachedBytes)
	}
}

// The budget evicts LEAST RECENTLY USED, which is what makes it track the
// working set rather than arbitrary rows. A package a transfer just touched
// must outlive one nobody has looked at.
func TestBudgetEvictsTheLeastRecentlyUsedFirst(t *testing.T) {
	h := newCacheHarness(t)
	ctx := t.Context()

	old := h.seed("orb_23.8.1000", strings.Repeat("1", 64), 2, 500)
	recent := h.seed("orb_23.8.2000", strings.Repeat("2", 64), 2, 500)

	// Age the first package's stamps rather than sleeping: the LRU order is the
	// property under test, not the clock.
	h.backdate(old, 48*time.Hour)

	// Room for one package's worth of bodies.
	if _, err := h.packages.SweepManifestCache(ctx,
		ManifestCachePolicy{BudgetBytes: 1000}); err != nil {
		t.Fatal(err)
	}

	if got := h.cachedIn(old); got != 0 {
		t.Errorf("the older package still holds %d bodies; it should have gone first", got)
	}
	if got := h.cachedIn(recent); got != 2 {
		t.Errorf("the recently used package holds %d bodies, want 2", got)
	}
}

// TTL is the other bound, and it bites whatever the budget says: a deployment
// that discovers far more than it transfers would otherwise carry a full budget
// of manifests nobody will ever push.
func TestExpiryDropsBodiesNobodyHasTouched(t *testing.T) {
	h := newCacheHarness(t)
	ctx := t.Context()

	stale := h.seed("orb_23.8.1000", strings.Repeat("1", 64), 2, 100)
	fresh := h.seed("orb_23.8.2000", strings.Repeat("2", 64), 2, 100)
	h.backdate(stale, 48*time.Hour)

	res, err := h.packages.SweepManifestCache(ctx, ManifestCachePolicy{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExpiredManifests != 2 {
		t.Errorf("expired %d manifest(s), want 2", res.ExpiredManifests)
	}
	if got := h.cachedIn(stale); got != 0 {
		t.Errorf("stale package holds %d bodies, want 0", got)
	}
	if got := h.cachedIn(fresh); got != 2 {
		t.Errorf("fresh package holds %d bodies, want 2", got)
	}
}

// A policy with neither bound keeps everything. "Sweep with no budget" must not
// mean "sweep to zero".
func TestSweepWithNoPolicyKeepsEverything(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 3, 100)

	res, err := h.packages.SweepManifestCache(t.Context(), ManifestCachePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifests() != 0 {
		t.Errorf("an empty policy reclaimed %d manifest(s)", res.Manifests())
	}
}

// Touching is what a transfer does on its way to pushing the bytes, and it must
// move the package to the back of the eviction queue.
func TestTouchProtectsAPackageFromTheNextSweep(t *testing.T) {
	h := newCacheHarness(t)
	ctx := t.Context()

	a := h.seed("orb_23.8.1000", strings.Repeat("1", 64), 2, 500)
	b := h.seed("orb_23.8.2000", strings.Repeat("2", 64), 2, 500)
	h.backdate(a, 48*time.Hour)
	h.backdate(b, 72*time.Hour)

	// b is the oldest - until something uses it.
	if err := h.packages.TouchManifestCache(ctx, b); err != nil {
		t.Fatal(err)
	}

	if _, err := h.packages.SweepManifestCache(ctx,
		ManifestCachePolicy{BudgetBytes: 1000}); err != nil {
		t.Fatal(err)
	}

	if got := h.cachedIn(b); got != 2 {
		t.Errorf("the touched package holds %d bodies, want 2", got)
	}
	if got := h.cachedIn(a); got != 0 {
		t.Errorf("the untouched package holds %d bodies, want 0", got)
	}
}

// backdate ages a package's cache stamps.
func (h *cacheHarness) backdate(packageID int64, by time.Duration) {
	h.t.Helper()
	d := h.packages.dialect
	query := d.Rewrite(`
		UPDATE package_artifacts
		   SET raw_used_at = ` + d.TimeAgo("?") + `, fetched_at = ` + d.TimeAgo("?") + `
		 WHERE package_id = ?`)
	secs := int64(by.Seconds())
	if _, err := h.packages.DB().ExecContext(h.t.Context(), query, secs, secs, packageID); err != nil {
		h.t.Fatalf("backdate package %d: %v", packageID, err)
	}
}

// cachedIn counts a package's manifests that still hold their bytes.
func (h *cacheHarness) cachedIn(packageID int64) int {
	h.t.Helper()
	var n int
	err := h.packages.DB().QueryRowContext(h.t.Context(), h.packages.dialect.Rewrite(
		`SELECT COUNT(*) FROM package_artifacts WHERE package_id = ? AND raw IS NOT NULL`),
		packageID).Scan(&n)
	if err != nil {
		h.t.Fatalf("count cached bodies: %v", err)
	}
	return n
}

// A manifest we have already fetched is served from the store.
//
// The comparison path is the one that needed this: it walked both sides from
// their roots every time, asking a vendor registry to re-serve two hundred
// manifests we held, verified and kept. A digest addresses bytes, so bytes that
// hash to it ARE the answer - there is no version of this where the registry
// would say something else.
func TestAFetchedManifestIsReadableByItsDigest(t *testing.T) {
	h := newCacheHarness(t)
	// One package whose tree was walked and whose bodies are still here.
	h.seed("orb_25.7.2131", "sha256:"+strings.Repeat("a", 64), 1, 32)

	var digest string
	if err := h.packages.DB().QueryRowContext(t.Context(),
		`SELECT digest FROM package_artifacts WHERE raw IS NOT NULL LIMIT 1`).
		Scan(&digest); err != nil {
		t.Fatal(err)
	}

	m, ok, err := h.packages.ManifestByDigest(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a manifest we hold was reported missing")
	}
	if len(m.Raw) != 32 {
		t.Errorf("raw is %d bytes, want the body as served", len(m.Raw))
	}
	if m.MediaType == "" {
		t.Error("the media type came back empty; a descriptor built from this would be wrong")
	}
}

// An evicted body is a MISS, not an error: every caller has a registry to fall
// back to, and the fetch is a fact the eviction did not unlearn.
func TestAnEvictedManifestIsAMissRatherThanAnError(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_25.7.2131", "sha256:"+strings.Repeat("b", 64), 1, 32)

	var digest string
	if err := h.packages.DB().QueryRowContext(t.Context(),
		`SELECT digest FROM package_artifacts WHERE raw IS NOT NULL LIMIT 1`).
		Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.packages.DB().ExecContext(t.Context(),
		`UPDATE package_artifacts SET raw = NULL WHERE digest = ?`, digest); err != nil {
		t.Fatal(err)
	}

	_, ok, err := h.packages.ManifestByDigest(t.Context(), digest)
	if err != nil {
		t.Fatalf("an evicted body came back as an error: %v", err)
	}
	if ok {
		t.Error("an evicted body was reported as present")
	}
}

// The releases worth walking unasked are the RECENT ones nobody has walked.
//
// Discovery records what a tag's manifest lists and stops, so a fresh package
// has one fetched artifact and a list of children nobody has read. Walking
// those ahead of time is what turns "nobody has looked" into an answer before
// anybody asks the question - and bounding it to recent releases is what stops
// that being a conversation with a vendor about eight months of history.
func TestUnanalysedRecentFindsWhatIsWorthWalking(t *testing.T) {
	h := newCacheHarness(t)

	fresh := h.seedPackageAt("orb_25.7.2131", time.Now().Add(-2*time.Hour))
	h.seedListedChild(fresh)

	old := h.seedPackageAt("orb_24.1.100", time.Now().Add(-90*24*time.Hour))
	h.seedListedChild(old)

	walked := h.seedPackageAt("orb_25.7.2000", time.Now().Add(-3*time.Hour))
	h.seedFetchedChild(walked)

	rows, err := h.packages.UnanalysedRecent(t.Context(), h.repoID, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("found %d releases to walk, want the one recent unwalked: %+v", len(rows), rows)
	}
	if rows[0].Tag != "orb_25.7.2131" {
		t.Errorf("chose %q, want the recent release nobody has walked", rows[0].Tag)
	}
	if rows[0].SourceRepository == "" {
		t.Error("the row carries no repository, so nothing could build a client for it")
	}
}

// And it is bounded, so a source that has just discovered two hundred releases
// catches up over several scans rather than opening two hundred conversations
// with somebody else's registry at once.
func TestUnanalysedRecentIsBounded(t *testing.T) {
	h := newCacheHarness(t)
	for i := range 5 {
		id := h.seedPackageAt(fmt.Sprintf("orb_25.7.%d", 2000+i), time.Now().Add(-time.Hour))
		h.seedListedChild(id)
	}

	rows, err := h.packages.UnanalysedRecent(t.Context(), h.repoID, 7*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("returned %d releases, want the two asked for", len(rows))
	}
}

// seedPackageAt writes a package published at a given moment.
func (h *cacheHarness) seedPackageAt(tag string, publishedAt time.Time) int64 {
	h.t.Helper()
	ctx := h.t.Context()

	tx, err := h.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertPackage(ctx, tx, PackageRow{
		ProductID: h.productID, SourceRepoID: h.repoID, Tag: tag,
		ManifestDigest: "sha256:" + strings.Repeat("e", 60) + fmt.Sprintf("%04d", len(tag)),
		MediaType:      "application/vnd.oci.image.index.v1+json",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.packages.DB().ExecContext(ctx,
		`UPDATE packages SET published_at = ? WHERE id = ?`,
		publishedAt.UTC().Format("2006-01-02T15:04:05Z"), id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// seedListedChild adds a child the index NAMED and nobody fetched - which is
// what discovery leaves behind.
func (h *cacheHarness) seedListedChild(packageID int64) {
	h.t.Helper()
	h.child(packageID, false)
}

// seedFetchedChild adds one that has been walked.
func (h *cacheHarness) seedFetchedChild(packageID int64) {
	h.t.Helper()
	h.child(packageID, true)
}

func (h *cacheHarness) child(packageID int64, fetched bool) {
	h.t.Helper()
	h.n++
	digest := "sha256:" + strings.Repeat("f", 60) + fmt.Sprintf("%04d", h.n)
	fetchedAt := "NULL"
	if fetched {
		fetchedAt = "'2026-01-01T00:00:00Z'"
	}
	if _, err := h.packages.DB().ExecContext(h.t.Context(),
		`INSERT INTO package_artifacts (package_id, digest, media_type, size_bytes, depth, fetched_at)
		 VALUES (?, ?, 'application/vnd.oci.image.manifest.v1+json', 512, 1, `+fetchedAt+`)`,
		packageID, digest); err != nil {
		h.t.Fatal(err)
	}
}
