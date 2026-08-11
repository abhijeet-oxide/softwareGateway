package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// What happens to a transfer when the network goes, and how it starts again.
//
// The first assertion here is the one that was missing from the system
// entirely: a transfer whose jobs have all given up must SAY so. It reported
// `running` forever instead, with nothing running, which is the failure mode
// that hides an outage from the person watching for one.

func TestATransferWhoseJobsHaveAllGivenUpIsReportedFailed(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(2)

	h.exhaust(id)

	if got := h.state(id); got != "failed" {
		t.Errorf("transfer state = %q after every job failed, want failed", got)
	}
	reason := h.failureReasonOf(id)
	// The reason has to be actionable on its own. A bare "failed" sends the
	// reader to the jobs table to learn anything at all.
	if !strings.Contains(reason, "2 job(s) failed") {
		t.Errorf("reason %q does not say how much failed", reason)
	}
	if !strings.Contains(reason, "connection reset") {
		t.Errorf("reason %q does not carry what the registry actually said", reason)
	}
}

// A transfer mid-backoff is WAITING, not finished, and reporting it as failed
// would be worse than the bug this fixes: an operator would retry work that was
// about to retry itself.
func TestATransferStillRetryingIsNotReportedFailed(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(2)

	// One job exhausts its attempts; the other still has budget, so it goes
	// back to pending behind a backoff.
	h.failJob(h.jobIDs(id)[0], 8)
	h.failJob(h.jobIDs(id)[1], 1)

	if got := h.state(id); got == "failed" {
		t.Error("a transfer with a job still in retry backoff was declared failed")
	}

	if _, err := h.packages.SettleStalledTransfers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.state(id); got == "failed" {
		t.Errorf("the sweep declared a transfer failed while a job was still waiting to retry")
	}
}

// The case that matters most, and the one the completion path cannot see. In a
// real outage the workers stop answering: nothing completes, the leases expire,
// and the reaper fails the jobs with no completion ever reported.
func TestTheSweepCatchesFailuresTheReaperProduced(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(3)

	// Exactly what ReapExpiredLeases leaves behind for a job out of attempts.
	for _, jobID := range h.jobIDs(id) {
		h.exec(`UPDATE jobs SET state='failed', attempts=8, last_error='lease expired',
		                        last_error_class='lease_expired' WHERE id = ?`, jobID)
	}
	if got := h.state(id); got == "failed" {
		t.Fatal("the fixture failed the transfer by itself; the test would prove nothing")
	}

	stalled, err := h.packages.SettleStalledTransfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 1 || stalled[0].ID != id {
		t.Fatalf("swept %+v, want the one stalled transfer", stalled)
	}
	if stalled[0].Failed != 3 {
		t.Errorf("reported %d failed jobs, want 3", stalled[0].Failed)
	}
	if got := h.state(id); got != "failed" {
		t.Errorf("transfer state = %q, want failed", got)
	}
}

