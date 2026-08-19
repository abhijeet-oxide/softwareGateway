package store

import (
	"errors"
	"testing"
	"time"
)

// Two processes reaching for the same release produce one walk.
//
// The whole reason the claim is a database row: discovery's background analyser
// and somebody pressing "Analyze package" are two processes, possibly on two
// replicas, and without this they would hold two conversations with the vendor's
// registry to compute the same answer.
func TestOnlyOneClaimOnARelease(t *testing.T) {
	h := newCacheHarness(t)
	id := h.seedPackageAt("orb_25.7.2131", time.Now().Add(-time.Hour))

	first, err := h.packages.ClaimAnalysis(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("the first claim on an unheld release was refused")
	}

	second, err := h.packages.ClaimAnalysis(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("a second process claimed a release already being walked")
	}
}

// A release being walked is not offered to the queue that finds work.
//
// Without this the background analyser would pick up, on its next pass, exactly
// the releases it is already walking.
func TestAReleaseBeingWalkedIsNotQueuedAgain(t *testing.T) {
	h := newCacheHarness(t)
	id := h.seedPackageAt("orb_25.7.2131", time.Now().Add(-time.Hour))
	h.seedListedChild(id)

	if rows, err := h.packages.UnanalysedRecent(t.Context(), h.repoID, 7*24*time.Hour, 10); err != nil {
		t.Fatal(err)
	} else if len(rows) != 1 {
		t.Fatalf("the release is not offered before it is claimed: %+v", rows)
	}

	if _, err := h.packages.ClaimAnalysis(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	rows, err := h.packages.UnanalysedRecent(t.Context(), h.repoID, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a release already being walked is offered again: %+v", rows)
	}
}

// A failure keeps its reason, and the release is left alone for a while.
//
// A component the vendor has withdrawn cannot be walked at all, and without the
// cooldown every pass would spend its whole batch failing on the same release.
func TestAFailedWalkIsRememberedAndNotRetriedImmediately(t *testing.T) {
	h := newCacheHarness(t)
	id := h.seedPackageAt("orb_25.7.2131", time.Now().Add(-time.Hour))
	h.seedListedChild(id)

	if _, err := h.packages.ClaimAnalysis(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.packages.FinishAnalysis(t.Context(), id,
		errors.New("the vendor withdrew cfx-5000-product/bgcf")); err != nil {
		t.Fatal(err)
	}

	rows, err := h.packages.UnanalysedRecent(t.Context(), h.repoID, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a release that just failed is retried immediately: %+v", rows)
	}

	pkg, err := h.packages.GetPackage(t.Context(), "vendor-a", "orb_25.7.2131")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.AnalysisState != AnalysisFailed {
		t.Errorf("state = %q, want %q", pkg.AnalysisState, AnalysisFailed)
	}
	if pkg.AnalysisError == "" {
		t.Error("the release does not say why the walk gave up, which is a dead end " +
			"for whoever reads it a week later")
	}
}

// A crash must not leave a release marked "analyzing" for ever.
//
// This is the whole reason the claim carries the moment it was taken. The
// process that held it is gone; nothing will ever finish it; and with the claim
// stuck the queue skips the release and the interface shows a spinner that
// never stops.
func TestAClaimNobodyCanStillHoldIsReleased(t *testing.T) {
	h := newCacheHarness(t)
	id := h.seedPackageAt("orb_25.7.2131", time.Now().Add(-time.Hour))
	h.seedListedChild(id)

	if _, err := h.packages.ClaimAnalysis(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	// A claim taken a moment ago is a claim somebody is honouring.
	if n, err := h.packages.RecoverAnalyses(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("released %d live claims — a running walk had its release stolen", n)
	}

	// Backdated to what a crashed process leaves behind: a claim taken hours
	// ago with nothing running. Backdated rather than waited for, because the
	// alternative is a test that sleeps for the timeout it is checking.
	if _, err := h.packages.DB().ExecContext(t.Context(),
		`UPDATE packages SET analysis_started_at = ? WHERE id = ?`,
		time.Now().Add(-3*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"), id); err != nil {
		t.Fatal(err)
	}

	// One older than any walk takes is not being honoured by anybody.
	n, err := h.packages.RecoverAnalyses(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("released %d abandoned claims, want 1", n)
	}

	// RELEASED, not failed: nothing went wrong with the release, so the queue
	// must pick it up and finish the work.
	rows, err := h.packages.UnanalysedRecent(t.Context(), h.repoID, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("a release freed by the restart is not queued again: %+v", rows)
	}
}
