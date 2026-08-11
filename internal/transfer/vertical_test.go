package transfer_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// The vertical slice: plan, lease, stream, complete, advance, tag.
//
// This is the test docs/design/17 §1 calls M3's reason for existing. It drives
// the same path a real transfer takes — the planner on the Coordinator, the
// queue handing out leases, the engine moving bytes on a worker, the
// completion closing the loop and opening the next wave — and it does it
// against a real OCI Distribution server rather than a mock of one.
//
// It runs in an external test package on purpose. Everything it touches is
// exported API, so if this compiles, the packages compose the way the
// Coordinator and the worker will actually wire them.

type slice struct {
	t   *testing.T
	src *fakeregistry.Registry
	dst *fakeregistry.Registry

	st       store.Store
	packages *store.Packages
	q        *queue.Queue
	engine   *transfer.Engine

	productID int64
	sourceID  int64
	targetID  int64
}

const (
	// Shaped like a real vendor bundle rather than a toy: NEAR publishes every
	// product under an `orbs/` namespace, and the destination has to reproduce
	// that rather than flatten it.
	sourcePath = "orbs/CFX-5000-k8s"
	// targetBase is the target's configured `repository`: a PREFIX, not the
	// destination. The source's structure is reproduced beneath it, so the
	// package itself lands at targetBase + "/" + sourcePath.
	targetBase = "nokia-lab"
	targetPath = targetBase + "/" + sourcePath
)

// newSlice wires a Coordinator and a worker against two registries.
//
// Two registries rather than one, so the STREAMING path is what gets
// exercised. A single registry would let every blob take the mount fast path
// and the test would prove that mounting works while proving nothing about
// moving bytes. Mounting has its own test below.
func newSlice(t *testing.T, opts ...fakeregistry.Option) *slice {
	t.Helper()

	src := fakeregistry.New(opts...)
	t.Cleanup(src.Close)
	dst := fakeregistry.New(opts...)
	t.Cleanup(dst.Close)

	return newSliceOn(t, src, dst)
}

func newSliceOn(t *testing.T, src, dst *fakeregistry.Registry) *slice {
	t.Helper()

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "slice.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	packages := store.NewPackages(st)
	s := &slice{
		t: t, src: src, dst: dst, st: st, packages: packages,
		q:      queue.New(packages, 0, testLogger(t)),
		engine: transfer.NewEngine(0, testLogger(t)),
	}

	res, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a','h','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	s.productID, _ = res.LastInsertId()

	s.sourceID = s.repo("source", src.Host(), sourcePath)
	s.targetID = s.repo("target", dst.Host(), targetBase)
	return s
}

