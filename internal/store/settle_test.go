package store

import (
	"strconv"
	"strings"
	"testing"
)

// A transfer with failed jobs is not a successful transfer.
//
// # What was reported, and why it could not be acted on
//
//	STATE      DONE  JOBS       FAILED
//	succeeded  100%  2486/2489  3
//
// Three jobs failed, the transfer said it succeeded, and `transfers retry`
// answered "there is nothing to retry" - because a settled transfer is not a
// retry candidate, by design. So the three failures were simultaneously
// reported, unexplained and unfixable.
//
// # How the walk misses it
//
// Settling asks two questions, and neither is "did anything fail". `waveDrained`
// is asked about ONE wave, the one the completing job belongs to.
// `waveOccupied` counts only work that can still move - pending, blocked,
// leased - so a wave whose remaining jobs have exhausted their attempts reads
// as empty. The advance loop walks past it, runs out of waves, and falls into
// the branch that finishes the transfer.
//
// Per-artifact readiness is what makes that the normal case rather than a
// corner: a manifest becomes runnable the moment its own content lands, well
// before the wave it belongs to opens. By the time a lower wave formally
// drains, the waves above it are routinely already terminal - including
// whatever failed in them.

func TestATransferWithFailedJobsAboveTheCurrentWaveIsNotSucceeded(t *testing.T) {
	h := newSettleHarness(t)
	id := h.transferAcrossWaves(2)

	// The shape per-artifact readiness produces: a manifest two waves up ran
	// early, failed terminally, and is sitting there while wave 0 finishes.
	h.failTerminally(h.jobInWave(id, 2))

	// The last blob of wave 0 succeeds, through the real completion path.
	h.succeed(h.jobInWave(id, 0))

	if got := h.state(id); got == "succeeded" {
		t.Fatal("a transfer with a permanently failed job was declared succeeded; " +
			"retry then refuses it as settled, so the failure can be neither " +
			"explained nor fixed")
	}
	if got := h.state(id); got != "failed" {
		t.Errorf("transfer state = %q, want failed", got)
	}

	// And it must say what failed. A `failed` with no reason sends the reader
	// to the jobs table to learn anything at all.
	if reason := h.failureReasonOf(id); !strings.Contains(reason, "1 job(s) failed") {
		t.Errorf("failure reason %q does not say how much failed", reason)
	}
}

// The other half, and the one a regression would break silently: a transfer
// where nothing failed still succeeds.
func TestATransferWithNothingFailedStillSucceeds(t *testing.T) {
	h := newSettleHarness(t)
	id := h.transferAcrossWaves(2)

	h.succeed(h.jobInWave(id, 2))
	h.succeed(h.jobInWave(id, 0))

	if got := h.state(id); got != "succeeded" {
		t.Errorf("transfer state = %q, want succeeded", got)
	}
	if reason := h.failureReasonOf(id); reason != "" {
		t.Errorf("a succeeded transfer carries failure reason %q", reason)
	}
}

// Once it says `failed`, retry has something to act on. This is the whole point
// of the fix: the state word is not cosmetic, it is what gates recovery.
func TestTheFailedStateIsWhatMakesTheRetryPossible(t *testing.T) {
	h := newSettleHarness(t)
	id := h.transferAcrossWaves(2)

	h.failTerminally(h.jobInWave(id, 2))
	h.succeed(h.jobInWave(id, 0))

	res, err := h.packages.RetryTransfer(t.Context(), id)
	if err != nil {
		t.Fatalf("retry refused a transfer with a failed job: %v", err)
	}
	if res.Requeued != 1 {
		t.Errorf("retry requeued %d jobs, want the 1 that failed", res.Requeued)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type settleHarness struct {
	*transferRefHarness
	n int
}

func newSettleHarness(t *testing.T) *settleHarness {
	t.Helper()
	return &settleHarness{transferRefHarness: newTransferRefHarness(t)}
}

// transferAcrossWaves creates a transfer with one leased job in wave 0 and one
// in the top wave, and nothing in between.
//
// The gap is deliberate: it is what makes the advance loop walk rather than
// stop, which is the path the failure hides in.
func (h *settleHarness) transferAcrossWaves(maxWave int) string {
	h.t.Helper()

	h.n++
	id := "0000000" + strconv.Itoa(h.n) + "-aaaa-bbbb-cccc-dddddddddddd"
	h.transfer(id)
	h.exec(`UPDATE transfers SET state='running', current_wave=0, max_wave=?,
	                             started_at='2026-01-01T00:00:00Z' WHERE id = ?`, maxWave, id)

	for _, wave := range []int{0, maxWave} {
		digest := "sha256:" + strings.Repeat(strconv.Itoa(wave), 64)
		kind := "blob"
		if wave > 0 {
			kind = "manifest"
		}
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, attempts, max_attempts,
		                          lease_owner, lease_expires_at)
		         VALUES (?, ?, ?, 1024, ?, ?, 'leased', ?, 1, 8, 'worker-1',
		                 '2026-01-01T00:00:00Z')`,
			id, kind, digest, h.repoID, h.repoID, wave)
	}
	return id
}

func (h *settleHarness) jobInWave(transferID string, wave int) int64 {
	h.t.Helper()
	var id int64
	if err := h.query(`SELECT id FROM jobs WHERE transfer_id = ? AND wave = ?`,
		transferID, wave).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// failTerminally spends a job's whole attempt budget, through the completion
// path a worker's report takes.
func (h *settleHarness) failTerminally(jobID int64) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET state='leased', lease_owner='worker-1', attempts=8 WHERE id = ?`, jobID)
	if _, err := h.packages.CompleteJob(h.t.Context(), Completion{
		JobID: jobID, Owner: "worker-1", Outcome: "failed", Attempt: 8,
		ErrorMsg: "unauthorized", ErrorClass: "auth",
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *settleHarness) succeed(jobID int64) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET state='leased', lease_owner='worker-1', attempts=1 WHERE id = ?`, jobID)
	if _, err := h.packages.CompleteJob(h.t.Context(), Completion{
		JobID: jobID, Owner: "worker-1", Outcome: "succeeded", Attempt: 1,
		BytesTransferred: 1024,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *settleHarness) state(transferID string) string {
	h.t.Helper()
	var state string
	if err := h.query(`SELECT state FROM transfers WHERE id = ?`, transferID).Scan(&state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *settleHarness) failureReasonOf(transferID string) string {
	h.t.Helper()
	var reason string
	if err := h.query(`SELECT COALESCE(failure_reason,'') FROM transfers WHERE id = ?`,
		transferID).Scan(&reason); err != nil {
		h.t.Fatal(err)
	}
	return reason
}

func (h *settleHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *settleHarness) query(query string, args ...any) *rowScanner {
	return &rowScanner{row: h.st.DB().QueryRowContext(h.t.Context(), query, args...)}
}
