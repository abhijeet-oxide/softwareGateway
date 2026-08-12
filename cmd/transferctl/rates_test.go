package main

import (
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// SPEED and ETA are questions about NOW. The average over the whole transfer is
// not an answer to either, and printing it as though it were produced a listing
// that contradicted itself on one line: 581.9 KiB/s and ~1m22s next to a
// RUNNING column reading 0, on a transfer that had been motionless for hours.

func TestSpeedIsNotReportedWhenNothingIsRunning(t *testing.T) {
	tr := stalledTransfer()

	if got := speedOf(tr, 0); got != "-" {
		t.Errorf("speed = %q with nothing in flight, want a dash", got)
	}
	// Even a live sample must not resurrect it: a rate measured a moment ago
	// describes a moment ago.
	if got := speedOf(tr, 595_000); got != "-" {
		t.Errorf("speed = %q with nothing in flight, want a dash", got)
	}
}

// "stalled" and "waiting" need different responses — one starts again on its
// own, the other will not — so the ETA column says which rather than shrugging.
func TestTheETASaysWhyThereIsNoEstimate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1.Transfer)
		want   string
	}{
		{"nothing running and nothing waiting", func(*v1.Transfer) {}, "stalled"},
		{"jobs in retry backoff", func(tr *v1.Transfer) {
			tr.Progress.JobsWaiting = 3
		}, "waiting"},
		{"nothing left to do", func(tr *v1.Transfer) {
			tr.Progress.JobsOutstanding = 0
		}, "-"},
	}

	for _, tc := range cases {
		tr := stalledTransfer()
		tc.mutate(tr)
		if got := etaOf(tr, 0); got != tc.want {
			t.Errorf("%s: eta = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// And a transfer that IS running still gets a real estimate — the fix must not
// blank the column for everyone.
func TestARunningTransferStillEstimates(t *testing.T) {
	tr := stalledTransfer()
	tr.Progress.JobsInFlight = 4

	if got := speedOf(tr, 595_000); got == "-" {
		t.Error("speed is blank for a transfer with four jobs in flight")
	}
	if got := etaOf(tr, 595_000); got == "stalled" || got == "-" {
		t.Errorf("eta = %q for a transfer with four jobs in flight", got)
	}
}

// stalledTransfer is the shape observed: everything but the last few percent
// copied, a long elapsed time, and nothing at all in flight.
func stalledTransfer() *v1.Transfer {
	return &v1.Transfer{
		ID:        "281614ab-e88a-452e-87f1-8af51ae8fef8",
		State:     v1.TransferRunning,
		StartedAt: "2026-08-11T00:00:00Z",
		Progress: v1.TransferProgress{
			JobsPlanned:      2493,
			JobsDone:         2286,
			JobsOutstanding:  207,
			JobsInFlight:     0,
			BytesTransferred: "68292942233",
			PlannedBytes:     "68400000000",
		},
	}
}
