package store

import (
	"testing"
)

// What the dequeue hands out when several kinds of work are runnable at once.
//
// Per-artifact readiness means a manifest becomes runnable the moment its own
// content lands, while blobs belonging to other artifacts are still moving. So
// the two now COMPETE for the same lease slots, which they never did under the
// wave barrier, and the order between them is a scheduling decision nobody has
// made yet.

// A runnable manifest must not queue behind gigabytes of blobs.
//
// Ordering by size largest-first is right for blobs — it is what keeps a
// multi-gigabyte layer from running alone at the end — and it is exactly wrong
// for a manifest, which is a kilobyte and therefore sorts last however urgent
// it is. A manifest costs one round trip, moves no bandwidth worth counting,
// and UNBLOCKS the index above it. Making it wait for a queue of blobs delays
// the whole tree to save nothing.
func TestARunnableManifestIsNotQueuedBehindBlobs(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	// The shape at a wave boundary: one enormous blob still to move, and a
	// manifest whose own content has already landed.
	huge := h.jobRanked(id, "blob", 1, 0, 0, 8<<30)
	small := h.jobRanked(id, "blob", 2, 0, 0, 4<<20)
	manifest := h.jobRanked(id, "manifest", 3, 1, 0, 900)

	got := h.lease(3)
	if len(got) != 3 {
		t.Fatalf("leased %d jobs, want all three", len(got))
	}
	if got[0] != manifest {
		t.Errorf("dequeue order = %v; the manifest (%d) is behind %d bytes of blobs, "+
			"and it costs one round trip and unblocks the index above it",
			got, manifest, int64(8<<30)+int64(4<<20))
	}
	// And blobs keep their own order: largest first, which is what removes the
	// tail where one huge layer runs alone.
	if got[1] != huge || got[2] != small {
		t.Errorf("blob order = %v, want the largest first", got[1:])
	}
}

// The ordering must not let manifests starve blobs either. They cannot: a
// manifest is only runnable once its own content is present, so the supply is
// bounded by work that has already finished.
func TestManifestsDoNotStarveBlobs(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	for i := range 4 {
		h.jobRanked(id, "manifest", i+1, 1, 0, 900)
	}
	blob := h.jobRanked(id, "blob", 9, 0, 0, 1<<30)

	// A batch larger than the manifests means the blob is reached in the same
	// lease, so nothing waits on a round trip that is already in flight.
	got := h.lease(8)
	if len(got) != 5 {
		t.Fatalf("leased %d jobs, want all five", len(got))
	}
	if got[len(got)-1] != blob {
		t.Errorf("the blob was not leased alongside the manifests: %v", got)
	}
}

// Priority still outranks everything. An operator who raised a transfer's
// priority means it, and a kind-based rule that quietly overrode it would be a
// setting that does nothing.
func TestPriorityOutranksKind(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	urgent := h.insertJob(id, "blob", 1, 0, "pending", 0, 1<<20)
	h.exec(`UPDATE jobs SET priority = 900 WHERE id = ?`, urgent)
	ordinary := h.jobRanked(id, "manifest", 2, 1, 0, 900)

	got := h.lease(2)
	if len(got) != 2 || got[0] != urgent {
		t.Errorf("dequeue order = %v, want the priority-900 blob (%d) first", got, ordinary)
	}
}
