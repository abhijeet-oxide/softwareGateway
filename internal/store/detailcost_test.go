package store

import (
	"strings"
	"testing"
	"time"
)

// What the download page costs the Coordinator, per poll.
//
// It is the page somebody has open while a download runs - the exact moment the
// interface was reported as slow - and it polls every two seconds. One request
// fans out to six store calls, so this times each of them against an estate
// with real job counts and says which of the six is worth looking at.
func TestDownloadPageCost(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds tens of thousands of rows")
	}
	h := newActiveHarness(t, openTestStore)

	const (
		transfers = 20
		jobsEach  = 2500
	)
	ids := make([]string, 0, transfers)
	for range transfers {
		ids = append(ids, h.running(30*time.Second, true))
	}
	seedJobs(t, h, ids, jobsEach)
	id := ids[0]

	type step struct {
		name string
		run  func() error
	}
	steps := []step{
		{"GetTransfer", func() error { _, err := h.packages.GetTransfer(t.Context(), id); return err }},
		{"WaveProgress", func() error { _, err := h.packages.WaveProgress(t.Context(), id); return err }},
		{"ContentBreakdown", func() error {
			_, err := h.packages.ContentBreakdown(t.Context(), id)
			return err
		}},
		{"TransferContentBytes", func() error {
			_, err := h.packages.TransferContentBytes(t.Context(), id)
			return err
		}},
		{"SkipBreakdown", func() error { _, err := h.packages.SkipBreakdown(t.Context(), id); return err }},
		{"ListJobs(200)", func() error {
			_, err := h.packages.ListJobs(t.Context(), id, "", 200)
			return err
		}},
	}

	var total time.Duration
	for _, s := range steps {
		start := time.Now()
		if err := s.run(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		d := time.Since(start)
		total += d
		t.Logf("%-22s %v", s.name, d.Round(100*time.Microsecond))
	}
	t.Logf("%-22s %v for one poll of one download page (%d jobs)",
		"TOTAL", total.Round(time.Millisecond), jobsEach)
}

// The lease, which is the hottest write path in the system.
//
// A worker asks for work every time it has room, and since the loop stopped
// sleeping between batches it asks a great deal more often. So the dequeue has
// to be cheap at the size a real estate reaches - and it is worth knowing
// whether the duplicate-suppression NOT EXISTS, which joins `repositories`,
// stays confined to the handful of digests actually in flight.
func TestLeaseCostAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds tens of thousands of rows")
	}
	h := newActiveHarness(t, openTestStore)

	const (
		transfers = 20
		jobsEach  = 2500
	)
	ids := make([]string, 0, transfers)
	for range transfers {
		ids = append(ids, h.running(30*time.Second, true))
	}
	seedJobs(t, h, ids, jobsEach)

	// Make them all leasable: seedJobs writes them settled.
	h.exec(`UPDATE jobs SET state='pending', lease_owner=NULL, completed_at=NULL`)

	/*
	  THE BUNDLE SHAPE, which is the case the dequeue's second clause exists
	  for and the only one that runs it.

	  A component published under its own name as well as inside a bundle has
	  one digest and two destination repositories, so it becomes two jobs: rank
	  0 and rank 1. A rank-1 job is not leasable while its rank-0 sibling is
	  still outstanding, and establishing that is a lookup by digest.

	  With every job at rank 0 the clause short-circuits and is never executed,
	  so a measurement taken that way says nothing about it. Half the estate is
	  put at rank 1 here, which is what a bundle-heavy product actually looks
	  like.
	*/
	h.exec(`UPDATE jobs SET site_rank = 1 WHERE id % 2 = 0`)

	var total time.Duration
	const rounds = 10
	for i := range rounds {
		start := time.Now()
		leased, err := h.packages.LeaseJobs(t.Context(), LeaseRequest{
			Owner: "w1", Limit: 32, Duration: 2 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		total += time.Since(start)
		if i == 0 && len(leased) != 32 {
			t.Fatalf("leased %d jobs, want a full batch of 32", len(leased))
		}
	}
	t.Logf("LeaseJobs(32) over %d pending jobs: %v per call",
		transfers*jobsEach, (total / rounds).Round(100*time.Microsecond))

	rows, err := h.st.DB().QueryContext(t.Context(),
		"EXPLAIN QUERY PLAN "+leaseSQLForPlan(h))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		t.Logf("  %s", detail)
	}

	for _, line := range plan {
		// "AUTOMATIC" is the planner saying it had to build a transient index
		// because none existed. On the hottest write path in the system, over a
		// table that only grows, that is a cost per lease rather than a cost
		// once. See migration 00031.
		if strings.Contains(line, "AUTOMATIC") {
			t.Errorf("the dequeue builds a transient index on every lease:\n  %s", line)
		}
		if strings.Contains(line, "TEMP B-TREE") {
			t.Errorf("the dequeue sorts rather than walking the index in order, so its "+
				"cost is the whole queue rather than the batch:\n  %s", line)
		}
	}
}

