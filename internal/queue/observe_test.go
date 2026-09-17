package queue_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/store/storetest"
)

// The reading these have to protect.
//
// A registry having a bad day fails forty in-flight jobs with a 503, each of
// which is retried up to eight times. Counting every one of those as a
// completed failure reports three hundred permanent failures from an incident
// that resolved itself, and a failure-rate panel built on it says the fleet is
// broken when it is working exactly as designed. So jobs_completed_total takes
// terminal outcomes only, and the retries are job_errors_total's business.

func TestACompletionCountsOnceItIsTerminal(t *testing.T) {
	h := newObserveHarness(t)

	// maxAttempts 3, failing twice: both retried, neither terminal.
	job := h.job(1024)
	h.fail(job, "transient", 0, 3)
	h.fail(job, "transient", 1, 3)

	body := h.exposition()
	if strings.Contains(body, `jobs_completed_total{kind="blob",outcome="failed"}`) {
		t.Errorf("a retried failure was counted as a completed job.\n\n%s\n"+
			"A job with attempts left went back on the queue and is still outstanding\n"+
			"work; queue_jobs is what counts it. Counting it here turns one bad\n"+
			"afternoon at a registry into a permanent-failure graph.", body)
	}
	if want := `job_errors_total{class="transient",kind="blob"} 2`; !strings.Contains(body, want) {
		t.Errorf("expected %q - retries are exactly what this counter is for.\n\n%s", want, body)
	}

	// The third exhausts the budget.
	h.fail(job, "transient", 2, 3)
	body = h.exposition()
	if want := `jobs_completed_total{kind="blob",outcome="failed"} 1`; !strings.Contains(body, want) {
		t.Errorf("expected %q once attempts were exhausted.\n\n%s", want, body)
	}
	if want := `job_errors_total{class="transient",kind="blob"} 3`; !strings.Contains(body, want) {
		t.Errorf("expected %q - the terminal attempt is a failure too.\n\n%s", want, body)
	}
}

func TestBytesSavedAreNotCountedAsThroughput(t *testing.T) {
	h := newObserveHarness(t)

	moved := h.job(4096)
	h.complete(moved, store.Completion{Outcome: "succeeded", BytesTransferred: 4096, Placed: true})

	skipped := h.job(8192)
	h.complete(skipped, store.Completion{
		Outcome: "skipped", SkipReason: "placement_hit", Placed: true,
	})

	body := h.exposition()
	for _, want := range []string{
		`job_bytes_total{disposition="transferred"} 4096`,
		`job_bytes_total{disposition="saved"} 8192`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q.\n\n%s\n"+
				"A skip moves nothing and reports zero bytes transferred, so the\n"+
				"quantity to count for it is the descriptor's size. Added to the\n"+
				"transferred total it would inflate throughput; left out entirely it\n"+
				"would hide the number that justifies this system existing.", want, body)
		}
	}
	if want := `jobs_completed_total{kind="blob",outcome="skipped"} 1`; !strings.Contains(body, want) {
		t.Errorf("expected %q.\n\n%s", want, body)
	}
}

// TestAQueueWithoutMetricsStillWorks covers the wiring a test and the worker's
// in-process harness use.
func TestAQueueWithoutMetricsStillWorks(t *testing.T) {
	h := newObserveHarness(t)
	h.queue = queue.New(h.packages, 0, discardLogger()) // no WithMetrics

	job := h.job(1)
	h.complete(job, store.Completion{Outcome: "succeeded", BytesTransferred: 1, Placed: true})
}

type observeHarness struct {
	t        *testing.T
	packages *store.Packages
	queue    *queue.Queue
	metrics  *metrics.Registry
	st       store.Store
	repoID   int64
	transfer string
	n        int
}

func newObserveHarness(t *testing.T) *observeHarness {
	t.Helper()

	st := storetest.Open(t)
	packages := store.NewPackages(st)
	m := metrics.New("coordinator")
	h := &observeHarness{
		t: t, st: st, packages: packages, metrics: m,
		queue:    queue.New(packages, 0, discardLogger()).WithMetrics(m),
		transfer: "11111111-aaaa-bbbb-cccc-dddddddddddd",
	}

	res, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a','h','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	productID, _ := res.LastInsertId()

	tx, err := st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	h.repoID, err = packages.EnsureRepository(t.Context(), tx, productID, "source", "src",
		"registry.example.com", "vendor/suite", "generic", "config", "")
	if err != nil {
		t.Fatal(err)
	}
	pkgID, err := packages.InsertPackage(t.Context(), tx, store.PackageRow{
		ProductID: productID, SourceRepoID: h.repoID, Tag: "v1",
		ManifestDigest: "sha256:" + strings.Repeat("a", 64), MediaType: "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	h.exec(`INSERT INTO transfer_requests (id, product_id, package_id, operation,
	                                       source_repo_id, idempotency_key)
	         VALUES ('22222222-eeee-ffff-0000-111111111111', ?, ?, 'replicate', ?, 'k1')`,
		productID, pkgID, h.repoID)
	h.exec(`INSERT INTO transfers (id, request_id, package_id, source_repo_id,
	                               target_repo_id, state)
	         VALUES (?, '22222222-eeee-ffff-0000-111111111111', ?, ?, ?, 'running')`,
		h.transfer, pkgID, h.repoID, h.repoID)
	return h
}

// job inserts a leased job owned by "w1", which is what a completion requires.
func (h *observeHarness) job(size int64) int64 {
	h.t.Helper()
	h.n++
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
	                          target_repo_id, state, lease_owner, attempts, max_attempts,
	                          started_at)
	         VALUES (?, 'blob', ?, ?, ?, ?, 'leased', 'w1', 0, 8, `+
		h.packages.Dialect().TimeAgo("5")+`)`,
		h.transfer, fmt.Sprintf("sha256:%064d", h.n), size, h.repoID, h.repoID)

	var id int64
	if err := h.st.DB().QueryRowContext(h.t.Context(),
		`SELECT MAX(id) FROM jobs`).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *observeHarness) complete(jobID int64, c store.Completion) store.CompletionResult {
	h.t.Helper()
	c.JobID, c.Owner = jobID, "w1"
	res, err := h.queue.Complete(context.Background(), c)
	if err != nil {
		h.t.Fatal(err)
	}
	if !res.Applied {
		h.t.Fatalf("completion of job %d was not applied", jobID)
	}
	return res
}

// fail reports one failed attempt and re-leases the job, the way a worker
// picking it up again after the backoff would.
func (h *observeHarness) fail(jobID int64, class string, attempt, maxAttempts int) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET state='leased', lease_owner='w1', attempts=? WHERE id=?`,
		attempt+1, jobID)
	h.complete(jobID, store.Completion{
		Outcome: "failed", ErrorClass: class, ErrorMsg: "boom",
		Attempt: attempt, MaxAttempts: maxAttempts,
	})
}

func (h *observeHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(
		h.t.Context(), h.packages.Dialect().Rewrite(query), args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *observeHarness) exposition() string {
	h.t.Helper()
	families, err := h.metrics.Prometheus().Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range families {
		name := strings.TrimPrefix(f.GetName(), "softwaregateway_")
		if name == f.GetName() {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.Counter == nil {
				continue
			}
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			line := name
			if len(labels) > 0 {
				line += "{" + strings.Join(labels, ",") + "}"
			}
			fmt.Fprintf(&b, "%s %g\n", line, m.Counter.GetValue())
		}
	}
	return b.String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
