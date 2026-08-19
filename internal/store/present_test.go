package store

import (
	"strings"
	"testing"
)

// "Saved 63.7 GB" beside "Nothing has been found at the destination yet."
//
// Both came from this system and they cannot both be right. The listing read
// JOBS, and most of a saving leaves no job behind: planning asks the
// destination what it already holds, and content it holds gets no job at all -
// that is the point of the check, and those bytes are the great majority of the
// figure in the headline.
//
// So the listing must read what the release CONTAINS and ask whether the
// destination has it, by either route.
func TestWhatWasAlreadyThereIncludesContentThatNeverGotAJob(t *testing.T) {
	h := newPresentHarness(t)

	// Two blobs of one component. One was decided at PLANNING - recorded as
	// already at the target, so no job exists for it - and one got a job that a
	// worker then skipped.
	planned := h.blob("aa", 30_000_000_000)
	skipped := h.blob("bb", 2_000_000_000)
	artifact := h.artifact("cfx-5000-product/bgcf:25.7.673", planned, skipped)

	id := h.transfer()
	h.placed(planned)
	h.job(id, skipped, "skipped")

	got, err := h.packages.PresentComponents(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("reported %d components, want the one whose content is there: %+v", len(got), got)
	}
	if got[0].Digest != artifact {
		t.Errorf("named %s, want the component %s", got[0].Digest, artifact)
	}
	if got[0].Name != "cfx-5000-product/bgcf:25.7.673" {
		t.Errorf("name = %q, want the vendor's own name", got[0].Name)
	}

	// BOTH routes counted. Reading jobs alone reported the two gigabytes and
	// missed the thirty, which is how the listing came to say nothing was found
	// while the headline said 63.7 GB.
	if want := int64(32_000_000_000); got[0].Bytes != want {
		t.Errorf("bytes = %d, want %d - the content decided at planning time is "+
			"missing from what the destination is said to hold", got[0].Bytes, want)
	}
}

