package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Many downloads at once, and the two things that must survive it.
//
// # What these hold, and what they cannot
//
// A production deployment runs dozens of transfers concurrently against a fleet
// of workers, while somebody watches the interface. Two properties have to hold
// and they fail in different ways:
//
//   - CORRECTNESS. A job handed to two workers is the same blob pushed twice,
//     two progress streams fighting over one row, and a transfer that can
//     complete before its work has. This is not a matter of degree, it is
//     asserted exactly, and it is what the tests below spend most of their
//     effort on.
//   - LIVENESS. The dequeue under contention must not deadlock, must not
//     starve one worker forever, and must not leave the reads the interface
//     polls waiting behind the writes. A regression here reads to a user as
//     "the page hangs while a big download runs".
//
// # Why SQLite proves less here than it looks
//
// internal/store/sqlite.go opens the pool with SetMaxOpenConns(1). Goroutines
// therefore SERIALIZE at the driver rather than contending in the database, so
// these tests exercise the real dequeue path and the real transaction
// boundaries but they do not reproduce Postgres's row-level contention. That
// makes them a correctness harness that also runs in CI, not a benchmark of
// production throughput.
//
// The distinction matters for reading a failure: a double-lease HERE is a
// genuine bug in the predicate, because serialized access is the easiest case
// there is. A deadlock here is a genuine bug for the same reason. What passing
// does NOT establish is a number, which is why nothing below asserts a
// duration.

// throughputHarness seeds a queue that looks like a busy morning: several
// transfers, each with many jobs, all leasable.
type throughputHarness struct {
	*controlHarness
	transfers []string
	jobs      int
}

func newThroughputHarness(t *testing.T, transfers, jobsEach int) *throughputHarness {
	t.Helper()

	h := &throughputHarness{controlHarness: newControlHarness(t), jobs: transfers * jobsEach}
	for i := range transfers {
		h.transfers = append(h.transfers, h.seedTransfer(i, jobsEach))
	}
	return h
}

// seedTransfer creates one running transfer with `jobs` leasable blob jobs.
//
// Its own seeding rather than controlHarness.runningTransfer, which repeats one
// of ten digests and so cannot exceed ten jobs against the unique index on
// (transfer_id, kind, digest, target_repo_id). A contention test needs hundreds,
// and every digest here is distinct across the whole fixture.
func (h *throughputHarness) seedTransfer(seq, jobs int) string {
	h.t.Helper()

	h.n++
	id := fmt.Sprintf("%08d-cccc-dddd-eeee-ffffffffffff", h.n)
	h.transfer(id)
	h.exec(`UPDATE transfers SET state='running', started_at='2026-01-01T00:00:00Z' WHERE id = ?`, id)

	// One transaction for the whole transfer. The SQLite pool is a single
	// connection, so a per-row store call here would be both slow and a
	// deadlock risk - see seedJobs in transferperf_test.go.
	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	insert := h.packages.dialect.Rewrite(
		`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                   target_repo_id, state, wave, max_attempts)
		 VALUES (?, 'blob', ?, 1024, ?, ?, 'pending', 0, 8)`)

	for j := range jobs {
		digest := fmt.Sprintf("sha256:%064x", seq*1_000_000+j)
		if _, err := tx.ExecContext(h.t.Context(), insert, id, digest, h.repoID, h.repoID); err != nil {
			h.t.Fatalf("seed job %d of transfer %s: %v", j, id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// leaseAs takes work the way one worker does.
func (h *throughputHarness) leaseAs(ctx context.Context, owner string, limit int) ([]LeasedJob, error) {
	return h.packages.LeaseJobs(ctx, LeaseRequest{
		Owner: owner, Limit: limit, Duration: time.Minute,
	})
}

// THE PROPERTY EVERYTHING ELSE RESTS ON. Under concurrent workers, no job is
// handed out twice.
//
// Asserted by identity rather than by count: a count would pass if the dequeue
// handed out job 7 twice and job 8 never, which is the exact failure a
// candidate-then-update dequeue produces when the two steps are not atomic.
func TestConcurrentWorkersNeverLeaseTheSameJobTwice(t *testing.T) {
	const workers = 12
	h := newThroughputHarness(t, 8, 25)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		seenBy = map[int64]string{}
		failed []error
	)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				leased, err := h.leaseAs(ctx, owner, 4)
				if err != nil {
					// A cancelled context at the end of the run is the harness
					// shutting down, not a fault.
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					mu.Lock()
					failed = append(failed, fmt.Errorf("%s: %w", owner, err))
					mu.Unlock()
					return
				}
				if len(leased) == 0 {
					return // drained
				}

				mu.Lock()
				for _, j := range leased {
					if prev, dup := seenBy[j.ID]; dup {
						failed = append(failed, fmt.Errorf(
							"job %d leased by %s and again by %s", j.ID, prev, owner))
					}
					seenBy[j.ID] = owner
				}
				mu.Unlock()
			}
		}("worker-" + strconv.Itoa(w))
	}
	wg.Wait()

	for _, err := range failed {
		t.Error(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the dequeue did not drain within the deadline: %v - "+
			"that is a deadlock or a livelock, not slowness", ctx.Err())
	}
	if got, want := len(seenBy), h.jobs; got != want {
		t.Errorf("leased %d distinct jobs, want %d - some work was never handed out", got, want)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'pending'`); n != 0 {
		t.Errorf("%d jobs are still pending after every worker drained the queue", n)
	}
}

// Every job ends up leased exactly once AND the rows agree.
//
// The map above is what the workers were told; this is what the database
// believes. They have to be the same, or a worker is doing work the store will
// hand to somebody else when the lease expires.
func TestConcurrentLeasingLeavesTheRowsConsistent(t *testing.T) {
	const workers = 8
	h := newThroughputHarness(t, 6, 20)

	drainConcurrently(t, h, workers, 5)

	if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'leased'`); n != h.jobs {
		t.Errorf("%d job rows are leased, want %d", n, h.jobs)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'leased' AND lease_owner IS NULL`); n != 0 {
		t.Errorf("%d leased jobs have no owner", n)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'leased' AND lease_expires_at IS NULL`); n != 0 {
		t.Errorf("%d leased jobs have no expiry, so the reaper can never recover them", n)
	}
}

