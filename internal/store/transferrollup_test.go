package store

import (
	"fmt"
	"testing"
	"time"
)

// EVERY ONE OF THE TWELVE JOB ROLLUPS, against an estate whose answers are
// known by construction.
//
// # The mistake this exists to catch
//
// The rollups used to be twelve correlated subqueries in the select list, one
// per column, each with its own WHERE. They are now twelve aggregates in one
// GROUP BY over the page's jobs - the same numbers computed in a single pass
// instead of twelve, which is what took the Downloads page from seconds to
// tens of milliseconds (docs/design/32-performance.md).
//
// A rewrite like that fails QUIETLY. Every existing test still passed with a
// `SUM(CASE ...)` whose condition had drifted, because the ones that touch the
// rollups at all seed a single job and assert one or two of the columns. A
// count that is wrong by the rows of one state is not a crash; it is a
// progress bar that says 94% forever, and nobody can tell from the interface
// whether the number or the transfer is wrong.
//
// So this seeds one transfer with a known population of jobs in every state
// that matters and asserts all twelve, on both databases - the aggregate
// syntax is one of the places the two dialects are entitled to disagree.
func TestEveryJobRollupIsCountedExactly(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.rollupEstate()

		for _, tc := range []struct {
			what string
			got  func(TransferSummary) int64
			want int64
		}{
			// 3 succeeded + 2 skipped.
			{"jobs done", func(s TransferSummary) int64 { return int64(s.JobsDone) }, 5},
			{"jobs failed", func(s TransferSummary) int64 { return int64(s.JobsFailed) }, 2},
			// 4 pending (one of them not yet visible) + 1 blocked + 3 leased.
			{"jobs outstanding", func(s TransferSummary) int64 { return int64(s.JobsOutstanding) }, 8},
			{"jobs in flight", func(s TransferSummary) int64 { return int64(s.JobsInFlight) }, 3},
			{"jobs blocked", func(s TransferSummary) int64 { return int64(s.JobsBlocked) }, 1},
			// Two distinct lease owners across the three leased jobs.
			{"workers", func(s TransferSummary) int64 { return int64(s.Workers) }, 2},
			// The one pending job whose next_visible_at is in the future.
			{"jobs waiting", func(s TransferSummary) int64 { return int64(s.JobsWaiting) }, 1},
			{"jobs repaired", func(s TransferSummary) int64 { return int64(s.JobsRepaired) }, 2},
			// The two skipped jobs carry 1000 bytes each.
			{"skipped bytes", func(s TransferSummary) int64 { return s.SkippedBytes }, 2000},
			// Every one of the fifteen jobs carries 100 bytes transferred.
			{"bytes transferred", func(s TransferSummary) int64 { return s.BytesTransferred }, 1500},
			// The eight outstanding jobs are 1000 bytes each with 100 moved.
			{"outstanding bytes", func(s TransferSummary) int64 { return s.OutstandingBytes }, 7200},
		} {
			t.Run(tc.what, func(t *testing.T) {
				// BOTH READERS, because list and get build the page differently
				// - a filtered, ordered, limited page against a single id - and
				// a rollup scoped to the wrong one of those is exactly the bug
				// that would not show up in a listing of one transfer.
				list := h.listOne(t, id)
				if got := tc.got(list); got != tc.want {
					t.Errorf("ListTransfers: %s = %d, want %d", tc.what, got, tc.want)
				}
				got, err := h.packages.GetTransfer(t.Context(), id)
				if err != nil {
					t.Fatalf("get transfer: %v", err)
				}
				if v := tc.got(got); v != tc.want {
					t.Errorf("GetTransfer: %s = %d, want %d", tc.what, v, tc.want)
				}
			})
		}

		// The quietest leased job is the one whose updated_at is oldest, and it
		// goes out as RFC3339 text from both databases.
		list := h.listOne(t, id)
		if _, err := time.Parse(time.RFC3339, list.QuietestInFlight); err != nil {
			t.Errorf("quietest in flight = %q, which does not parse as RFC3339: %v",
				list.QuietestInFlight, err)
		}
	})
}

