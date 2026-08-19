package store

import (
	"strings"
	"testing"
)

// "Saved 63.7 GB" beside "Nothing has been found at the destination yet."
//
// Both came from this system and they cannot both be right. The listing read
// JOBS, and most of a saving leaves no job behind: planning asks the
// destination what it already holds, and content it holds gets no job at all —
// that is the point of the check, and those bytes are the great majority of the
// figure in the headline.
//
// So the listing must read what the release CONTAINS and ask whether the
// destination has it, by either route.
func TestWhatWasAlreadyThereIncludesContentThatNeverGotAJob(t *testing.T) {
	h := newPresentHarness(t)

	// Two blobs of one component. One was decided at PLANNING — recorded as
	// already at the target, so no job exists for it — and one got a job that a
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
		t.Errorf("bytes = %d, want %d — the content decided at planning time is "+
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

// placed records the destination as already holding a blob — the planning-time
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
