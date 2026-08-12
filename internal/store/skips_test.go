package store

import (
	"strings"
	"testing"
)

// "Done" is four different claims wearing one word, and the difference is the
// answer to "why is it re-sending blobs that already succeeded?".

func TestSkipsAreReportedByReasonAndByHowMuchTheyAreWorth(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	// Streamed: proof the bytes arrived.
	h.skippedBlob(id, 1, "", "succeeded", 5_000_000)
	// The registry relocated it. Its own action, so the saving is real.
	h.skippedBlob(id, 2, "mounted", "skipped", 3_000_000)
	// Two claims. Only as good as the answer they trusted.
	h.skippedBlob(id, 3, "exists_at_target", "skipped", 900_000)
	h.skippedBlob(id, 4, "exists_at_target", "skipped", 600_000)
	h.skippedBlob(id, 5, "placement_hit", "skipped", 100_000)

	got, err := h.packages.SkipBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("reported %d reasons, want 3", len(got))
	}

	by := map[string]SkipSummary{}
	for _, s := range got {
		by[s.Reason] = s
	}

	// A streamed blob is not a skip: it moved bytes, which is the one thing
	// none of these did.
	if _, wrong := by[""]; wrong {
		t.Error("a succeeded job was counted as a skip")
	}
	if s := by["exists_at_target"]; s.Jobs != 2 || s.Bytes != 1_500_000 {
		t.Errorf("exists_at_target = %+v, want 2 jobs and 1.5 MB", s)
	}
	if by["mounted"].Trusted != true {
		t.Error("a mount is something the registry DID; it must not read as unverified")
	}
	for _, reason := range []string{"exists_at_target", "placement_hit"} {
		if by[reason].Trusted {
			t.Errorf("%s rests on a claim, not an action, and must be flagged as such", reason)
		}
	}

	// Biggest saving first: the line worth doubting is the one holding the most.
	if got[0].Reason != "mounted" {
		t.Errorf("first reason = %q, want the largest saving first", got[0].Reason)
	}
}

func (h *failureHarness) skippedBlob(transferID string, n int, reason, state string, size int64) {
	h.t.Helper()

	h.n++
	digest := "sha256:" + strings.Repeat("f", 56) + padded(n) + padded(h.n)
	var skip any
	if reason != "" {
		skip = reason
	}
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
	                          target_repo_id, state, skip_reason, wave, attempts, max_attempts)
	         VALUES (?, 'blob', ?, ?, ?, ?, ?, ?, 0, 1, 8)`,
		transferID, digest, size, h.repoID, h.repoID, state, skip)
}