// A transfer with no jobs at all reads as zero rather than null.
//
// The rollup is a LEFT JOIN now, so a transfer whose jobs have not been planned
// yet has no row on the right of it. Scanning a null into an int64 is an error,
// not a zero, so this is the difference between an empty transfer listing and a
// 500.
func TestATransferWithNoJobsRollsUpToZero(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(time.Minute, false)
		h.exec(`DELETE FROM jobs WHERE transfer_id = ?`, id)

		got, err := h.packages.GetTransfer(t.Context(), id)
		if err != nil {
			t.Fatalf("get a transfer with no jobs: %v", err)
		}
		if got.JobsDone != 0 || got.JobsOutstanding != 0 || got.BytesTransferred != 0 {
			t.Errorf("rollup of a transfer with no jobs = %d done, %d outstanding, %d bytes; want zeroes",
				got.JobsDone, got.JobsOutstanding, got.BytesTransferred)
		}
		if got.QuietestInFlight != "" {
			t.Errorf("quietest in flight = %q for a transfer with no jobs, want empty",
				got.QuietestInFlight)
		}
	})
}

// listOne is the transfer under test, read through the LISTING rather than by
// id, so the page-scoped rollup is what answers.
func (h *activeHarness) listOne(t *testing.T, id string) TransferSummary {
	t.Helper()
	rows, err := h.packages.ListTransfers(t.Context(), ListTransfersFilter{Limit: 25})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	for i := range rows {
		if rows[i].ID == id {
			return rows[i]
		}
	}
	t.Fatalf("listed %d transfers, none of them %s", len(rows), id)
	return TransferSummary{}
}

// rollupEstate is one transfer carrying fifteen jobs whose states, owners,
// sizes and repair levels are chosen so that every one of the twelve rollups
// has a different answer. A column that reads another column's population is
// then a visible failure rather than a coincidence.
func (h *activeHarness) rollupEstate() string {
	h.t.Helper()

	id := h.running(time.Minute, false)
	// running() seeds one pending job of its own; this test counts only what
	// it writes below.
	h.exec(`DELETE FROM jobs WHERE transfer_id = ?`, id)

	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	future := time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.000Z")

	type job struct {
		state       string
		owner       any
		repairLevel int
		visibleAt   string
	}
	jobs := []job{
		{state: "succeeded"}, {state: "succeeded"}, {state: "succeeded"},
		{state: "skipped"}, {state: "skipped"},
		{state: "failed", repairLevel: 1}, {state: "failed", repairLevel: 2},
		{state: "pending", visibleAt: past}, {state: "pending", visibleAt: past},
		{state: "pending", visibleAt: past},
		// The one job that is pending but not yet VISIBLE - a retry backing off.
		{state: "pending", visibleAt: future},
		{state: "blocked", visibleAt: past},
		// Three leased jobs held by TWO workers, so `workers` cannot be read
		// off the count of leased jobs.
		{state: "leased", owner: "worker-a", visibleAt: past},
		{state: "leased", owner: "worker-a", visibleAt: past},
		{state: "leased", owner: "worker-b", visibleAt: past},
	}

	for i, j := range jobs {
		visible := j.visibleAt
		if visible == "" {
			visible = past
		}
		// Every job is 1000 bytes with 100 moved, so a rollup that sums the
		// wrong population is off by a round, recognisable amount.
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, attempts, max_attempts,
		                          bytes_transferred, repair_level, next_visible_at,
		                          lease_owner, updated_at)
		         VALUES (?, 'blob', ?, 1000, ?, ?, ?, 0, 1, 8, 100, ?, ?, ?, ?)`,
			id, fmt.Sprintf("sha256:%064d", i), h.repoID, h.repoID, j.state,
			j.repairLevel, visible, j.owner,
			time.Now().UTC().Add(-time.Duration(i)*time.Minute).Format("2006-01-02T15:04:05.000Z"))
	}
	return id
}
