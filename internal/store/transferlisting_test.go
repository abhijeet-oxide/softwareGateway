package store

import (
	"testing"
	"time"
)

// LISTING AND READING A TRANSFER, ON BOTH DATABASES.
//
// # Why this needs a test at all
//
// The transfer projection is the most-read query in the application - the
// Downloads page, the Overview, the shell's status line, every `transferctl
// transfers` command - and until this test existed it had never once been run
// against Postgres. Every test that touched it opened SQLite, whose dialect is
// largely the identity function, so the two ways this query was NOT portable
// both shipped:
//
//   - `?` placeholders left unrewritten, because an apostrophe in a comment
//     put the rewriter inside an imaginary string literal. Postgres answered
//     `syntax error at or near "OFFSET"`.
//   - `COALESCE(<timestamptz>, ”)`, which Postgres rejects outright: the
//     empty string is not a timestamp and there is no implicit cast that makes
//     it one.
//
// Neither is subtle once seen and neither could be seen from SQLite. So this
// asserts the modest thing - that the query RUNS, and that what comes back is
// the transfer that went in - on whichever databases are reachable.
func TestTransfersCanBeListedAndRead(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(time.Minute, true)

		rows, err := h.packages.ListTransfers(t.Context(), ListTransfersFilter{Limit: 10})
		if err != nil {
			t.Fatalf("list transfers: %v", err)
		}
		var listed *TransferSummary
		for i := range rows {
			if rows[i].ID == id {
				listed = &rows[i]
			}
		}
		if listed == nil {
			t.Fatalf("listed %d transfers, none of them the one just created", len(rows))
		}

		got, err := h.packages.GetTransfer(t.Context(), id)
		if err != nil {
			t.Fatalf("get transfer: %v", err)
		}
		if got.ID != id || got.State != "running" {
			t.Errorf("read back %s in state %q, want %s running", got.ID, got.State, id)
		}

		// The rollup ran: one job was seeded and a worker is holding it.
		if got.JobsInFlight != 1 || got.JobsOutstanding != 1 {
			t.Errorf("rollup = %d in flight, %d outstanding; want 1 and 1",
				got.JobsInFlight, got.JobsOutstanding)
		}

		// THE TIMESTAMPS ARE STRINGS A CLIENT CAN PARSE, which is the part
		// `COALESCE(x, '')` was there to arrange and the part Postgres could
		// not do. `started_at` was set; `completed_at` was not, and its absence
		// must read as empty rather than as a failure or the word "null".
		if _, err := time.Parse(time.RFC3339, got.StartedAt); err != nil {
			t.Errorf("startedAt %q does not parse as RFC3339: %v", got.StartedAt, err)
		}
		if got.CompletedAt != "" {
			t.Errorf("completedAt = %q on a running transfer, want empty", got.CompletedAt)
		}
		if _, err := time.Parse(time.RFC3339, got.CreatedAt); err != nil {
			t.Errorf("createdAt %q does not parse as RFC3339: %v", got.CreatedAt, err)
		}
		// The anchor, which the API adds this instant's remainder to. Same
		// requirement: a string, parseable, and empty rather than absent.
		if _, err := time.Parse(time.RFC3339, got.LastActiveAt); err != nil {
			t.Errorf("lastActiveAt %q does not parse as RFC3339: %v", got.LastActiveAt, err)
		}
		// MIN(updated_at) over the leased jobs - a timestamp produced by an
		// aggregate rather than read from a column, and coalesced the same way.
		if _, err := time.Parse(time.RFC3339, got.QuietestInFlight); err != nil {
			t.Errorf("quietestInFlight %q does not parse as RFC3339: %v",
				got.QuietestInFlight, err)
		}
	})
}

// The summary listing takes a different branch through the same projection -
// the twelve subqueries become literal zeros and MIN(updated_at) becomes an
// empty string - so it is a second query that has to be portable, not the same
// one with a flag.
func TestTheSummaryListingIsPortableToo(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(time.Minute, true)

		rows, err := h.packages.ListTransfers(t.Context(),
			ListTransfersFilter{Limit: 10, WithoutJobCounts: true})
		if err != nil {
			t.Fatalf("list transfers without counts: %v", err)
		}
		for _, r := range rows {
			if r.ID != id {
				continue
			}
			if r.JobsInFlight != 0 {
				t.Errorf("the summary reported %d jobs in flight, want the "+
					"literal zero that stands in for the rollup", r.JobsInFlight)
			}
			if _, err := time.Parse(time.RFC3339, r.StartedAt); err != nil {
				t.Errorf("startedAt %q does not parse as RFC3339: %v", r.StartedAt, err)
			}
			return
		}
		t.Fatalf("listed %d transfers, none of them the one just created", len(rows))
	})
}