// COMPLETING under contention. Workers lease and complete concurrently, and the
// transfers must all finish - no job stuck leased, no transfer stuck running.
//
// This is the one that catches a completion path that races with the wave
// advance: a transfer whose last job succeeds while another worker is opening
// the next wave can be left running with nothing left to run.
func TestConcurrentLeaseAndCompleteFinishesEveryTransfer(t *testing.T) {
	const workers = 10
	h := newThroughputHarness(t, 5, 16)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var (
		mu        sync.Mutex
		completed int
		failed    []error
	)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				leased, err := h.leaseAs(ctx, owner, 3)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					mu.Lock()
					failed = append(failed, err)
					mu.Unlock()
					return
				}
				if len(leased) == 0 {
					return
				}
				for _, j := range leased {
					if _, err := h.packages.CompleteJob(ctx, Completion{
						JobID: j.ID, Owner: owner, Outcome: "succeeded",
						Attempt: 1, BytesTransferred: 1024,
					}); err != nil {
						mu.Lock()
						failed = append(failed, fmt.Errorf("complete %d: %w", j.ID, err))
						mu.Unlock()
						return
					}
					mu.Lock()
					completed++
					mu.Unlock()
				}
			}
		}("worker-" + strconv.Itoa(w))
	}
	wg.Wait()

	for _, err := range failed {
		t.Error(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("lease-and-complete did not drain within the deadline: %v", ctx.Err())
	}
	if completed != h.jobs {
		t.Errorf("completed %d jobs, want %d", completed, h.jobs)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE state NOT IN ('succeeded','skipped')`); n != 0 {
		t.Errorf("%d jobs did not reach a terminal state", n)
	}

	// And no transfer is left believing it still has work.
	for _, id := range h.transfers {
		if state := h.state(id); state == "running" || state == "ready" {
			t.Errorf("transfer %s is still %q with every job finished", id, state)
		}
	}
}

// THE INTERFACE STAYS ANSWERABLE while the fleet is busy.
//
// A reader polling the transfer listing must keep getting answers while workers
// hammer the dequeue. What is asserted is that every read COMPLETES and returns
// a consistent answer - not how fast, because the pool here is one connection
// and a duration on a loaded CI box is noise. The shape of the query is
// asserted separately by TestTransferListingQueryPlan, which is where a
// regression to a full scan is caught.
func TestListingStaysAnswerableWhileWorkersDrainTheQueue(t *testing.T) {
	h := newThroughputHarness(t, 10, 20)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	readerDone := make(chan struct{})
	var (
		mu      sync.Mutex
		reads   int
		errs    []error
		slowest time.Duration
	)

	go func() {
		defer close(readerDone)
		for ctx.Err() == nil {
			start := time.Now()
			var n int
			err := h.st.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM transfers WHERE state IN ('running','ready')`).Scan(&n)
			took := time.Since(start)

			mu.Lock()
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				errs = append(errs, err)
			} else if err == nil {
				reads++
				if took > slowest {
					slowest = took
				}
			}
			mu.Unlock()

			if err != nil {
				return
			}
		}
	}()

	drainConcurrently(t, h, 8, 4)
	cancel()
	<-readerDone

	mu.Lock()
	defer mu.Unlock()
	for _, err := range errs {
		t.Errorf("the listing failed while the queue was busy: %v", err)
	}
	if reads == 0 {
		t.Fatal("the reader never completed a single query while workers were leasing - " +
			"reads are being starved by writes")
	}
	t.Logf("listing answered %d times while %d jobs drained; slowest read %s",
		reads, h.jobs, slowest.Round(time.Microsecond))
}

// A worker that dies mid-lease must not strand its work.
//
// The reaper is what makes a crashed worker survivable, and it is only
// meaningful under contention: the jobs it recovers have to become leasable
// again for SOMEBODY ELSE, not just be marked pending.
func TestWorkFromACrashedWorkerBecomesLeasableAgain(t *testing.T) {
	h := newThroughputHarness(t, 2, 6)

	leased, err := h.packages.LeaseJobs(t.Context(), LeaseRequest{
		Owner: "doomed", Limit: 4, Duration: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leased) == 0 {
		t.Fatal("the fixture leased nothing; the test would prove nothing")
	}

	// The lease is already expired; the reaper is what notices.
	time.Sleep(5 * time.Millisecond)
	reaped, err := h.packages.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) == 0 {
		t.Fatal("nothing was reaped, so a crashed worker's jobs would be stranded")
	}

	recovered, err := h.leaseAs(t.Context(), "survivor", len(leased))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) == 0 {
		t.Error("the reaped jobs were not leasable by another worker")
	}
}

// drainConcurrently runs `workers` goroutines leasing until the queue is empty.
func drainConcurrently(t *testing.T, h *throughputHarness, workers, batch int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		failed []error
	)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for ctx.Err() == nil {
				leased, err := h.leaseAs(ctx, owner, batch)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					mu.Lock()
					failed = append(failed, err)
					mu.Unlock()
					return
				}
				if len(leased) == 0 {
					return
				}
			}
		}("worker-" + strconv.Itoa(w))
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, err := range failed {
		t.Error(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("draining did not finish within the deadline: %v", ctx.Err())
	}
}
