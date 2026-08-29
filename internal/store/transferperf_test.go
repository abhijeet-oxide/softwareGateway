package store

import (
	"fmt"
	"testing"
	"time"
)

// What a transfer listing costs, and the two things that decide it.
//
// # The measurement these exist to keep
//
// On an estate of sixty transfers of 2,500 jobs - 150,000 job rows, which a
// real deployment passes in a week - the listing the interface polls measured:
//
//	                          before   after
//	ListTransfers(limit=25)    160ms    64ms
//	ListTransfers(limit=100)   158ms   158ms
//	Activity (the shell's line)   --      1ms
//
// The first row is migration 00030. The listing sorts by `created_at DESC, id
// DESC` and had no index in that order, so the planner read EVERY transfer,
// evaluated a dozen correlated subqueries over `jobs` for each of them, sorted
// the lot in a temporary B-tree and threw all but the page away. The second row
// is the tell: asking for 25 rows and asking for 100 cost the same, because the
// work was per table rather than per page.
//
// The third is the shell's status line, which used to be the hundred-row
// listing counted in the browser.
//
// # Why the numbers are not asserted
//
// They are wall-clock timings on whatever machine happens to be running, and a
// threshold tight enough to catch the regression would fail on a loaded CI box.
// What IS asserted lives in TestTransferListingQueryPlan, which checks the
// shape rather than the duration: a full scan of `transfers` or a temporary
// sort is the regression, and neither is a matter of degree.

// seedJobs gives each transfer `each` jobs, in ONE transaction.
//
// One transaction, and no store call inside it, because the SQLite pool is a
// single connection: a BeginTx nested inside another waits for a connection the
// same goroutine is holding, and never returns. See internal/store/sqlite.go.
func seedJobs(t *testing.T, h *activeHarness, ids []string, each int) {
	t.Helper()

	tx, err := h.st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	insert := h.packages.dialect.Rewrite(
		`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                   target_repo_id, state, wave, attempts, max_attempts,
		                   bytes_transferred, started_at, completed_at)
		 VALUES (?, 'blob', ?, 1048576, ?, ?, ?, 0, 1, 8, 1048576, ?, ?)`)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	for i, id := range ids {
		for j := range each {
			state := "succeeded"
			if j%17 == 0 {
				state = "skipped"
			}
			if _, err := tx.ExecContext(t.Context(), insert,
				id, fmt.Sprintf("sha256:%064x", i*1_000_000+j),
				h.repoID, h.repoID, state, now, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// The shell's line has to be the same answer the listing would give, or the
// cheap way to ask is just a wrong one.
//
// Small on purpose: this is a correctness test, and the timings above are what
// justify the route's existence rather than what this checks.
func TestActivityAgreesWithTheListing(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		moving := []string{h.running(time.Second, true), h.running(time.Second, true)}
		held := []string{h.running(time.Second, false)}
		seedJobs(t, h, moving, 3)

		failed := h.running(time.Second, true)
		h.settle(failed, "failed", time.Second)

		got, err := h.packages.Activity(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got.Moving != len(moving) {
			t.Errorf("Moving = %d, want %d", got.Moving, len(moving))
		}
		if got.Held != len(held) {
			t.Errorf("Held = %d, want %d", got.Held, len(held))
		}
		if got.Failed != 1 {
			t.Errorf("Failed = %d, want 1", got.Failed)
		}

		/*
		  Derived from the listing the shell used to count, so the two answers
		  are COMPARED rather than the cheap one merely asserted.

		  SQLite only, and not because the summary is dialect-specific - it
		  returns three integers and runs on both. The LISTING does not work on
		  Postgres, for reasons that predate this route and are not its to fix:
		  the projection coalesces timestamptz columns to an empty string, which
		  Postgres refuses as a timestamp. Skipping the cross-check where the
		  thing being cross-checked against is broken is honest; asserting the
		  summary alone on Postgres, which happens above, is what this can
		  legitimately claim there.
		*/
		if h.packages.dialect.Name() != DriverSQLite {
			return
		}
		rows, err := h.packages.ListTransfers(t.Context(), ListTransfersFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var wantMoving, wantHeld, wantFailed int
		for _, r := range rows {
			switch {
			case r.State == "failed":
				wantFailed++
			case liveState(r.State) && r.JobsInFlight > 0:
				wantMoving++
			case liveState(r.State):
				wantHeld++
			}
		}

		if got.Moving != wantMoving || got.Held != wantHeld || got.Failed != wantFailed {
			t.Errorf("summary %+v disagrees with the listing (moving %d, held %d, failed %d)",
				got, wantMoving, wantHeld, wantFailed)
		}
	})
}

func liveState(state string) bool {
	for _, s := range []string{
		"pending", "planning", "ready", "running", "paused",
		"cancelling", "verifying", "promoting", "syncing",
	} {
		if state == s {
			return true
		}
	}
	return false
}