// Paging is where the placeholder rewriting was caught, so it is asserted
// rather than left to a listing that happens to fit on one page.
func TestATransferListingPages(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		for range 3 {
			h.running(time.Minute, false)
		}

		first, err := h.packages.ListTransfers(t.Context(),
			ListTransfersFilter{Limit: 2})
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		second, err := h.packages.ListTransfers(t.Context(),
			ListTransfersFilter{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		if len(first) != 2 || len(second) != 1 {
			t.Fatalf("pages of %d and %d, want 2 and 1", len(first), len(second))
		}
		for _, a := range first {
			for _, b := range second {
				if a.ID == b.ID {
					t.Errorf("%s is on both pages", a.ID)
				}
			}
		}
	})
}

// THE WHOLE DOWNLOAD PAGE, on both databases.
//
// Listing a transfer is one query of the several a reader actually causes: the
// detail page adds the waves, the content breakdown, the byte account, the skip
// reasons and the job list, and the shell adds the activity summary. They are
// all read paths built the same way out of the same helpers, so they can all
// carry the same portability defect - and until this ran, none of them had ever
// been executed against Postgres either.
//
// It asserts that each RUNS and comes back coherent. What each one MEANS is
// tested elsewhere, on SQLite, where a fixture is cheap; this is here to catch
// the class of failure that only one of the two databases can produce.
func TestTheDownloadPageReadsAreAllPortable(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(time.Minute, true)

		if _, err := h.packages.WaveProgress(t.Context(), id); err != nil {
			t.Errorf("wave progress: %v", err)
		}
		if _, err := h.packages.ContentBreakdown(t.Context(), id); err != nil {
			t.Errorf("content breakdown: %v", err)
		}
		if _, err := h.packages.TransferContentBytes(t.Context(), id); err != nil {
			t.Errorf("content bytes: %v", err)
		}
		if _, err := h.packages.SkipBreakdown(t.Context(), id); err != nil {
			t.Errorf("skip breakdown: %v", err)
		}

		jobs, err := h.packages.ListJobs(t.Context(), id, "", 100)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("listed %d jobs, want the one that was seeded", len(jobs))
		}
		if jobs[0].State != "leased" {
			t.Errorf("the job came back %q, want leased", jobs[0].State)
		}

		// The shell's status line. One transfer, running, with a job in a
		// worker's hands - so it is moving rather than held.
		act, err := h.packages.Activity(t.Context())
		if err != nil {
			t.Fatalf("activity: %v", err)
		}
		if act.Moving != 1 || act.Held != 0 || act.Failed != 0 {
			t.Errorf("activity = %+v, want 1 moving and nothing else", act)
		}
	})
}

// EVERY FILTER, because each one appends a placeholder.
//
// The placeholders are rewritten by position, so a filter that is never
// exercised is a `?` nobody has watched become a `$N`. All four are combined in
// one call, which is the arrangement most likely to expose an off-by-one
// between the WHERE clause and LIMIT/OFFSET at the end.
func TestEveryTransferFilterIsPortable(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(time.Minute, true)

		rows, err := h.packages.ListTransfers(t.Context(), ListTransfersFilter{
			ProductName: "p",
			State:       "running",
			PackageID:   h.pkgID,
			Operation:   "replicate",
			Limit:       10,
		})
		if err != nil {
			t.Fatalf("list with every filter set: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("the transfer matches all four filters and was not listed "+
				"among %d rows", len(rows))
		}

		// And a filter that matches nothing answers nothing rather than
		// everything - which is what a placeholder bound to the wrong position
		// would produce.
		none, err := h.packages.ListTransfers(t.Context(),
			ListTransfersFilter{State: "succeeded", Limit: 10})
		if err != nil {
			t.Fatalf("list by state: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("filtering on a state nothing is in returned %d rows", len(none))
		}
	})
}

// A SHORT ID, which is how a person refers to a transfer.
//
// `transferctl` and the API both accept a prefix, and the resolution behind
// them does two things Postgres does not do to a UUID column: it compares it
// with an arbitrary user-supplied string, and it matches it with LIKE. On
// SQLite the column is text and both are ordinary; on Postgres the first raises
// `invalid input syntax for type uuid` for anything that is not a whole UUID -
// so the "not found, try a prefix" path was never reached - and the second
// raises `operator does not exist: uuid ~~ unknown`.
func TestATransferCanBeFoundByAShortID(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(time.Minute, false)

		got, err := h.packages.ResolveTransferID(t.Context(), id[:8])
		if err != nil {
			t.Fatalf("resolve %q: %v", id[:8], err)
		}
		if got != id {
			t.Errorf("resolved to %s, want %s", got, id)
		}

		// The whole id still resolves to itself, which is the path every
		// caller with a full id takes.
		if got, err := h.packages.ResolveTransferID(t.Context(), id); err != nil || got != id {
			t.Errorf("resolving the full id gave %q, %v", got, err)
		}

		// And something that is neither is NOT FOUND rather than a database
		// error, because it is a person mistyping rather than a fault.
		if _, err := h.packages.ResolveTransferID(t.Context(), "nosuchtransfer"); err == nil {
			t.Error("an unknown reference resolved to something")
		}
	})
}
