package store

import (
	"strings"
	"testing"
	"time"
)

// What the planner actually does with the transfer projection.
//
// Kept as a test rather than a one-off because the answer is the justification
// for how the projection is written, and a future edit that reintroduces a
// full scan of `jobs` should be visible.
func TestTransferListingQueryPlan(t *testing.T) {
	h := newActiveHarness(t, openTestStore)
	ids := []string{h.running(time.Second, true)}
	seedJobs(t, h, ids, 10)

	rows, err := h.st.DB().QueryContext(t.Context(),
		"EXPLAIN QUERY PLAN "+
			h.packages.transferListQuery(" WHERE 1=1", true, 0), 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	t.Log("query plan:\n  " + strings.Join(plan, "\n  "))

	var scans int
	for _, line := range plan {
		if strings.Contains(line, "SCAN jobs") {
			scans++
		}
	}
	if scans > 0 {
		t.Errorf("%d full scans of `jobs` in one listing - the projection is "+
			"reading the whole table per row rather than seeking by transfer", scans)
	}
}

// Content accounting looks up the same digest several ways. Without the
// transfer-and-digest index, each lookup walks every job in the transfer.
func TestTransferContentLookupUsesDigestIndex(t *testing.T) {
	h := newActiveHarness(t, openTestStore)
	id := h.running(time.Second, true)
	seedJobs(t, h, []string{id}, 10)

	rows, err := h.st.DB().QueryContext(t.Context(),
		"EXPLAIN QUERY PLAN SELECT MAX(bytes_transferred) FROM jobs "+
			"WHERE transfer_id = ? AND digest = ?", id, "sha256:missing")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var plan []string
	for rows.Next() {
		var planID, parent, notused int
		var detail string
		if err := rows.Scan(&planID, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	detail := strings.Join(plan, "\n")
	if !strings.Contains(detail, "jobs_transfer_digest_idx (transfer_id=? AND digest=?)") {
		t.Fatalf("content lookup does not seek by transfer and digest:\n%s", detail)
	}
}
