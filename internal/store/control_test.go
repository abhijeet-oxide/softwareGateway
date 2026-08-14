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
