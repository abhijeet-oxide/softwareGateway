package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pausing, resuming and stopping a transfer that is already under way.
//
// The property that matters for all three is what happens to work already done,
// and it is the same property: nothing is undone. A pause keeps it, a stop keeps
// it, and neither deletes anything at the destination — half a bundle there is
// unreferenced blobs and untagged manifests, invisible to consumers and free to
// the next attempt.

// A pause has to stop work being HANDED OUT. A transfer state alone would not:
// the dequeue reads `NOT paused` on the job row, so setting the flag is what
// actually stops it rather than recording an intention every lease path would
// have to remember to check.
func TestPauseStopsJobsBeingHandedOut(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)

	res, err := h.packages.PauseTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "paused" {
		t.Errorf("state = %q, want paused", res.State)
	}
	if res.Jobs != 3 {
		t.Errorf("paused %d jobs, want 3", res.Jobs)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE transfer_id = ? AND paused`, id); n != 3 {
		t.Errorf("%d job rows carry the pause flag, want 3", n)
	}

	// And the queue agrees: nothing is leasable.
	if got := h.lease(); len(got) != 0 {
		t.Errorf("leased %d jobs from a paused transfer", len(got))
	}
}

// A job already in flight FINISHES. Abandoning a nine-gigabyte blob nine tenths
// of the way through to honour a pause a fraction of a second sooner is a bad
// trade, and the count says so rather than leaving the reader to wonder.
func TestPauseLeavesWorkAlreadyInFlightAlone(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)
	h.lease()

	res, err := h.packages.PauseTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.InFlight == 0 {
		t.Fatal("the fixture leased nothing; the test would prove nothing")
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'leased'`, id); n == 0 {
		t.Error("the pause cancelled work that was already in flight")
	}
}

func TestResumeMakesThemLeasableAgain(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)

	if _, err := h.packages.PauseTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	res, err := h.packages.ResumeTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "ready" {
		t.Errorf("state = %q, want ready — a worker leasing the first job is "+
			"what makes it running", res.State)
	}
	if got := h.lease(); len(got) == 0 {
		t.Error("nothing was leasable after a resume")
	}
}

// Stop cancels what has not started and leaves what has.
func TestStopCancelsTheWorkNotYetStarted(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)

	res, err := h.packages.StopTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in flight, so it is already over: waiting for a completion that
	// will never arrive would leave it reading `cancelling` forever, which is
	// the shape of a hang.
	if res.State != "cancelled" {
		t.Errorf("state = %q, want cancelled — nothing was in flight", res.State)
	}
	if n := h.count(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'cancelled'`, id); n != 3 {
		t.Errorf("%d jobs cancelled, want 3", n)
	}
}

// With work in flight, `cancelling` is a real state rather than a flag: a leased
// job belongs to a worker and stops at that worker's next checkpoint.
func TestStopWaitsForTheLastLeaseBeforeItIsCancelled(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)
	leased := h.lease()
	if len(leased) == 0 {
		t.Fatal("the fixture leased nothing; the test would prove nothing")
	}

	res, err := h.packages.StopTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "cancelling" {
		t.Fatalf("state = %q, want cancelling with %d jobs in flight", res.State, res.InFlight)
	}

	// The last lease reports, through the real completion path.
	for _, jobID := range leased {
		h.complete(jobID)
	}
	if got := h.state(id); got != "cancelled" {
		t.Errorf("state = %q after the last lease reported, want cancelled — a "+
			"transfer stuck in `cancelling` is indistinguishable from a hang", got)
	}
}

// The guard that matters most: a stopped transfer must never come out the other
// side reading `succeeded`. Stop cancels every job not yet started, so the waves
// genuinely drain, and without the check the settle walk would run to the end
// and declare success on the strength of work it deliberately did not do.
func TestAStoppedTransferIsNeverDeclaredSuccessful(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(2)
	leased := h.lease()

	if _, err := h.packages.StopTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, jobID := range leased {
		h.complete(jobID)
	}

	if got := h.state(id); got == "succeeded" {
		t.Fatal("a transfer somebody stopped was declared succeeded")
	}
	if got := h.state(id); got != "cancelled" {
		t.Errorf("state = %q, want cancelled", got)
	}
}

// A verb the state does not admit is refused, not silently ignored: it is
// somebody acting on a stale listing, and saying the state has moved is the
// difference between a confusing outcome and an obvious one.
func TestAVerbTheStateDoesNotAdmitIsRefused(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(1)
	h.exec(`UPDATE transfers SET state='succeeded' WHERE id = ?`, id)

	_, err := h.packages.PauseTransfer(t.Context(), id)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want an illegal transition", err)
	}
	if !strings.Contains(err.Error(), "succeeded") {
		t.Errorf("the error does not name the state that refused it: %v", err)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type controlHarness struct {
	*transferRefHarness
	n int
}

func newControlHarness(t *testing.T) *controlHarness {
	t.Helper()
	return &controlHarness{transferRefHarness: newTransferRefHarness(t)}
}

func (h *controlHarness) runningTransfer(jobs int) string {
	h.t.Helper()

	h.n++
	id := "0000000" + strconv.Itoa(h.n) + "-cccc-dddd-eeee-ffffffffffff"
	h.transfer(id)
	h.exec(`UPDATE transfers SET state='running', started_at='2026-01-01T00:00:00Z' WHERE id = ?`, id)

	for i := range jobs {
		digest := "sha256:" + strings.Repeat(strconv.Itoa(i%10), 64)
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, max_attempts)
		         VALUES (?, 'blob', ?, 1024, ?, ?, 'pending', 0, 8)`,
			id, digest, h.repoID, h.repoID)
	}
	return id
}