// A succeeded transfer must never be touched by the sweep, whatever its job
// rows say.
func TestTheSweepLeavesSettledTransfersAlone(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(1)
	h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE transfer_id = ?`, id)
	h.exec(`UPDATE transfers SET state='cancelled' WHERE id = ?`, id)

	if _, err := h.packages.SettleStalledTransfers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.state(id); got != "cancelled" {
		t.Errorf("state = %q, want the cancellation to stand", got)
	}
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

func TestRetryRequeuesFailedJobsAndReopensTheTransfer(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(3)

	jobs := h.jobIDs(id)
	h.exhaust(id)
	// One job had already succeeded before the outage. It must NOT be
	// requeued: re-copying what is already at the destination is the waste
	// this whole system exists to avoid.
	h.exec(`UPDATE jobs SET state='succeeded', attempts=1, last_error=NULL WHERE id = ?`, jobs[0])

	res, err := h.packages.RetryTransfer(t.Context(), id)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Requeued != 2 {
		t.Errorf("requeued %d job(s), want the 2 that failed", res.Requeued)
	}
	if res.State != "ready" {
		t.Errorf("transfer state = %q, want ready — nothing is in flight yet", res.State)
	}
	if got := h.state(id); got != "ready" {
		t.Errorf("stored state = %q, want ready", got)
	}
	if got := h.failureReasonOf(id); got != "" {
		t.Errorf("failure reason %q survived the retry", got)
	}

	// The attempt budget is per outage, not per lifetime: a job that used its
	// eight attempts against a dead network must not come back with none.
	for _, jobID := range jobs[1:] {
		state, attempts := h.jobState(jobID)
		if state != "pending" {
			t.Errorf("job %d is %q, want pending", jobID, state)
		}
		if attempts != 0 {
			t.Errorf("job %d came back with %d attempts already spent", jobID, attempts)
		}
	}
	if state, _ := h.jobState(jobs[0]); state != "succeeded" {
		t.Errorf("the succeeded job became %q; a retry must not redo finished work", state)
	}
}

// THE RETRY RESUMES, IT DOES NOT RESTART. A blob that was partway through when
// the network went keeps its byte count, so the progress an operator watches
// picks up where it stopped rather than dropping back to zero.
func TestRetryKeepsWhatWasAlreadyTransferred(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(1)
	jobID := h.jobIDs(id)[0]

	h.exec(`UPDATE jobs SET state='failed', attempts=8, bytes_transferred=4096 WHERE id = ?`, jobID)

	if _, err := h.packages.RetryTransfer(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var bytes int64
	h.query(`SELECT bytes_transferred FROM jobs WHERE id = ?`, jobID).Scan(&bytes)
	if bytes != 4096 {
		t.Errorf("bytes_transferred = %d after a retry, want the 4096 already moved", bytes)
	}
}

// Idempotent: a client whose request timed out and retried must not be
// punished, and a running transfer must not be knocked back to `ready` by a
// retry that had nothing to do.
func TestRetryWithNothingFailedChangesNothing(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(2)
	h.exec(`UPDATE transfers SET state='running' WHERE id = ?`, id)

	res, err := h.packages.RetryTransfer(t.Context(), id)
	if err != nil {
		t.Fatalf("retry of a healthy transfer errored: %v", err)
	}
	if res.Requeued != 0 {
		t.Errorf("requeued %d job(s) from a transfer with no failures", res.Requeued)
	}
	if got := h.state(id); got != "running" {
		t.Errorf("state = %q, want the transfer left running", got)
	}
}

// A cancellation is somebody's decision and a retry must not undo it.
func TestRetryRefusesSettledTransfers(t *testing.T) {
	h := newRecoveryHarness(t)

	for _, state := range []string{"succeeded", "cancelled"} {
		id := h.transferWithJobs(1)
		h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE transfer_id = ?`, id)
		h.exec(`UPDATE transfers SET state=? WHERE id = ?`, state, id)

		if _, err := h.packages.RetryTransfer(t.Context(), id); err == nil {
			t.Errorf("a %s transfer was retried", state)
		}
		if got := h.state(id); got != state {
			t.Errorf("state = %q, want %q", got, state)
		}
	}
}

// A transfer that failed during PLANNING has no jobs, so there is nothing to
// requeue. Reporting that as a successful retry of zero jobs would leave the
// operator waiting for work that will never be leased.
func TestRetryOfATransferThatNeverPlannedSaysSo(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transfer("11111111-2222-3333-4444-555555555555")
	h.exec(`UPDATE transfers SET state='failed', failure_reason='could not resolve the manifest'
	         WHERE id = ?`, id)

	res, err := h.packages.RetryTransfer(t.Context(), id)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !res.NoJobs {
		t.Error("a transfer with no jobs was not flagged as having none")
	}
	if got := h.state(id); got != "failed" {
		t.Errorf("state = %q, want it left failed", got)
	}
}