// leaseSQLForPlan is the candidate SELECT the SQLite dequeue runs, without the
// UPDATE around it, so its plan can be read.
//
// leaseOrder, not an order written out here: the whole question is whether the
// dequeue index matches the order the dequeue actually asks for, and a test
// that spelled it out separately would answer a question nobody is asking. It
// did, on the first attempt, and reported a sort that does not exist.
func leaseSQLForPlan(h *activeHarness) string {
	return `SELECT id FROM jobs WHERE ` + h.packages.leaseCandidatePredicate() +
		leaseOrder + ` LIMIT 32`
}

// What the job aggregates cost, and what the callers that do not want them save.
//
// The Overview and the package listing fetch a hundred transfers to join
// download history onto releases - which target, which state, when - and read
// none of the job counts. This is what they were paying for them.
func TestListingWithoutJobCountsIsMuchCheaper(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds tens of thousands of rows")
	}
	h := newActiveHarness(t, openTestStore)

	const (
		transfers = 60
		jobsEach  = 2500
	)
	ids := make([]string, 0, transfers)
	for range transfers {
		ids = append(ids, h.running(30*time.Second, true))
	}
	seedJobs(t, h, ids, jobsEach)

	start := time.Now()
	full, err := h.packages.ListTransfers(t.Context(), ListTransfersFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	withCounts := time.Since(start)

	start = time.Now()
	lite, err := h.packages.ListTransfers(t.Context(),
		ListTransfersFilter{Limit: 100, WithoutJobCounts: true})
	if err != nil {
		t.Fatal(err)
	}
	withoutCounts := time.Since(start)

	t.Logf("ListTransfers(100) with counts %v, without %v",
		withCounts.Round(time.Millisecond), withoutCounts.Round(time.Millisecond))

	if len(full) != len(lite) {
		t.Fatalf("the two listings returned %d and %d rows", len(full), len(lite))
	}

	// EVERYTHING BUT THE COUNTS HAS TO MATCH. A cheaper listing that quietly
	// says something different about a transfer's identity or outcome would be
	// worse than a slow one.
	for i := range full {
		if full[i].ID != lite[i].ID || full[i].State != lite[i].State ||
			full[i].Operation != lite[i].Operation ||
			full[i].CompletedAt != lite[i].CompletedAt ||
			full[i].ActiveSeconds != lite[i].ActiveSeconds {
			t.Fatalf("row %d differs beyond the job counts:\n full: %+v\n lite: %+v",
				i, full[i], lite[i])
		}
	}

	// And the counts really are absent rather than wrong-but-plausible.
	if lite[0].JobsDone != 0 || lite[0].BytesTransferred != 0 {
		t.Errorf("the lite listing carries job counts: %+v", lite[0])
	}
	if full[0].JobsDone == 0 {
		t.Fatal("the seeded transfers have no completed jobs, so this proves nothing")
	}
}