// A blob several components share is counted ONCE, or the parts of a saving
// add up to more than the saving.
func TestSharedContentIsCountedOnce(t *testing.T) {
	h := newPresentHarness(t)

	base := h.blob("cc", 1_000_000_000)
	h.artifact("cfx-5000-product/one:1.0", base)
	h.artifact("cfx-5000-product/two:1.0", base)

	id := h.transfer()
	h.placed(base)

	got, err := h.packages.PresentComponents(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	var total int64
	for _, c := range got {
		total += c.Bytes
	}
	if total != 1_000_000_000 {
		t.Errorf("a base layer shared by two components contributed %d, want it "+
			"counted once: %+v", total, got)
	}
}

// Content nobody has found at the destination is not listed, or the list stops
// being a list of what was already there.
func TestContentThatIsNotThereIsNotListed(t *testing.T) {
	h := newPresentHarness(t)

	blob := h.blob("dd", 500)
	h.artifact("cfx-5000-product/absent:1.0", blob)

	id := h.transfer()
	h.job(id, blob, "pending")

	got, err := h.packages.PresentComponents(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("listed %d components the destination does not hold: %+v", len(got), got)
	}
}

type presentHarness struct {
	t         *testing.T
	st        Store
	packages  *Packages
	productID int64
	repoID    int64
	packageID int64
	n         int
}

func newPresentHarness(t *testing.T) *presentHarness {
	t.Helper()

	st := openTestStore(t)
	h := &presentHarness{t: t, st: st, packages: NewPackages(st)}

	res, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('cfx-5000-product','h','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	h.productID, _ = res.LastInsertId()

	tx, err := st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.repoID, err = h.packages.EnsureRepository(t.Context(), tx, h.productID, "source", "src",
		"registry.example.com", "orbs/cfx-5000-k8s", "generic", "config", "")
	if err != nil {
		t.Fatal(err)
	}
	h.packageID, err = h.packages.InsertPackage(t.Context(), tx, PackageRow{
		ProductID: h.productID, SourceRepoID: h.repoID, Tag: "25.7_mp2604_2131",
		ManifestDigest: "sha256:" + strings.Repeat("0", 64),
		MediaType:      "application/vnd.oci.image.index.v1+json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return h
}

// blob records one blob of a given size and returns its digest.
func (h *presentHarness) blob(seed string, size int64) string {
	h.t.Helper()
	digest := "sha256:" + strings.Repeat(seed, 32)
	h.exec(`INSERT INTO blobs (digest, size_bytes, media_type) VALUES (?, ?, 'application/octet-stream')
	         ON CONFLICT (digest) DO NOTHING`, digest, size)
	return digest
}

// artifact records one component of the package, referencing the given blobs.
func (h *presentHarness) artifact(refName string, blobs ...string) string {
	h.t.Helper()
	h.n++
	digest := "sha256:" + strings.Repeat("e", 62) + padded(h.n)

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertArtifact(h.t.Context(), tx, ArtifactRow{
		PackageID: h.packageID,
		Digest:    digest,
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		SizeBytes: 512,
		Annotations: map[string]string{
			"org.opencontainers.image.ref.name": refName,
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	refs := make([]BlobRef, 0, len(blobs))
	for i, b := range blobs {
		refs = append(refs, BlobRef{Digest: b, Kind: "layer", Ordinal: i})
	}
	if err := h.packages.LinkBlobs(h.t.Context(), tx, id, refs); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return digest
}

func (h *presentHarness) transfer() string {
	h.t.Helper()
	id := "aaaaaaaa-1111-2222-3333-444444444444"

	h.exec(`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
	         VALUES (?, ?, ?, 'replicate', ?, ?)`,
		"req-"+id, h.productID, h.packageID, h.repoID, "key-"+id)

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := h.packages.CreateTransfer(h.t.Context(), tx, TransferRow{
		ID: id, RequestID: "req-" + id, PackageID: h.packageID,
		SourceRepoID: h.repoID, TargetRepoID: h.repoID, Priority: 50,
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// placed records the destination as already holding a blob - the planning-time
// answer, which produces no job at all.
func (h *presentHarness) placed(digest string) {
	h.t.Helper()
	h.exec(`INSERT INTO blob_placements (repository_id, digest, size_bytes, source, verified_at)
	         VALUES (?, ?, 0, 'observed', `+h.packages.dialect.Now()+`)`, h.repoID, digest)
}

func (h *presentHarness) job(transferID, digest, state string) {
	h.t.Helper()
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
	                          target_repo_id, state, wave, attempts, max_attempts)
	         VALUES (?, 'blob', ?, 0, ?, ?, ?, 0, 1, 8)`,
		transferID, digest, h.repoID, h.repoID, state)
}

func (h *presentHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(),
		h.packages.dialect.Rewrite(query), args...); err != nil {
		h.t.Fatal(err)
	}
}

// Bytes are weighed ONCE per digest, however many repositories the content has
// to reach.
//
// A component published under its own name as well as inside the bundle needs
// its layers in two repositories, and the planner counts them twice because two
// repositories is two pieces of bookkeeping. But the second copy costs no bytes
// - the registry mounts it - so a byte total counted per (repository, digest)
// reported a 29.8 GB release as 63.7 GB of traffic, which never happened.
func TestContentIsWeighedOncePerDigest(t *testing.T) {
	h := newPresentHarness(t)

	big := h.blob("aa", 20_000_000_000)
	small := h.blob("bb", 1_000_000_000)
	h.artifact("cfx-5000-product/bgcf:25.7.673", big, small)

	id := h.transfer()

	// The same two blobs, each with a job per destination repository: one in
	// the bundle's container and one under the component's own name. This is
	// what doubled the totals.
	h.jobIn(id, big, "succeeded", "orbs/cfx-5000-k8s", 20_000_000_000)
	h.jobIn(id, big, "skipped", "cfx-5000-product/bgcf", 0)
	h.jobIn(id, small, "succeeded", "orbs/cfx-5000-k8s", 1_000_000_000)
	h.jobIn(id, small, "skipped", "cfx-5000-product/bgcf", 0)

	got, err := h.packages.TransferContentBytes(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	// 21 GB of blobs plus the component's own manifest, which is content too.
	if got.Total < 21_000_000_000 || got.Total > 21_000_001_000 {
		t.Errorf("total = %d, want the distinct content - about 21 GB, not 42", got.Total)
	}
	if got.Moved != 21_000_000_000 {
		t.Errorf("moved = %d, want 21 GB: each blob was streamed once and mounted "+
			"once, and a mount moves nothing", got.Moved)
	}
	// The second copy is not a saving either. It is the same content arriving
	// at a second path, and counting it as saved is what made "saved" exceed
	// the size of the release.
	if got.Present != 0 {
		t.Errorf("present = %d, want 0: nothing here was at the destination "+
			"before this transfer put it there", got.Present)
	}
	if got.Moved+got.Present > got.Total {
		t.Errorf("moved + present = %d exceeds the total of %d",
			got.Moved+got.Present, got.Total)
	}
}

// And content the destination already held is weighed once too, whether that
// was decided at planning time or by a worker.
func TestContentAlreadyThereIsWeighedOnceAndNotAsMoved(t *testing.T) {
	h := newPresentHarness(t)

	blob := h.blob("cc", 5_000_000_000)
	h.artifact("cfx-5000-product/cvlk:1.0.11", blob)

	id := h.transfer()
	h.placed(blob)

	got, err := h.packages.TransferContentBytes(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Present != 5_000_000_000 {
		t.Errorf("present = %d, want the 5 GB the destination already held", got.Present)
	}
	if got.Moved != 0 {
		t.Errorf("moved = %d, want 0: nothing was streamed", got.Moved)
	}
}

// jobIn records one job for a digest at a named destination repository.
//
// A destination repository is its own catalog row - which is exactly why the
// same blob can have two jobs in one transfer, and exactly why counting their
// bytes twice was wrong.
func (h *presentHarness) jobIn(transferID, digest, state, repository string, moved int64) {
	h.t.Helper()
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
	                          target_repo_id, target_repository, state, bytes_transferred,
	                          wave, attempts, max_attempts)
	         VALUES (?, 'blob', ?, 0, ?, ?, ?, ?, ?, 0, 1, 8)`,
		transferID, digest, h.repoID, h.destination(repository), repository, state, moved)
}

// destination is the catalog row for one destination repository.
func (h *presentHarness) destination(repository string) int64 {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.EnsureRepository(h.t.Context(), tx, h.productID, "target",
		"lab/"+repository, "jfrog.example.com", repository, "generic", "config", "")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}
