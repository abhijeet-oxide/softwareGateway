package transfer

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// The planner's half of per-artifact readiness: it has to write down WHAT each
// manifest is waiting for, precisely enough that the queue can promote one
// artifact without promoting the rest.

// Every reference in the tree becomes an edge, and the edges are the OCI
// preconditions rather than a heuristic: a manifest push is rejected
// BLOB_UNKNOWN if a layer it names is absent, and MANIFEST_UNKNOWN if a child
// index is.
func TestPlanRecordsWhatEachManifestIsWaitingFor(t *testing.T) {
	h := newHarness(t)

	shared := fakeregistry.NewLayer("shared-base-layer")
	amd := h.reg.AddImage("vendor/suite", "", shared, fakeregistry.NewLayer("amd"))
	arm := h.reg.AddImage("vendor/suite", "", shared, fakeregistry.NewLayer("arm"))
	root := h.seedIndex("v1.0.0", []string{amd, arm})

	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)
	h.transfer("t1")

	plan, err := NewPlanner(h.packages, 4, nil).Plan(t.Context(), Request{
		TransferID: "t1", Package: pkg,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Source: h.client("vendor/suite"),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Dependencies == 0 {
		t.Fatal("the plan wrote no dependencies; every manifest would be pushed immediately")
	}

	// Each image waits for its own config and its own two layers — the shared
	// base counts for both, because a dependency is on the JOB and there is one
	// job per (digest, destination).
	for _, image := range []string{amd, arm} {
		if got := h.dependencyCount(image); got != 3 {
			t.Errorf("image %s waits on %d jobs, want 3 (config + 2 layers)", image[:14], got)
		}
	}

	// The index waits for the two manifests it names, and for nothing else: an
	// index has no layers of its own.
	if got := h.dependencyCount(root); got != 2 {
		t.Errorf("the index waits on %d jobs, want the 2 manifests it names", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM job_dependencies d
	                     JOIN jobs dep ON dep.id = d.depends_on_id
	                     JOIN jobs j ON j.id = d.job_id
	                    WHERE j.digest = ? AND dep.kind <> 'manifest'`, root); got != 0 {
		t.Errorf("the index waits on %d blob job(s); it references no blobs", got)
	}
}

// A blob already at the destination gets no job, so a manifest whose content is
// entirely deduplicated away has nothing to wait for. It must be created
// RUNNABLE — blocking it would be waiting for a completion that can never
// arrive, which is the deadlock OpenFirstWave exists to avoid for waves.
//
// This is the second transfer of any product line, which is the case
// deduplication exists to make fast.
func TestAManifestWithNothingLeftToMoveStartsRunnable(t *testing.T) {
	h := newHarness(t)

	img := h.reg.AddImage("vendor/suite", "", fakeregistry.NewLayer("payload"))
	root := h.seedIndex("v1.0.0", []string{img})
	pkg := h.seedPackage("v1.0.0", root, registry.MediaTypeOCIIndex)

	planner := NewPlanner(h.packages, 4, nil)
	req := Request{
		Package: pkg, SourceRepoID: h.sourceID, TargetRepoID: h.targetID,
		Source: h.client("vendor/suite"),
	}

	// The first transfer walks the tree and records it. Everything it would
	// have moved is then at the destination — which is exactly the state the
	// SECOND release of a product line starts from.
	h.transfer("t1")
	req.TransferID = "t1"
	if _, err := planner.Plan(t.Context(), req); err != nil {
		t.Fatalf("first plan: %v", err)
	}
	h.place(h.blobsOf(img)...)
	h.exec(`DELETE FROM jobs WHERE transfer_id = 't1'`)

	h.transfer("t2")
	req.TransferID = "t2"
	if _, err := planner.Plan(t.Context(), req); err != nil {
		t.Fatalf("second plan: %v", err)
	}

	if got := h.stateOf(img); got != "pending" {
		t.Errorf("the image manifest is %q with every blob already at the destination, want pending",
			got)
	}
	// The index still waits: its child has not been PUSHED, only its bytes are
	// present, and an index naming a manifest the destination repository does
	// not hold is rejected MANIFEST_UNKNOWN.
	if got := h.stateOf(root); got != "blocked" {
		t.Errorf("the index is %q while its child has not been pushed, want blocked", got)
	}
}

// ---------------------------------------------------------------------------
// harness additions
// ---------------------------------------------------------------------------

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *harness) dependencyCount(digest string) int {
	h.t.Helper()
	return h.count(`SELECT COUNT(*) FROM job_dependencies d
	                  JOIN jobs j ON j.id = d.job_id
	                 WHERE j.digest = ?`, digest)
}

func (h *harness) stateOf(digest string) string {
	h.t.Helper()
	var state string
	if err := h.st.DB().QueryRowContext(h.t.Context(),
		`SELECT state FROM jobs WHERE digest = ?`, digest).Scan(&state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

// blobsOf reads the blobs an artifact references, from the record the walk
// wrote.
func (h *harness) blobsOf(digest string) []fakeregistry.Layer {
	h.t.Helper()

	rows, err := h.st.DB().QueryContext(h.t.Context(), `
		SELECT b.digest, b.size_bytes
		  FROM blobs b
		  JOIN artifact_blobs ab ON ab.digest = b.digest
		  JOIN package_artifacts a ON a.id = ab.artifact_id
		 WHERE a.digest = ?`, digest)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []fakeregistry.Layer
	for rows.Next() {
		var l fakeregistry.Layer
		if err := rows.Scan(&l.Digest, &l.Size); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	if len(out) == 0 {
		h.t.Fatalf("artifact %s has no recorded blobs; the fixture would prove nothing", digest)
	}
	return out
}

func (h *harness) place(blobs ...fakeregistry.Layer) {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, b := range blobs {
		if err := h.packages.RecordPlacement(h.t.Context(), tx, h.targetID,
			b.Digest, b.Size, "transferred"); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
}