// The fleet-wide case: an outage fails everything that was running, and the
// listing has to find them all.
func TestRetryableListsEveryTransferWithAFailedJob(t *testing.T) {
	h := newRecoveryHarness(t)

	broken := h.transferWithJobs(1)
	h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE transfer_id = ?`, broken)

	healthy := h.transferWithJobs(1)

	done := h.transferWithJobs(1)
	h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE transfer_id = ?`, done)
	h.exec(`UPDATE transfers SET state='succeeded' WHERE id = ?`, done)

	ids, err := h.packages.RetryableTransfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != broken {
		t.Errorf("retryable = %v, want only %s", ids, broken)
	}
	_ = healthy
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type recoveryHarness struct {
	*transferRefHarness
	n int
}

func newRecoveryHarness(t *testing.T) *recoveryHarness {
	t.Helper()
	return &recoveryHarness{transferRefHarness: newTransferRefHarness(t)}
}

// transferWithJobs creates a transfer and n blob jobs in wave 0, leased.
//
// Leased, because that is the state an outage interrupts: the worker took the
// work and then stopped answering.
func (h *recoveryHarness) transferWithJobs(n int) string {
	h.t.Helper()

	h.n++
	id := "0000000" + strconv.Itoa(h.n) + "-1111-2222-3333-444444444444"
	h.transfer(id)
	h.exec(`UPDATE transfers SET state='running', started_at='2026-01-01T00:00:00Z' WHERE id = ?`, id)

	for i := range n {
		digest := "sha256:" + strings.Repeat(strconv.Itoa(i%10), 64)
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, attempts, max_attempts, lease_owner,
		                          lease_expires_at)
		         VALUES (?, 'blob', ?, 1024, ?, ?, 'leased', 0, 1, 8, 'worker-1',
		                 '2026-01-01T00:00:00Z')`,
			id, digest, h.repoID, h.repoID)
	}
	return id
}

func (h *recoveryHarness) jobIDs(transferID string) []int64 {
	h.t.Helper()

	rows, err := h.st.DB().QueryContext(h.t.Context(),
		`SELECT id FROM jobs WHERE transfer_id = ? ORDER BY id`, transferID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// exhaust completes every job of a transfer as a failure with its attempts
// spent, through the REAL completion path — so the test exercises the same code
// a worker's report does.
func (h *recoveryHarness) exhaust(transferID string) {
	h.t.Helper()
	for _, id := range h.jobIDs(transferID) {
		h.failJob(id, 8)
	}
}

// failJob reports a failure for one job at a given attempt count.
func (h *recoveryHarness) failJob(jobID int64, attempts int) {
	h.t.Helper()

	// The lease is what CompleteJob checks ownership against, and attempts are
	// incremented at lease time in production.
	h.exec(`UPDATE jobs SET state='leased', lease_owner='worker-1', attempts=? WHERE id = ?`,
		attempts, jobID)

	if _, err := h.packages.CompleteJob(h.t.Context(), Completion{
		JobID: jobID, Owner: "worker-1", Outcome: "failed", Attempt: attempts,
		ErrorMsg: "connection reset by peer", ErrorClass: "unavailable",
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *recoveryHarness) state(transferID string) string {
	h.t.Helper()
	var state string
	if err := h.query(`SELECT state FROM transfers WHERE id = ?`, transferID).Scan(&state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *recoveryHarness) failureReasonOf(transferID string) string {
	h.t.Helper()
	var reason string
	if err := h.query(`SELECT COALESCE(failure_reason,'') FROM transfers WHERE id = ?`,
		transferID).Scan(&reason); err != nil {
		h.t.Fatal(err)
	}
	return reason
}

func (h *recoveryHarness) jobState(jobID int64) (string, int) {
	h.t.Helper()
	var (
		state    string
		attempts int
	)
	if err := h.query(`SELECT state, attempts FROM jobs WHERE id = ?`, jobID).
		Scan(&state, &attempts); err != nil {
		h.t.Fatal(err)
	}
	return state, attempts
}

func (h *recoveryHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *recoveryHarness) query(query string, args ...any) *rowScanner {
	return &rowScanner{row: h.st.DB().QueryRowContext(context.Background(), query, args...)}
}

type rowScanner struct {
	row interface{ Scan(...any) error }
}

func (r *rowScanner) Scan(dest ...any) error { return r.row.Scan(dest...) }