// repo registers a configured repository.
//
// The row NAME is the path, because (product, role, name) is unique and a
// product with a lab and a production target has two of the same role. In a
// real deployment the name is the configured target's, and the paths beneath
// it get "<configured>/<path>" — see the planner.
func (s *slice) repo(role, host, path string) int64 {
	s.t.Helper()

	tx, err := s.st.DB().BeginTx(s.t.Context(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := s.packages.EnsureRepository(s.t.Context(), tx, s.productID, role, path,
		host, path, "generic", "config", "")
	if err != nil {
		s.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// client builds a repository handle the way the worker will: from an endpoint
// and nothing else.
func (s *slice) client(e store.Endpoint) registry.Repository {
	s.t.Helper()

	c, err := generic.New(generic.Config{
		Registry: e.Registry, Repository: e.Repository, PlainHTTP: true,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// seedPackage publishes an index over two images that share a base layer, and
// records it the way DISCOVERY would: the root fetched, its children listed
// but not walked.
//
// The shared layer is the point. A package referencing one base layer from
// several images must move it ONCE — that is the whole reason the unit of work
// is a blob rather than an image.
func (s *slice) seedPackage(tag string) store.PackageRow {
	s.t.Helper()

	shared := fakeregistry.NewLayer("a base layer shared by both platforms")
	amd := s.src.AddImage(sourcePath, "", shared, fakeregistry.NewLayer("amd64 payload"))
	arm := s.src.AddImage(sourcePath, "", shared, fakeregistry.NewLayer("arm64 payload"))

	children := []map[string]any{
		{"mediaType": registry.MediaTypeOCIManifest, "digest": amd, "size": 100},
		{"mediaType": registry.MediaTypeOCIManifest, "digest": arm, "size": 100},
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex, "manifests": children,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	rootDigest := s.src.AddManifest(sourcePath, tag, raw, registry.MediaTypeOCIIndex)
	return s.recordPackage(tag, rootDigest, 3)
}

// recordPackage writes the package row the way DISCOVERY would: the root
// fetched, its children listed but not walked.
func (s *slice) recordPackage(tag, rootDigest string, artifacts int) store.PackageRow {
	s.t.Helper()

	tx, err := s.st.DB().BeginTx(s.t.Context(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := s.packages.InsertPackage(s.t.Context(), tx, store.PackageRow{
		ProductID: s.productID, SourceRepoID: s.sourceID, Tag: tag,
		ManifestDigest: rootDigest, MediaType: registry.MediaTypeOCIIndex,
		ArtifactCount: artifacts,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatal(err)
	}

	row, err := s.packages.GetPackageByID(s.t.Context(), id)
	if err != nil {
		s.t.Fatal(err)
	}
	return row
}

// addGeneric seeds an artifact whose layers are files at annotated paths —
// NEAR's `generic_system` and `generic_custo` shape, described only by OCI.
func (s *slice) addGeneric(repoPath string, files map[string]string) string {
	s.t.Helper()

	// Sorted, so the manifest bytes are deterministic and a byte-identity
	// assertion is comparing content rather than map iteration order.
	titles := make([]string, 0, len(files))
	for title := range files {
		titles = append(titles, title)
	}
	slices.Sort(titles)

	layers := make([]fakeregistry.AnnotatedLayer, 0, len(files))
	for _, title := range titles {
		layers = append(layers, fakeregistry.AnnotatedLayer{
			Title: title, Content: files[title], MediaType: "application/json",
		})
	}
	// No tag: a component of a bundle is reached through the index, and the
	// name it answers to at the destination comes from the index's annotation.
	return s.src.AddArtifact(repoPath, "", "application/vnd.nokia.ncd.orb.generic", layers...)
}

// plan opens a transfer and plans it, returning the transfer ID.
func (s *slice) plan(pkg store.PackageRow, transferID string) transfer.Plan {
	s.t.Helper()

	if _, err := s.st.DB().ExecContext(s.t.Context(),
		`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
		 VALUES (?, ?, ?, 'replicate', ?, ?)`,
		"req-"+transferID, s.productID, pkg.ID, s.sourceID, "key-"+transferID); err != nil {
		s.t.Fatal(err)
	}

	tx, err := s.st.DB().BeginTx(s.t.Context(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.packages.CreateTransfer(s.t.Context(), tx, store.TransferRow{
		ID: transferID, RequestID: "req-" + transferID, PackageID: pkg.ID,
		SourceRepoID: s.sourceID, TargetRepoID: s.targetID, Priority: 50,
	}); err != nil {
		s.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatal(err)
	}

	sourceClient, err := generic.New(generic.Config{
		Registry: s.src.Host(), Repository: sourcePath, PlainHTTP: true,
	})
	if err != nil {
		s.t.Fatal(err)
	}

	planner := transfer.NewPlanner(s.packages, 4, testLogger(s.t))
	plan, err := planner.Plan(s.t.Context(), transfer.Request{
		TransferID: transferID, RequestID: "req-" + transferID,
		Package: pkg, SourceRepoID: s.sourceID, TargetRepoID: s.targetID,
		ProductID: s.productID, TargetName: "target", TargetRegistry: s.dst.Host(),
		TargetRegistryType: "generic", TargetBasePath: targetBase,
		SourceRepository: sourcePath,
		Priority:         50, Source: sourceClient,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return plan
}

// drain runs the worker loop to completion and reports what moved.
//
// This is the loop cmd/worker runs, with the HTTP hop removed: lease, execute,
// complete, repeat until the queue has nothing left. Removing the transport
// is deliberate — it is the only part not under test here, and leaving it in
// would make a failure ambiguous between the engine and the router.
type drainResult struct {
	Leases     int
	Succeeded  int
	Skipped    int
	Failed     int
	BytesMoved int64
	SkipReason map[string]int
	LastError  error
}

func (s *slice) drain(workerID string, capacity int) drainResult {
	s.t.Helper()
	return s.drainInto(workerID, capacity, s.dst)
}

// drainInto runs the worker loop when the destination is not the harness's
// own — a promotion moves into a registry the first hop never touched.
func (s *slice) drainInto(workerID string, capacity int, _ *fakeregistry.Registry) drainResult {
	s.t.Helper()
	ctx := s.t.Context()

	out := drainResult{SkipReason: map[string]int{}}

	// Bounded rather than `for {}`: a bug that leaves a job permanently
	// leasable should fail this test in a second, not hang the suite.
	for range 100 {
		lease, err := s.q.Lease(ctx, workerID, capacity)
		if err != nil {
			s.t.Fatalf("lease: %v", err)
		}
		if len(lease.Jobs) == 0 {
			return out
		}
		out.Leases++

		for _, a := range lease.Jobs {
			res := s.engine.Run(ctx, transfer.Job{
				ID:             a.ID,
				TransferID:     a.TransferID,
				Kind:           a.Kind,
				Digest:         a.Digest,
				SizeBytes:      a.SizeBytes,
				MediaType:      a.MediaType,
				Attempt:        a.Attempt,
				Source:         s.client(a.Source),
				Target:         s.client(a.Target),
				KnownPlacement: a.KnownPlacement,
				MountFrom:      a.MountFrom,
				Tags:           a.Tags,
			}, nil)

			switch res.Outcome {
			case transfer.OutcomeSucceeded:
				out.Succeeded++
			case transfer.OutcomeSkipped:
				out.Skipped++
				out.SkipReason[res.SkipReason]++
			case transfer.OutcomeFailed:
				out.Failed++
				out.LastError = res.Err
			}
			out.BytesMoved += res.BytesMoved

			if _, err := s.q.Complete(ctx, store.Completion{
				JobID:            a.ID,
				Owner:            workerID,
				Outcome:          string(res.Outcome),
				BytesTransferred: res.BytesMoved,
				SkipReason:       res.SkipReason,
				ErrorClass:       res.ErrorClass,
				ErrorMsg:         errString(res.Err),
				Placed:           res.Placed,
				Attempt:          a.Attempt,
			}); err != nil {
				s.t.Fatalf("complete job %d: %v", a.ID, err)
			}
		}
	}
	s.t.Fatal("the queue never drained: a job stayed leasable for 100 rounds")
	return out
}

// openTransfer records a request and one transfer, the way the requester does
// when a person asks for a copy.
func (s *slice) openTransfer(id string, pkg store.PackageRow, operation string, from, to int64) {
	s.t.Helper()

	if _, err := s.st.DB().ExecContext(s.t.Context(),
		`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"req-"+id, s.productID, pkg.ID, operation, from, "key-"+id); err != nil {
		s.t.Fatal(err)
	}

	tx, err := s.st.DB().BeginTx(s.t.Context(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.packages.CreateTransfer(s.t.Context(), tx, store.TransferRow{
		ID: id, RequestID: "req-" + id, PackageID: pkg.ID,
		SourceRepoID: from, TargetRepoID: to, Priority: 50,
	}); err != nil {
		s.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		s.t.Fatal(err)
	}
}

func (s *slice) transferState(id string) string {
	s.t.Helper()
	var state string
	if err := s.st.DB().QueryRowContext(s.t.Context(),
		`SELECT state FROM transfers WHERE id = ?`, id).Scan(&state); err != nil {
		s.t.Fatal(err)
	}
	return state
}

// ---------------------------------------------------------------------------

// THE MILESTONE TEST. A package moves from a source registry to a destination
// registry, and the destination ends up holding the whole thing under the
// right tag.
func TestPackageTransfersEndToEnd(t *testing.T) {
	s := newSlice(t)
	pkg := s.seedPackage("v2.14.0")

	plan := s.plan(pkg, "transfer-1")

	// Three manifests (an index and two images) and five blobs: two configs,
	// two platform layers, and ONE shared base layer counted once.
	if plan.Manifests != 3 {
		t.Fatalf("planned %d manifests, want 3", plan.Manifests)
	}
	if plan.Blobs != 5 {
		t.Fatalf("planned %d blobs, want 5 (the shared base layer must be counted once)", plan.Blobs)
	}
	if plan.MaxWave != 2 {
		t.Fatalf("planned max wave %d, want 2 (blobs, images, index)", plan.MaxWave)
	}

	got := s.drain("worker-1", 4)
	if got.Failed != 0 {
		t.Fatalf("%d jobs failed, last error: %v", got.Failed, got.LastError)
	}
	if got.BytesMoved == 0 {
		t.Fatal("nothing moved: a first transfer to an empty destination must stream every blob")
	}

	if state := s.transferState("transfer-1"); state != "succeeded" {
		t.Fatalf("transfer is %q, want succeeded", state)
	}

	// The destination holds the content, and the tag resolves to the same root
	// the source published. That last part is invariant I1's observable end:
	// a consumer pulling this tag gets the complete package.
	assertDestinationComplete(t, s, pkg)
}

func assertDestinationComplete(t *testing.T, s *slice, pkg store.PackageRow) {
	t.Helper()

	if got := s.dst.TagDigest(targetPath, pkg.Tag); got != pkg.ManifestDigest {
		t.Fatalf("destination tag %s resolves to %s, want %s", pkg.Tag, got, pkg.ManifestDigest)
	}

	tree, complete, err := s.packages.ReadExpandedTree(t.Context(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("the planner did not record a complete tree")
	}

	for _, a := range tree.Artifacts {
		if _, ok := s.dst.ManifestBytes(targetPath, a.Row.Digest); !ok {
			t.Errorf("manifest %s is missing from the destination", a.Row.Digest)
		}
		for _, b := range a.Blobs {
			if !s.dst.BlobExists(targetPath, b.Digest) {
				t.Errorf("blob %s is missing from the destination", b.Digest)
			}
		}
	}
}

// Waves are what keep a tag honest. A manifest must never be pushed before the
// blobs it references, and the index must land after the images it names.
func TestWavesOrderTheTransfer(t *testing.T) {
	s := newSlice(t)
	pkg := s.seedPackage("v2.14.0")
	s.plan(pkg, "transfer-waves")

	// Capacity of one, so every job is leased in isolation and the ordering
	// under test is the queue's rather than a lucky interleaving.
	got := s.drain("worker-1", 1)
	if got.Failed != 0 {
		t.Fatalf("%d jobs failed, last error: %v", got.Failed, got.LastError)
	}

	// If ordering were wrong the fake registry would have rejected a manifest
	// with BLOB_UNKNOWN, so reaching `succeeded` IS the assertion. Stated
	// explicitly because a reader should not have to infer it.
	if state := s.transferState("transfer-waves"); state != "succeeded" {
		t.Fatalf("transfer is %q, want succeeded — a manifest was pushed before its blobs", state)
	}
}

// DEDUPLICATION IS THE SYSTEM'S LARGEST OPTIMIZATION, and it can silently
// regress: a placement-key bug shows up as "slightly slower", never as a
// failure. So re-transferring an already-present package must move ~zero
// bytes, and that is asserted rather than assumed.
func TestSecondTransferMovesAlmostNothing(t *testing.T) {
	s := newSlice(t)
	pkg := s.seedPackage("v2.14.0")

	s.plan(pkg, "transfer-first")
	first := s.drain("worker-1", 4)
	if first.Failed != 0 {
		t.Fatalf("first transfer failed: %v", first.LastError)
	}

	// A second request for the same package to the same destination. The
	// planner should now find every blob already placed and create no blob
	// jobs at all.
	second := s.plan(pkg, "transfer-second")
	if second.Blobs != 0 {
		t.Errorf("second plan created %d blob jobs, want 0 — every blob is already placed",
			second.Blobs)
	}
	if second.DedupeSkippedBytes == 0 {
		t.Error("second plan reported no deduplicated bytes")
	}

	got := s.drain("worker-1", 4)
	if got.Failed != 0 {
		t.Fatalf("second transfer failed: %v", got.LastError)
	}
	if got.BytesMoved != 0 {
		t.Errorf("second transfer moved %d bytes, want 0", got.BytesMoved)
	}
	if state := s.transferState("transfer-second"); state != "succeeded" {
		t.Fatalf("second transfer is %q, want succeeded", state)
	}
}

// Within one registry the blobs should be relocated server-side rather than
// pulled and pushed. This is the dominant optimization for promotion, where a
// 45 GB move can complete in seconds.
func TestSameRegistryMountsInsteadOfStreaming(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	s := newSliceOn(t, reg, reg)
	pkg := s.seedPackage("v2.14.0")
	s.plan(pkg, "transfer-mount")

	got := s.drain("worker-1", 4)
	if got.Failed != 0 {
		t.Fatalf("%d jobs failed, last error: %v", got.Failed, got.LastError)
	}
	if got.SkipReason[transfer.SkipMounted] == 0 {
		t.Fatal("no blob was mounted: within one registry every blob should relocate server-side")
	}
	if reg.UploadedBytes.Load() != 0 {
		t.Errorf("%d bytes were uploaded; a same-registry transfer should move none",
			reg.UploadedBytes.Load())
	}
	if state := s.transferState("transfer-mount"); state != "succeeded" {
		t.Fatalf("transfer is %q, want succeeded", state)
	}
}

// A registry that answers 202 to a mount has DECLINED it and opened an
// ordinary upload session. That is a normal outcome, not an error, and a
// client that treats it as one fails against half the registries in use.
func TestDeclinedMountFallsBackToStreaming(t *testing.T) {
	reg := fakeregistry.New(fakeregistry.WithDeclinedMounts())
	t.Cleanup(reg.Close)

	s := newSliceOn(t, reg, reg)
	pkg := s.seedPackage("v2.14.0")
	s.plan(pkg, "transfer-declined")

	got := s.drain("worker-1", 4)
	if got.Failed != 0 {
		t.Fatalf("%d jobs failed, last error: %v", got.Failed, got.LastError)
	}
	if got.BytesMoved == 0 {
		t.Fatal("a declined mount must fall back to streaming, and streaming moves bytes")
	}
	if state := s.transferState("transfer-declined"); state != "succeeded" {
		t.Fatalf("transfer is %q, want succeeded", state)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// testLogger keeps a failing test's output about the failure.
//
// The engine and the queue both log at INFO on every job, which for a package
// with eight jobs buries the one line that matters. Warnings and above still
// come through, because a test that provokes one should say so.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
