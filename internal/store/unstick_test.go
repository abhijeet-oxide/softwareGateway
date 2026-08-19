package store

import (
	"strings"
	"testing"
)

// The deadlock, exactly as it happened: 31 hours in, every blob done, nothing
// running, nothing runnable, nothing failed, and 207 manifests sitting in
// `blocked` with no way out.

func TestATransferWithNothingRunnableUnsticksItself(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	// A transfer planned BEFORE dependency edges existed: manifests gated by
	// wave alone, and not one edge in the table.
	blob := h.blobJobFor(id, "sha256:"+strings.Repeat("a", 64))
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id = ?`, blob)

	stuck := h.manifestJob(id, 1)
	h.exec(`UPDATE jobs SET state='blocked', wave=1 WHERE id = ?`, stuck)
	// Its wave is open - the watermark passed it long ago, which is what makes
	// it provably runnable.
	h.exec(`UPDATE transfers SET current_wave=1, state='running' WHERE id = ?`, id)

	unstuck, err := h.packages.UnstickTransfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(unstuck) != 1 || unstuck[0].ID != id {
		t.Fatalf("unstuck %+v, want the one deadlocked transfer", unstuck)
	}
	if unstuck[0].Promoted != 1 {
		t.Errorf("promoted %d, want the 1 blocked manifest", unstuck[0].Promoted)
	}
	if state, _ := h.jobState(stuck); state != "pending" {
		t.Errorf("the manifest is %q after unsticking, want pending", state)
	}
}

// A transfer with work in flight is not stuck, it is WORKING. Promoting its
// blocked jobs would push manifests whose content is still being uploaded -
// which is invariant I1 broken by the mechanism meant to protect availability.
func TestATransferWithWorkInFlightIsLeftAlone(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	running := h.blobJobFor(id, "sha256:"+strings.Repeat("b", 64))
	h.exec(`UPDATE jobs SET state='leased', lease_owner='worker-1' WHERE id = ?`, running)

	waiting := h.manifestJob(id, 1)
	h.exec(`UPDATE jobs SET state='blocked', wave=1 WHERE id = ?`, waiting)
	h.exec(`UPDATE transfers SET current_wave=1, state='running' WHERE id = ?`, id)

	unstuck, err := h.packages.UnstickTransfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(unstuck) != 0 {
		t.Errorf("unstuck %+v; a transfer with a job in flight is working, not stuck", unstuck)
	}
	if state, _ := h.jobState(waiting); state != "blocked" {
		t.Errorf("a manifest was promoted while its content was still uploading (%q)", state)
	}
}

// The wave watermark is the guarantee that a job's content has landed. A job
// ABOVE it has not had its lower waves drained, so promoting it would push a
// manifest naming things that are not there.
func TestUnstickingWillNotReachAboveTheWaveWatermark(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	done := h.blobJobFor(id, "sha256:"+strings.Repeat("c", 64))
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id = ?`, done)

	high := h.manifestJob(id, 1)
	h.exec(`UPDATE jobs SET state='blocked', wave=3 WHERE id = ?`, high)
	h.exec(`UPDATE transfers SET current_wave=1, state='running' WHERE id = ?`, id)

	if _, err := h.packages.UnstickTransfers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state, _ := h.jobState(high); state != "blocked" {
		t.Errorf("a wave-3 job was promoted at watermark 1 (%q); its content has "+
			"not been proven present", state)
	}
}

// And an outstanding dependency still means blocked, watermark or no watermark.
// The wave test is a floor, not a replacement for invariant I1.
func TestUnstickingRespectsAnOutstandingDependency(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	artifact, digest := h.seedArtifactWithBlob(id)
	blob := h.blobJobFor(id, digest)
	manifest := h.manifestJobForArtifact(id, artifact)
	h.dependsOn(manifest, blob)

	// The blob is failed, so it is neither runnable nor satisfied - exactly the
	// shape that makes a transfer look stuck when it is genuinely broken.
	h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE id = ?`, blob)
	h.exec(`UPDATE jobs SET state='blocked', wave=1 WHERE id = ?`, manifest)
	h.exec(`UPDATE transfers SET current_wave=1, state='running' WHERE id = ?`, id)

	if _, err := h.packages.UnstickTransfers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state, _ := h.jobState(manifest); state != "blocked" {
		t.Errorf("a manifest was promoted over a blob that failed (%q)", state)
	}
}

func (h *failureHarness) dependsOn(job int64, deps ...int64) {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	edges := make([]JobEdge, 0, len(deps))
	for _, d := range deps {
		edges = append(edges, JobEdge{JobID: job, DependsOnID: d})
	}
	if err := h.packages.AddJobDependencies(h.t.Context(), tx, edges); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
}
