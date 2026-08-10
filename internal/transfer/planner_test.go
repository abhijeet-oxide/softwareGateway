package transfer

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

type harness struct {
	t        *testing.T
	reg      *fakeregistry.Registry
	packages *store.Packages
	st       store.Store

	productID int64
	sourceID  int64
	targetID  int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "plan.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, reg: reg, packages: store.NewPackages(st), st: st}

	res, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a','h','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	h.productID, _ = res.LastInsertId()

	h.sourceID = h.repo("source", "vendor/suite")
	h.targetID = h.repo("target", "mirror/vendor/suite")
	return h
}

func (h *harness) repo(role, path string) int64 {
	h.t.Helper()
	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.EnsureRepository(h.t.Context(), tx, h.productID, role, role+"-"+path,
		"registry.example.com", path, "generic", "config", "")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// seedPackage puts a package in the registry and records it the way DISCOVERY
// would: the root manifest fetched, its children listed but not.
func (h *harness) seedPackage(tag string, rootDigest string, mediaType string) store.PackageRow {
	h.t.Helper()

	client := h.client("vendor/suite")
	desc, _, err := client.FetchManifest(h.t.Context(), rootDigest)
	if err != nil {
		h.t.Fatal(err)
	}

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertPackage(h.t.Context(), tx, store.PackageRow{
		ProductID: h.productID, SourceRepoID: h.sourceID, Tag: tag,
		ManifestDigest: desc.Digest.String(), MediaType: mediaType, ArtifactCount: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}

	row, err := h.packages.GetPackageByID(h.t.Context(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return row
}

func (h *harness) client(path string) *generic.Repository {
	h.t.Helper()
	c, err := generic.New(generic.Config{
		Registry: h.reg.Host(), Repository: path, PlainHTTP: true,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) transfer(id string) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
		 SELECT ?, ?, id, 'replicate', ?, ? FROM packages LIMIT 1`,
		"req-"+id, h.productID, h.sourceID, "key-"+id); err != nil {
		h.t.Fatal(err)
	}

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	var pkgID int64
	if err := tx.QueryRowContext(h.t.Context(), `SELECT id FROM packages LIMIT 1`).Scan(&pkgID); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.packages.CreateTransfer(h.t.Context(), tx, store.TransferRow{
		ID: id, RequestID: "req-" + id, PackageID: pkgID,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Priority: 50,
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRowContext(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// seedIndex writes an index over several images, so the tree has real depth.
func (h *harness) seedIndex(tag string, images []string) string {
	h.t.Helper()
	children := make([]map[string]any, 0, len(images))
	for _, d := range images {
		children = append(children, map[string]any{
			"mediaType": registry.MediaTypeOCIManifest, "digest": d, "size": 100,
		})
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex, "manifests": children,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.reg.AddManifest("vendor/suite", tag, raw, registry.MediaTypeOCIIndex)
}

// ---------------------------------------------------------------------------

// THE UNIT OF WORK IS A BLOB. One job per blob, one per manifest, and nothing
// in between — which is what makes a thousand-blob package saturate a fleet and
// a failure cost one blob rather than the package.
func TestPlanCreatesOneJobPerBlobAndManifest(t *testing.T) {
	h := newHarness(t)

	shared := fakeregistry.NewLayer("shared-base-layer")
	amd := h.reg.AddImage("vendor/suite", "", shared, fakeregistry.NewLayer("amd"))
	arm := h.reg.AddImage("vendor/suite", "", shared, fakeregistry.NewLayer("arm"))
	root := h.seedIndex("v1.0.0", []string{amd, arm})

	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	plan, err := NewPlanner(h.packages, 4, nil).Plan(t.Context(), Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID,
		Source: h.client("vendor/suite"), Priority: 50,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Three manifests: the index and its two images.
	if plan.Manifests != 3 {
		t.Errorf("created %d manifest jobs, want 3", plan.Manifests)
	}
	// Five distinct blobs: two configs, and three layers — the SHARED base
	// counted once, which is the whole point of the unit being a blob.
	if plan.Blobs != 5 {
		t.Errorf("created %d blob jobs, want 5 (2 configs + 3 distinct layers)", plan.Blobs)
	}
	if got := h.count(`SELECT COUNT(*) FROM jobs WHERE transfer_id='t1'`); got != plan.Jobs {
		t.Errorf("%d job rows but the plan reported %d", got, plan.Jobs)
	}
}

// Blobs go first and manifests are BLOCKED behind them. This is invariant I1:
// a tag never appears at the destination until everything under it is present,
// so an interrupted transfer leaves a consumer seeing the old tag or the
// complete new one, never a half-written one.
func TestBlobsArePendingAndManifestsAreBlocked(t *testing.T) {
	h := newHarness(t)

	img := h.reg.AddImage("vendor/suite", "", fakeregistry.NewLayer("l"))
	root := h.seedIndex("v1.0.0", []string{img})
	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	plan, err := NewPlanner(h.packages, 4, nil).Plan(t.Context(), Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := h.count(`SELECT COUNT(*) FROM jobs WHERE kind='blob' AND state<>'pending'`); got != 0 {
		t.Errorf("%d blob jobs are not pending; nothing blocks a blob", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM jobs WHERE kind='manifest' AND state<>'blocked'`); got != 0 {
		t.Errorf("%d manifest jobs are not blocked; a manifest must wait for its blobs", got)
	}

	// The index is deeper in wave order than the image it names, so the image
	// is pushed first and the registry never sees a dangling reference.
	imageWave := h.count(`SELECT wave FROM jobs WHERE kind='manifest' AND digest=?`, img)
	indexWave := h.count(`SELECT wave FROM jobs WHERE kind='manifest' AND digest=?`, root)
	if imageWave >= indexWave {
		t.Errorf("image is wave %d and the index wave %d; the index must come last",
			imageWave, indexWave)
	}
	if plan.MaxWave != indexWave {
		t.Errorf("MaxWave = %d, want the index's wave %d", plan.MaxWave, indexWave)
	}
}

// DEDUPE ACROSS PACKAGES. A blob already at the destination gets NO JOB — not a
// job that will be skipped, which would still cost a lease, a round trip and a
// row. This is what makes the second release of a product line nearly free.
func TestBlobsAlreadyAtTheDestinationGetNoJob(t *testing.T) {
	h := newHarness(t)

	shared := fakeregistry.NewLayer("shared-base-layer")
	img := h.reg.AddImage("vendor/suite", "", shared, fakeregistry.NewLayer("unique"))
	root := h.seedIndex("v1.0.0", []string{img})
	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	// The shared base is already at the destination, as it would be after an
	// earlier release of the same product line moved it.
	tx, err := h.st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.packages.RecordPlacement(t.Context(), tx, h.targetID,
		shared.Digest, shared.Size, "transferred"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlanner(h.packages, 4, nil).Plan(t.Context(), Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if plan.SkippedBlobs != 1 {
		t.Errorf("skipped %d blobs, want 1", plan.SkippedBlobs)
	}
	if plan.DedupeSkippedBytes != shared.Size {
		t.Errorf("dedupe saved %d bytes, want %d", plan.DedupeSkippedBytes, shared.Size)
	}
	if got := h.count(`SELECT COUNT(*) FROM jobs WHERE kind='blob' AND digest=?`, shared.Digest); got != 0 {
		t.Errorf("a blob already at the destination got %d jobs, want none", got)
	}
}

// Planning is IDEMPOTENT. A Coordinator that dies mid-plan leaves a partial job
// set, and the replan on restart must find those rows free rather than
// duplicating them.
func TestReplanningCreatesNoDuplicates(t *testing.T) {
	h := newHarness(t)

	img := h.reg.AddImage("vendor/suite", "", fakeregistry.NewLayer("l"))
	root := h.seedIndex("v1.0.0", []string{img})
	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	planner := NewPlanner(h.packages, 4, nil)
	req := Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	}

	first, err := planner.Plan(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	before := h.count(`SELECT COUNT(*) FROM jobs WHERE transfer_id='t1'`)

	second, err := planner.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if second.Jobs != 0 {
		t.Errorf("a replan created %d jobs, want 0", second.Jobs)
	}
	if got := h.count(`SELECT COUNT(*) FROM jobs WHERE transfer_id='t1'`); got != before {
		t.Errorf("job rows went from %d to %d on a replan", before, got)
	}
	if first.Jobs == 0 {
		t.Error("the first plan created nothing, so this proves nothing")
	}
}

// THE WALK HAPPENS ONCE. A package already expanded by `describe --expand` must
// be planned from the record, without touching the registry.
func TestPlanningAnExpandedPackageDoesNotWalk(t *testing.T) {
	h := newHarness(t)

	img := h.reg.AddImage("vendor/suite", "", fakeregistry.NewLayer("l"))
	root := h.seedIndex("v1.0.0", []string{img})
	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	planner := NewPlanner(h.packages, 4, nil)
	req := Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	}

	first, err := planner.Plan(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Walked == 0 {
		t.Fatal("the first plan should have walked: the package was not expanded")
	}

	// A second transfer of the same package to another target plans from the
	// record. THIS is the property: the walk is paid for once, by whoever asks
	// first, and everyone after reads it.
	other := h.repo("target", "other/vendor/suite")
	h.transfer("t2")

	before := h.reg.ManifestCalls.Load()
	second, err := planner.Plan(t.Context(), Request{
		TransferID: "t2", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: other, Source: h.client("vendor/suite"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if second.Walked != 0 {
		t.Errorf("the second plan walked %d manifests; the tree was already recorded", second.Walked)
	}
	if got := h.reg.ManifestCalls.Load() - before; got != 0 {
		t.Errorf("the second plan made %d registry calls, want 0", got)
	}
	if second.Jobs != first.Jobs {
		t.Errorf("planning from the record produced %d jobs, the walk produced %d",
			second.Jobs, first.Jobs)
	}
}

// Planning WITHOUT a prior inspect must populate exactly what inspect would
// have, so the sizes an estimate needs are present either way.
func TestPlanningPopulatesTheSizesAnEstimateNeeds(t *testing.T) {
	h := newHarness(t)

	l1 := fakeregistry.NewLayer("layer-one")
	l2 := fakeregistry.NewLayer("layer-two-which-is-longer")
	img := h.reg.AddImage("vendor/suite", "", l1, l2)
	root := h.seedIndex("v1.0.0", []string{img})
	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	// Before: discovery recorded the index and listed its child; no size.
	if pkg.TotalBytes != nil {
		t.Fatalf("a freshly discovered index should have no measured size, got %d", *pkg.TotalBytes)
	}

	if _, err := NewPlanner(h.packages, 4, nil).Plan(t.Context(), Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	}); err != nil {
		t.Fatal(err)
	}

	after, err := h.packages.GetPackageByID(t.Context(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalBytes == nil {
		t.Fatal("planning must record the measured size, as inspect does")
	}
	if after.BlobCount == nil || *after.BlobCount != 3 {
		t.Errorf("blob count = %v, want 3 (one config, two layers)", after.BlobCount)
	}

	// Per-blob sizes, which is what a per-layer estimate is built from.
	var jobBytes int64
	if err := h.st.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(SUM(size_bytes),0) FROM jobs WHERE transfer_id='t1' AND kind='blob'`,
	).Scan(&jobBytes); err != nil {
		t.Fatal(err)
	}
	if jobBytes < l1.Size+l2.Size {
		t.Errorf("blob jobs total %d bytes, less than the two layers alone (%d)",
			jobBytes, l1.Size+l2.Size)
	}
}

// A package whose transfer root is a WRAPPER must be planned from the wrapper,
// so the signature travels with the payload.
func TestPlanUsesTheTransferRootWhenOneIsRecorded(t *testing.T) {
	h := newHarness(t)

	payload := h.reg.AddImage("vendor/suite", "orb_1.0", fakeregistry.NewLayer("payload"))
	signature := h.reg.AddImage("vendor/suite", "signature_orb_1.0", fakeregistry.NewLayer("pkcs7"))
	wrapper := h.seedIndex("signed_orb_1.0", []string{payload, signature})

	pkg := h.seedPackage("orb_1.0", payload, registry.MediaTypeOCIManifest)
	// The layout plugin recorded the wrapper as the transfer root.
	tx, err := h.st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.packages.UpdateSignatureState(t.Context(), tx, pkg.ID,
		"signed", wrapper, "signed_orb_1.0"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	pkg, err = h.packages.GetPackageByID(t.Context(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.transfer("t1")

	if _, err := NewPlanner(h.packages, 4, nil).Plan(t.Context(), Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	}); err != nil {
		t.Fatal(err)
	}

	// The plan must reach all three manifests — wrapper, payload, signature —
	// and therefore the PKCS#7 blob. Planning from the payload alone would move
	// the bytes and foreclose destination-side verification for good.
	for name, dgst := range map[string]string{
		"wrapper": wrapper, "payload": payload, "signature": signature,
	} {
		if got := h.count(
			`SELECT COUNT(*) FROM jobs WHERE kind='manifest' AND digest=?`, dgst); got != 1 {
			t.Errorf("the %s manifest has %d jobs, want 1", name, got)
		}
	}
}
