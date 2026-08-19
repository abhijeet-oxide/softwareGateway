package store

import (
	"testing"
	"time"
)

// The fleet view, which until this existed had a table and no writer.

func TestAWorkerIsRecordedFromWhatItAsksFor(t *testing.T) {
	h := newRecoveryHarness(t)

	// A worker configured for 32 that already holds 5 asks for 27. The ceiling
	// an operator recognises is the sum, not the remainder.
	if err := h.packages.RecordWorker(t.Context(), WorkerRow{
		ID: "worker-1", Version: "1.4.0", MaxConcurrency: 32, ActiveJobs: 5,
	}); err != nil {
		t.Fatal(err)
	}

	workers, err := h.packages.ListWorkers(t.Context(), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 {
		t.Fatalf("listed %d worker(s), want 1", len(workers))
	}
	w := workers[0]
	if w.MaxConcurrency != 32 || w.ActiveJobs != 5 || w.Version != "1.4.0" {
		t.Errorf("worker = %+v, want 32/5 on 1.4.0", w)
	}
	if w.Stale {
		t.Error("a worker recorded just now is stale")
	}
}

// A heartbeat must not wipe the ceiling a lease recorded. Between leases - most
// of the time, on a multi-gigabyte blob - a heartbeat is the only signal, and
// zeroing capacity would make every busy worker report as having none.
func TestAHeartbeatKeepsTheRecordedCeiling(t *testing.T) {
	h := newRecoveryHarness(t)

	if err := h.packages.RecordWorker(t.Context(), WorkerRow{
		ID: "worker-1", Version: "1.4.0", MaxConcurrency: 32, ActiveJobs: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.packages.TouchWorker(t.Context(), "worker-1", 3); err != nil {
		t.Fatal(err)
	}

	workers, _ := h.packages.ListWorkers(t.Context(), 2*time.Minute)
	if workers[0].MaxConcurrency != 32 {
		t.Errorf("ceiling = %d after a heartbeat, want the 32 the lease recorded",
			workers[0].MaxConcurrency)
	}
	if workers[0].ActiveJobs != 3 {
		t.Errorf("active = %d, want the 3 the heartbeat reported", workers[0].ActiveJobs)
	}
}

// A worker that stopped talking is the thing the fleet view exists to show.
func TestAQuietWorkerIsReportedStale(t *testing.T) {
	h := newRecoveryHarness(t)
	if err := h.packages.RecordWorker(t.Context(), WorkerRow{
		ID: "worker-1", MaxConcurrency: 8,
	}); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workers SET last_heartbeat_at = '2020-01-01T00:00:00Z' WHERE id = ?`, "worker-1")

	workers, _ := h.packages.ListWorkers(t.Context(), 2*time.Minute)
	if !workers[0].Stale {
		t.Error("a worker silent since 2020 was not reported stale")
	}
}

// LEASED IS COUNTED FROM THE JOBS, NOT FROM THE WORKER'S CLAIM.
//
// The two disagreeing is worth seeing: a worker still working on jobs the
// Coordinator has already reaped is about to report completions nobody will
// accept.
func TestLeasedJobsAreCountedFromTheQueue(t *testing.T) {
	h := newRecoveryHarness(t)
	id := h.transferWithJobs(3) // leased to worker-1 by the fixture

	if err := h.packages.RecordWorker(t.Context(), WorkerRow{
		ID: "worker-1", MaxConcurrency: 32, ActiveJobs: 99,
	}); err != nil {
		t.Fatal(err)
	}

	workers, _ := h.packages.ListWorkers(t.Context(), 2*time.Minute)
	if workers[0].LeasedJobs != 3 {
		t.Errorf("leased = %d, want the 3 the queue actually holds", workers[0].LeasedJobs)
	}
	if workers[0].ActiveJobs != 99 {
		t.Errorf("active = %d, want the worker's own claim kept as reported",
			workers[0].ActiveJobs)
	}
	_ = id
}
