package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Per-artifact readiness, and the two things it must not break.
//
// The claim is narrow and worth stating exactly: a manifest becomes runnable
// when ITS OWN content is present, not when every blob in the transfer is.
// Invariant I1 is unchanged - a manifest still never precedes what it
// references - and these tests hold both ends of that at once.

// The headline. Under wave gating this was the measured failure: 1974 of 1976
// blobs done, and 517 manifests whose content was complete could not be
// written because two unrelated blobs had not landed.
func TestAManifestRunsAsSoonAsItsOwnBlobsAreDone(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	mine := h.job(id, "blob", 1, 0)
	theirs := h.job(id, "blob", 2, 0)
	manifest := h.job(id, "manifest", 3, 1)
	h.dependsOn(manifest, mine)

	h.succeed(mine)

	if got, _ := h.jobState(manifest); got != "pending" {
		t.Errorf("manifest is %q with its own blob done, want pending", got)
	}
	if got, _ := h.jobState(theirs); got != "pending" {
		t.Fatalf("the unrelated blob is %q; the fixture proves nothing if it finished too", got)
	}
}

// The other end of the same claim. A manifest pushed while one of its layers is
// missing is rejected BLOB_UNKNOWN by the registry, so this is the registry's
// precondition and not a preference.
func TestAManifestWaitsForEveryOneOfItsOwnBlobs(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	first := h.job(id, "blob", 1, 0)
	second := h.job(id, "blob", 2, 0)
	manifest := h.job(id, "manifest", 3, 1)
	h.dependsOn(manifest, first, second)

	h.succeed(first)

	if got, _ := h.jobState(manifest); got != "blocked" {
		t.Fatalf("manifest is %q with one of its two blobs outstanding, want blocked", got)
	}

	h.succeed(second)

	if got, _ := h.jobState(manifest); got != "pending" {
		t.Errorf("manifest is %q once both blobs landed, want pending", got)
	}
}

// A blob deduplicated away gets no job at all, so a manifest whose content is
// entirely already at the destination has no edges. It must be created runnable
// - waiting on an empty dependency set is waiting for an event that cannot
// come, and that is the deadlock OpenFirstWave exists to avoid for waves.
func TestAManifestWithNothingToWaitForIsNotBlocked(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	blob := h.job(id, "blob", 1, 0)
	// No edge from this manifest to anything: nothing it references needs
	// moving.
	free := h.jobInState(id, "manifest", 2, 1, "pending")

	if got, _ := h.jobState(free); got != "pending" {
		t.Errorf("a manifest with no dependencies is %q, want pending", got)
	}
	_ = blob
}

// Depth. An index waits for the child manifest that waits for the blob, and
// the middle one is what proves the promotion is not one level deep.
func TestPromotionReachesUpThroughTheTree(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	blob := h.job(id, "blob", 1, 0)
	child := h.job(id, "manifest", 2, 1)
	index := h.job(id, "manifest", 3, 2)
	h.dependsOn(child, blob)
	h.dependsOn(index, child)

	h.succeed(blob)

	if got, _ := h.jobState(child); got != "pending" {
		t.Fatalf("child manifest is %q after its blob landed, want pending", got)
	}
	if got, _ := h.jobState(index); got != "blocked" {
		t.Errorf("the index is %q while its child is only runnable, want blocked", got)
	}

	h.succeed(child)

	if got, _ := h.jobState(index); got != "pending" {
		t.Errorf("the index is %q once its child was pushed, want pending", got)
	}
}

// A blob found already at the destination satisfies a dependency exactly as a
// transferred one does. It is present, which is the only thing I1 asks.
func TestASkippedBlobSatisfiesWhatWaitsOnIt(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	blob := h.job(id, "blob", 1, 0)
	manifest := h.job(id, "manifest", 2, 1)
	h.dependsOn(manifest, blob)

	h.finish(blob, "skipped", "exists_at_target")

	if got, _ := h.jobState(manifest); got != "pending" {
		t.Errorf("manifest is %q after its blob was found already there, want pending", got)
	}
}