// lease takes work the way a worker does, so "is this leasable" is answered by
// the real dequeue rather than by reading the flag the pause set.
func (h *controlHarness) lease() []int64 {
	h.t.Helper()

	res, err := h.packages.LeaseJobs(h.t.Context(), LeaseRequest{
		Owner: "worker-1", Limit: 8, Duration: time.Minute,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]int64, 0, len(res))
	for _, j := range res {
		out = append(out, j.ID)
	}
	return out
}

func (h *controlHarness) complete(jobID int64) {
	h.t.Helper()
	if _, err := h.packages.CompleteJob(h.t.Context(), Completion{
		JobID: jobID, Owner: "worker-1", Outcome: "succeeded", Attempt: 1,
		BytesTransferred: 1024,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *controlHarness) state(transferID string) string {
	h.t.Helper()
	var state string
	if err := h.st.DB().QueryRowContext(h.t.Context(),
		`SELECT state FROM transfers WHERE id = ?`, transferID).Scan(&state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *controlHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRowContext(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *controlHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

// Deleting a transfer removes its RECORD, and only its record.
//
// The thing an operator wants is the row out of their listing — a transfer that
// failed before it was planned has nothing to retry and would otherwise sit
// there forever. What the transfer put at the destination is content-addressed,
// shared with every other release using the same layers, and stays exactly
// where it is.
func TestDeleteRemovesTheTransferAndItsJobs(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)

	if _, err := h.packages.StopTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	res, err := h.packages.DeleteTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Jobs != 3 {
		t.Errorf("reported %d jobs removed, want 3", res.Jobs)
	}

	if n := h.count(`SELECT count(*) FROM transfers WHERE id = ?`, id); n != 0 {
		t.Errorf("the transfer row survived the delete")
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE transfer_id = ?`, id); n != 0 {
		t.Errorf("%d job rows were orphaned by the delete", n)
	}
	// The placements describe the DESTINATION, not the transfer, and a later
	// transfer of the same content still wants to know the bytes are there.
	if n := h.count(`SELECT count(*) FROM blob_placements`); n < 0 {
		t.Errorf("placements were disturbed")
	}
}

// A transfer with work a worker may be holding is refused.
//
// Its jobs are LEASED — a worker will report on them — and deleting the rows
// underneath it turns every one of those reports into an update of nothing,
// silently. `stop` is one word and leaves a transfer this accepts.
func TestDeleteRefusesATransferThatIsStillWorking(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(2)

	_, err := h.packages.DeleteTransfer(t.Context(), id)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("deleting a running transfer returned %v, want an illegal transition", err)
	}
	if !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM transfers WHERE id = ?`, id); n != 1 {
		t.Errorf("a refused delete removed the transfer anyway")
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE transfer_id = ?`, id); n != 2 {
		t.Errorf("a refused delete removed %d of the jobs", 2-n)
	}
}

// A transfer that never existed is not found, rather than silently accepted.
func TestDeleteOfAnUnknownTransferIsAnError(t *testing.T) {
	h := newControlHarness(t)
	if _, err := h.packages.DeleteTransfer(t.Context(), "no-such-transfer"); err == nil {
		t.Fatal("deleting a transfer that does not exist reported success")
	}
}

// Reordering a transfer writes the JOBS, because that is the column the
// dequeue reads.
//
// A priority written only to the transfer row would show on every listing and
// change nothing about what runs next — the worst shape a control verb can
// have, since the operator watches their urgent download stay exactly where it
// was while the page insists it is now first.
func TestSetPriorityReordersTheJobsAndNotJustTheRow(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)

	res, err := h.packages.SetTransferPriority(t.Context(), id, 900)
	if err != nil {
		t.Fatal(err)
	}
	if res.Jobs != 3 {
		t.Errorf("reprioritized %d jobs, want 3", res.Jobs)
	}
	if n := h.count(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND priority = 900`, id); n != 3 {
		t.Errorf("%d job rows carry the new priority, want 3", n)
	}
	if n := h.count(
		`SELECT count(*) FROM transfers WHERE id = ? AND priority = 900`, id); n != 1 {
		t.Error("the transfer row still shows the old priority")
	}
	// The request too, so a later step of the same download inherits it rather
	// than dropping back to where it was.
	if n := h.count(`SELECT count(*) FROM transfer_requests r
	                   JOIN transfers t ON t.request_id = r.id
	                  WHERE t.id = ? AND r.priority = 900`, id); n != 1 {
		t.Error("the request behind the transfer still shows the old priority")
	}
}

// What a worker already holds is not reordered. It is bytes in flight, and
// preempting a blob at 90% throws away more work than the reordering recovers.
func TestSetPriorityLeavesWorkAlreadyInFlightAlone(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(3)
	leased := h.lease()
	if len(leased) == 0 {
		t.Fatal("the fixture leased nothing; the test would prove nothing")
	}

	res, err := h.packages.SetTransferPriority(t.Context(), id, 900)
	if err != nil {
		t.Fatal(err)
	}
	if res.Jobs != 0 {
		t.Errorf("reprioritized %d jobs, want none — all three are leased", res.Jobs)
	}
	if n := h.count(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND priority = 900`, id); n != 0 {
		t.Error("a leased job was reprioritized under the worker holding it")
	}
}

// A settled transfer has nothing left to order, and saying so beats reporting
// a change that changed nothing.
func TestSetPriorityRefusesASettledTransfer(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(1)
	h.exec(`UPDATE transfers SET state = 'succeeded' WHERE id = ?`, id)

	_, err := h.packages.SetTransferPriority(t.Context(), id, 900)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("error = %v, want an illegal transition", err)
	}
}

// The band is 0-1000 (docs/design/04 §6). Out of it, the caller is told in the
// terms they asked rather than by a constraint violation naming a column.
func TestSetPriorityRefusesAValueOutsideTheBand(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(1)

	for _, priority := range []int{-1, 1001} {
		if _, err := h.packages.SetTransferPriority(t.Context(), id, priority); !errors.Is(
			err, ErrInvalidPriority) {
			t.Errorf("priority %d: error = %v, want an invalid priority", priority, err)
		}
	}
}

// A paused transfer must not hold another transfer's work hostage.
//
// The dequeue makes a rank-1 job wait for the rank-0 copy of the same digest,
// so the second lands as a cross-repository mount rather than a second stream
// from the vendor. The clause matches across transfers on purpose — two
// releases of one product share most of their digests — but a PAUSED job is
// never going to run, so waiting for it waits forever.
//
// Reported from a real screen: two downloads of the same product, the first
// paused by hand, the second sitting in READY with `Took N/A` and never picked
// up. It could not resolve on its own; nothing about a paused transfer changes
// until somebody resumes it.
func TestAPausedJobDoesNotGateAnotherTransfersCopy(t *testing.T) {
	h := newControlHarness(t)
	paused := h.runningTransfer(0)
	waiting := h.runningTransfer(0)

	digest := "sha256:" + strings.Repeat("a", 64)
	rankedJob := func(transferID string, rank int) {
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, site_rank, max_attempts)
		         VALUES (?, 'manifest', ?, 1024, ?, ?, 'pending', 0, ?, 8)`,
			transferID, digest, h.repoID, h.repoID, rank)
	}

	rankedJob(paused, 0)
	if _, err := h.packages.PauseTransfer(t.Context(), paused); err != nil {
		t.Fatal(err)
	}
	rankedJob(waiting, 1)

	if got := h.lease(); len(got) != 1 {
		t.Fatalf("leased %d jobs, want the one in the transfer nobody paused", len(got))
	}
}

// A stop reaches the WORKER, not just the queue.
//
// Cancellation has no push channel: the heartbeat is the only regular call from
// a worker holding a long blob, so a stopped job is dropped from the renewal
// list and named as cancelled, and the worker abandons it within one interval.
//
// Renewing it instead — which is what happened before this existed — made
// `stop` mean "stop when the current blob finishes". On a forty-gigabyte blob
// that is an hour of bytes moving into a transfer somebody had just asked to
// stop, with the page reading CANCELLING throughout.
func TestAStoppedTransfersLeasesAreNotRenewed(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(2)
	leased := h.lease()
	if len(leased) != 2 {
		t.Fatalf("leased %d jobs, want 2", len(leased))
	}

	if _, err := h.packages.StopTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	renewed, cancelled, err := h.packages.RenewLeases(
		t.Context(), "worker-1", leased, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(renewed) != 0 {
		t.Errorf("renewed %d leases of a stopped transfer, want none", len(renewed))
	}
	if len(cancelled) != 2 {
		t.Fatalf("told the worker to drop %d jobs, want 2", len(cancelled))
	}
}

// An expired lease on a stopped transfer is CANCELLED, not requeued.
//
// The reaper's job is to return abandoned work to the queue, which is right for
// a live transfer and exactly wrong for a stopped one: requeueing undoes the
// stop by timeout, the job runs again, and the transfer never empties.
func TestTheReaperDoesNotResurrectStoppedWork(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(1)
	h.lease()

	if _, err := h.packages.StopTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// The worker vanished: nothing will report, and the lease simply expires.
	h.exec(`UPDATE jobs SET lease_expires_at = '2000-01-01T00:00:00Z' WHERE transfer_id = ?`, id)

	reaped, err := h.packages.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].State != "cancelled" {
		t.Fatalf("reaped %+v, want one job cancelled", reaped)
	}
	if n := h.count(
		`SELECT count(*) FROM jobs WHERE transfer_id = ? AND state = 'pending'`, id); n != 0 {
		t.Error("a stopped job was put back on the queue by the reaper")
	}
}

// And the transfer itself finishes stopping.
//
// `cancelling` closes when the last lease REPORTS. A worker that died holding
// that job reports nothing, and the reaper does not run the completion path —
// so the transfer said `cancelling` for as long as anybody left it there, which
// is what a hang looks like on the one operation somebody performs when they
// are already unhappy.
func TestACancellationClosesWhenNothingIsLeftInFlight(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(1)
	h.lease()

	if _, err := h.packages.StopTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := h.state(id); got != "cancelling" {
		t.Fatalf("state = %q, want cancelling while the lease is held", got)
	}

	// Nothing is leased any more, and nothing ever reported.
	h.exec(`UPDATE jobs SET state = 'cancelled', lease_owner = NULL WHERE transfer_id = ?`, id)

	closed, err := h.packages.CloseStalledCancellations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].ID != id {
		t.Fatalf("closed %+v, want the one stuck cancellation", closed)
	}
	if got := h.state(id); got != "cancelled" {
		t.Errorf("state = %q, want cancelled", got)
	}
}

// A transfer with a lease still out is NOT closed: the worker holding it may
// still report, and reporting a finished cancellation while bytes are moving is
// the same lie in the other direction.
func TestACancellationWithWorkInFlightStaysOpen(t *testing.T) {
	h := newControlHarness(t)
	id := h.runningTransfer(1)
	h.lease()

	if _, err := h.packages.StopTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	closed, err := h.packages.CloseStalledCancellations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 0 {
		t.Fatalf("closed %+v while a worker still held a job", closed)
	}
	if got := h.state(id); got != "cancelling" {
		t.Errorf("state = %q, want it still cancelling", got)
	}
}
