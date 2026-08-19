package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Keeping the database bounded, and keeping the catalogue.
//
// The two halves are equally load-bearing. `jobs` grows with USE - 2500 rows per
// transfer, one transfer per release per target, forever - and nothing was ever
// removing it. Everything else grows with the CATALOGUE, is the answer to "what
// does this vendor publish", and must survive every sweep.

func TestSettledTransfersExpireAndTakeTheirJobsWithThem(t *testing.T) {
	h := newRetentionHarness(t)
	old := h.settledTransfer("succeeded", 120*24*time.Hour, 3)

	res, err := h.packages.SweepRetention(t.Context(), RetentionPolicy{
		Transfers: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfers != 1 {
		t.Errorf("removed %d transfers, want 1", res.Transfers)
	}
	if n := h.count(`SELECT count(*) FROM transfers WHERE id = ?`, old); n != 0 {
		t.Error("the transfer is still there")
	}
	// Through the foreign key, not through a second statement: a sweep that
	// removed transfers and left their jobs would leave the largest table
	// growing exactly as before.
	if n := h.count(`SELECT count(*) FROM jobs WHERE transfer_id = ?`, old); n != 0 {
		t.Errorf("%d job rows outlived the transfer they belong to", n)
	}
}

// A transfer still RUNNING is one somebody is watching, whatever its age.
// Deleting it would look exactly like the data loss this sweep exists to avoid
// being blamed for.
func TestAnUnsettledTransferIsNeverSwept(t *testing.T) {
	h := newRetentionHarness(t)
	running := h.runningTransferAged(365 * 24 * time.Hour)

	if _, err := h.packages.SweepRetention(t.Context(), RetentionPolicy{
		Transfers: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM transfers WHERE id = ?`, running); n != 1 {
		t.Error("a transfer that has not settled was deleted for being old")
	}
}

// Inside the retention window nothing moves.
func TestARecentTransferIsKept(t *testing.T) {
	h := newRetentionHarness(t)
	recent := h.settledTransfer("succeeded", time.Hour, 2)

	res, err := h.packages.SweepRetention(t.Context(), RetentionPolicy{
		Transfers: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfers != 0 {
		t.Errorf("removed %d transfers inside the retention window", res.Transfers)
	}
	if n := h.count(`SELECT count(*) FROM transfers WHERE id = ?`, recent); n != 1 {
		t.Error("a transfer inside the retention window was deleted")
	}
}

// THE CATALOGUE SURVIVES. It is what the system is for, it grows with the
// vendor rather than with use, and a sweep that took it would be indefensible.
func TestTheCatalogueOutlivesEveryTransfer(t *testing.T) {
	h := newRetentionHarness(t)
	h.settledTransfer("succeeded", 365*24*time.Hour, 4)

	packagesBefore := h.count(`SELECT count(*) FROM packages`)
	reposBefore := h.count(`SELECT count(*) FROM repositories`)

	if _, err := h.packages.SweepRetention(t.Context(), RetentionPolicy{
		Transfers: time.Hour, WorkerLogs: time.Hour, AuditEvents: time.Hour,
		Placements: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}

	if got := h.count(`SELECT count(*) FROM packages`); got != packagesBefore {
		t.Errorf("packages went from %d to %d; the catalogue is not history",
			packagesBefore, got)
	}
	if got := h.count(`SELECT count(*) FROM repositories`); got != reposBefore {
		t.Errorf("repositories went from %d to %d", reposBefore, got)
	}
}

// Nothing configured deletes nothing. A deployment that wants an unbounded
// audit trail gets one by leaving the field unset, not by finding a switch.
func TestNoPolicyDeletesNothing(t *testing.T) {
	h := newRetentionHarness(t)
	id := h.settledTransfer("succeeded", 365*24*time.Hour, 2)

	res, err := h.packages.SweepRetention(t.Context(), RetentionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows() != 0 {
		t.Errorf("an empty policy removed %d rows", res.Rows())
	}
	if n := h.count(`SELECT count(*) FROM transfers WHERE id = ?`, id); n != 1 {
		t.Error("a transfer was deleted with no retention configured")
	}
	if (RetentionPolicy{}).Enabled() {
		t.Error("an empty policy reports itself enabled")
	}
}

// One pass removes at most BatchSize, so the first sweep of a database that has
// run unbounded for a year does not hold a lock for minutes.
func TestASweepIsBounded(t *testing.T) {
	h := newRetentionHarness(t)
	for range 5 {
		h.settledTransfer("succeeded", 365*24*time.Hour, 1)
	}

	res, err := h.packages.SweepRetention(t.Context(), RetentionPolicy{
		Transfers: time.Hour, BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfers != 2 {
		t.Errorf("one pass removed %d transfers, want the batch size of 2", res.Transfers)
	}
	if n := h.count(`SELECT count(*) FROM transfers`); n != 3 {
		t.Errorf("%d transfers left, want 3", n)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type retentionHarness struct {
	*transferRefHarness
	n int
}

func newRetentionHarness(t *testing.T) *retentionHarness {
	t.Helper()
	return &retentionHarness{transferRefHarness: newTransferRefHarness(t)}
}

func (h *retentionHarness) settledTransfer(state string, age time.Duration, jobs int) string {
	h.t.Helper()

	id := h.newTransfer()
	h.exec(`UPDATE transfers SET state=?, completed_at=? WHERE id = ?`,
		state, ago(age), id)
	h.addJobs(id, jobs, "succeeded")
	return id
}

func (h *retentionHarness) runningTransferAged(age time.Duration) string {
	h.t.Helper()

	id := h.newTransfer()
	h.exec(`UPDATE transfers SET state='running', started_at=?, completed_at=NULL WHERE id = ?`,
		ago(age), id)
	h.addJobs(id, 1, "pending")
	return id
}

func (h *retentionHarness) newTransfer() string {
	h.t.Helper()
	h.n++
	id := "0000000" + strconv.Itoa(h.n) + "-eeee-ffff-0000-111111111111"
	h.transfer(id)
	return id
}

func (h *retentionHarness) addJobs(transferID string, n int, state string) {
	h.t.Helper()
	for i := range n {
		digest := "sha256:" + strings.Repeat(strconv.Itoa(i%10), 64)
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, max_attempts)
		         VALUES (?, 'blob', ?, 1024, ?, ?, ?, 0, 8)`,
			transferID, digest, h.repoID, h.repoID, state)
	}
}

func (h *retentionHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRowContext(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *retentionHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

// ago is a timestamp in the format the SQLite schema's columns hold.
func ago(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format("2006-01-02T15:04:05.000Z")
}