// Opening a wave is a bulk promotion by wave NUMBER, and the number is only a
// proxy for readiness. A replan whose lower wave holds a failed job leaves that
// wave out of "the first workable wave", so the wave above it would be opened
// over a dependency that never landed - which is invariant I1 broken by the
// mechanism that exists to enforce it.
func TestOpeningAWaveDoesNotPromoteOverAnUnmetDependency(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	broken := h.job(id, "blob", 1, 0)
	manifest := h.job(id, "manifest", 2, 1)
	h.dependsOn(manifest, broken)

	// The blob is out of the running, so wave 1 is the lowest wave with
	// anything left in it.
	h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE id = ?`, broken)

	tx, err := h.st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.packages.OpenFirstWave(t.Context(), tx, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if got, _ := h.jobState(manifest); got == "pending" {
		t.Error("a manifest was made runnable over a blob that never landed")
	}
}

// ---------------------------------------------------------------------------
// What per-artifact readiness makes WORSE, and the cascade that fixes it
// ---------------------------------------------------------------------------

// Under wave gating a permanent failure left everything blocked behind one
// wave, and the stall check saw a transfer with nothing runnable. With edges,
// the unaffected manifests run and finish, and what is left is a handful of
// jobs waiting on a dependency that will never arrive - which reads as "still
// working" unless the failure is propagated.
func TestAPermanentFailureFailsWhatWasWaitingOnIt(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	broken := h.job(id, "blob", 1, 0)
	manifest := h.job(id, "manifest", 2, 1)
	index := h.job(id, "manifest", 3, 2)
	h.dependsOn(manifest, broken)
	h.dependsOn(index, manifest)

	h.failJob(broken, 8)

	for _, tc := range []struct {
		name string
		id   int64
	}{{"manifest", manifest}, {"index", index}} {
		state, _ := h.jobState(tc.id)
		if state != "failed" {
			t.Errorf("%s is %q behind a permanently failed blob, want failed", tc.name, state)
		}
		if class := h.errorClassOf(tc.id); class != "dependency" {
			t.Errorf("%s failed with class %q, want it distinguishable from a real failure",
				tc.name, class)
		}
	}

	// And therefore the transfer says it has stopped, rather than reporting
	// `running` with nothing in flight - which is the whole point.
	if got := h.state(id); got != "failed" {
		t.Errorf("transfer state = %q with nothing able to run, want failed", got)
	}
}

// The sweep has to reach the same conclusion, because in a real outage nothing
// completes at all: the reaper fails the jobs and no completion is ever
// reported, so the inline cascade never runs.
func TestTheSweepPropagatesFailuresTheReaperProduced(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	broken := h.job(id, "blob", 1, 0)
	manifest := h.job(id, "manifest", 2, 1)
	h.dependsOn(manifest, broken)

	// Exactly what ReapExpiredLeases leaves behind: failed, with no completion.
	h.exec(`UPDATE jobs SET state='failed', attempts=8, last_error='lease expired',
	                        last_error_class='lease_expired' WHERE id = ?`, broken)

	if got := h.state(id); got == "failed" {
		t.Fatal("the fixture settled the transfer by itself; the test would prove nothing")
	}

	if _, err := h.packages.SettleStalledTransfers(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got, _ := h.jobState(manifest); got != "failed" {
		t.Errorf("the dependent manifest is %q after the sweep, want failed", got)
	}
	if got := h.state(id); got != "failed" {
		t.Errorf("transfer state = %q after the sweep, want failed", got)
	}
}

// A retry requeues what actually broke. What failed only BECAUSE of it goes
// back to waiting - promoting it would push a manifest whose blob is once
// again in flight.
func TestRetryReturnsCascadeFailuresToWaitingRatherThanRunning(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	broken := h.job(id, "blob", 1, 0)
	manifest := h.job(id, "manifest", 2, 1)
	h.dependsOn(manifest, broken)
	h.failJob(broken, 8)

	res, err := h.packages.RetryTransfer(t.Context(), id)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Requeued != 1 {
		t.Errorf("requeued %d, want the 1 job that actually failed", res.Requeued)
	}
	if res.Reblocked != 1 {
		t.Errorf("reblocked %d, want the 1 that failed only as a consequence", res.Reblocked)
	}

	if got, _ := h.jobState(broken); got != "pending" {
		t.Errorf("the blob is %q after a retry, want pending", got)
	}
	if got, _ := h.jobState(manifest); got != "blocked" {
		t.Errorf("the manifest is %q after a retry, want blocked - its blob has not run yet", got)
	}

	// And it still promotes when the blob comes back.
	h.succeed(broken)
	if got, _ := h.jobState(manifest); got != "pending" {
		t.Errorf("the manifest is %q after the requeued blob succeeded, want pending", got)
	}
}

// ---------------------------------------------------------------------------
// Dequeue order
// ---------------------------------------------------------------------------

// A component published under its own name is the SAME digest already going to
// a sibling repository. Running the second copy after the first turns a
// wide-area transfer into a cross-repository mount, and the ordering is what
// guarantees it rather than insertion order happening to separate them.
func TestTheDequeueRunsTheResolvableCopyBeforeThePublishedOne(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	published := h.jobRanked(id, "blob", 1, 0, 1, 1024)
	resolvable := h.jobRanked(id, "blob", 2, 0, 0, 1024)

	got := h.lease(2)
	if len(got) != 2 {
		t.Fatalf("leased %d jobs, want both", len(got))
	}
	if got[0] != resolvable || got[1] != published {
		t.Errorf("dequeue order = %v, want the rank-0 copy (%d) before the rank-1 copy (%d)",
			got, resolvable, published)
	}
}

// Within a rank, largest first. Insertion order is arbitrary with respect to
// size, so a multi-gigabyte layer leased last runs alone while every other slot
// idles - and that tail is what this ordering removes.
func TestTheDequeueRunsTheLargestJobFirstWithinARank(t *testing.T) {
	h := newDepHarness(t)
	id := h.transferWithJobs(0)

	small := h.jobRanked(id, "blob", 1, 0, 0, 1024)
	huge := h.jobRanked(id, "blob", 2, 0, 0, 8<<30)
	medium := h.jobRanked(id, "blob", 3, 0, 0, 1<<20)

	got := h.lease(3)
	want := []int64{huge, medium, small}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("dequeue order = %v, want largest first %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type depHarness struct {
	*recoveryHarness
	digest int
}

func newDepHarness(t *testing.T) *depHarness {
	t.Helper()
	return &depHarness{recoveryHarness: newRecoveryHarness(t)}
}

// job inserts one job. Manifests start blocked, blobs pending - which is what
// the planner does for anything with dependencies.
func (h *depHarness) job(transferID, kind string, n, wave int) int64 {
	state := "pending"
	if kind == "manifest" {
		state = "blocked"
	}
	return h.jobInState(transferID, kind, n, wave, state)
}

func (h *depHarness) jobInState(transferID, kind string, n, wave int, state string) int64 {
	h.t.Helper()
	return h.insertJob(transferID, kind, n, wave, state, 0, 1024)
}

func (h *depHarness) jobRanked(transferID, kind string, n, wave, rank int, size int64) int64 {
	h.t.Helper()
	return h.insertJob(transferID, kind, n, wave, "pending", rank, size)
}

func (h *depHarness) insertJob(
	transferID, kind string, n, wave int, state string, rank int, size int64,
) int64 {
	h.t.Helper()

	h.digest++
	digest := "sha256:" + strings.Repeat(strconv.Itoa(n%10), 63) + strconv.Itoa(h.digest%10)

	res, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id, target_repo_id,
		                   state, wave, site_rank, attempts, max_attempts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 8)`,
		transferID, kind, digest, size, h.repoID, h.repoID, state, wave, rank)
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *depHarness) dependsOn(job int64, deps ...int64) {
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

// succeed completes a job through the REAL path, so the promotion under test
// is the one a worker's report triggers.
func (h *depHarness) succeed(jobID int64) {
	h.t.Helper()
	h.finish(jobID, "succeeded", "")
}

func (h *depHarness) finish(jobID int64, outcome, skipReason string) {
	h.t.Helper()

	h.exec(`UPDATE jobs SET state='leased', lease_owner='worker-1', attempts=1 WHERE id = ?`, jobID)
	if _, err := h.packages.CompleteJob(h.t.Context(), Completion{
		JobID: jobID, Owner: "worker-1", Outcome: outcome, SkipReason: skipReason,
		Attempt: 1, Placed: true,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *depHarness) errorClassOf(jobID int64) string {
	h.t.Helper()
	var class string
	if err := h.query(`SELECT COALESCE(last_error_class,'') FROM jobs WHERE id = ?`, jobID).
		Scan(&class); err != nil {
		h.t.Fatal(err)
	}
	return class
}

func (h *depHarness) lease(limit int) []int64 {
	h.t.Helper()

	jobs, err := h.packages.LeaseJobs(h.t.Context(), LeaseRequest{
		Owner: "worker-1", Limit: limit, Duration: time.Minute,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}
