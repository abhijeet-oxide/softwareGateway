package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The retry route, and specifically the routing — a custom method on a
// resource whose ID is a UUID is the shape most likely to be matched at the
// wrong colon.

// fakeQueue records what the handler asked the queue to do.
type fakeQueue struct {
	retried  []string
	result   store.RetryResult
	err      error
	recorded []store.WorkerRow
	workers  []store.WorkerSummary
}

func (f *fakeQueue) RecordWorker(_ context.Context, id string, capacity, active int, version string) {
	f.recorded = append(f.recorded, store.WorkerRow{
		ID: id, Version: version, MaxConcurrency: capacity + active, ActiveJobs: active,
	})
}

func (f *fakeQueue) Workers(context.Context) ([]store.WorkerSummary, error) {
	return f.workers, nil
}

func (f *fakeQueue) Lease(context.Context, string, int) (queue.LeaseResult, error) {
	return queue.LeaseResult{}, nil
}
func (f *fakeQueue) Progress(context.Context, int64, string, int64) error { return nil }
func (f *fakeQueue) Complete(context.Context, store.Completion) (store.CompletionResult, error) {
	return store.CompletionResult{}, nil
}
func (f *fakeQueue) Heartbeat(context.Context, string, []int64) ([]int64, error) { return nil, nil }

func (f *fakeQueue) Retry(_ context.Context, transferID string) (store.RetryResult, error) {
	f.retried = append(f.retried, transferID)
	if f.err != nil {
		return store.RetryResult{}, f.err
	}
	res := f.result
	res.TransferID = transferID
	return res, nil
}

func newRetryHarness(t *testing.T) (*httptest.Server, *fakeQueue, *apiHarness) {
	t.Helper()

	h := newAPIHarness(t)
	q := &fakeQueue{result: store.RetryResult{Requeued: 4, State: "ready"}}

	srv := NewServer(Deps{
		Products:  nil,
		Store:     h.store,
		Packages:  h.packages,
		Queue:     q,
		Component: "coordinator",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, q, h
}

func TestRetryRouteAcceptsAShortTransferID(t *testing.T) {
	ts, q, h := newRetryHarness(t)
	full := h.seedTransfer("4d882940-1111-2222-3333-444444444444")

	// The listing prints the short form and tells the reader to use it. A
	// route that then refused it would be the same trap `describe` already
	// had to fix.
	resp := postJSON(t, ts.URL+"/api/v1/transfers/4d882940:retry")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(q.retried) != 1 || q.retried[0] != full {
		t.Errorf("queue was asked to retry %v, want the resolved %s", q.retried, full)
	}

	var out v1.RetryTransferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Requeued != 4 || len(out.Transfers) != 1 {
		t.Errorf("response = %+v, want one transfer with 4 requeued", out)
	}
	if out.Transfers[0].State != "ready" {
		t.Errorf("state = %q, want ready", out.Transfers[0].State)
	}
}

// A transfer ID is a UUID and contains no colon, but the verb split must not
// DEPEND on that: it is a trap set for whoever changes the identifier.
func TestRetryRouteSplitsTheVerbAtTheLastColon(t *testing.T) {
	ts, q, h := newRetryHarness(t)
	full := h.seedTransfer("9c1e8f2a-5555-6666-7777-888888888888")

	resp := postJSON(t, ts.URL+"/api/v1/transfers/"+full+":retry")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(q.retried) != 1 || q.retried[0] != full {
		t.Errorf("retried %v, want %s", q.retried, full)
	}
}

// The verbs that are specified but unbuilt are the ones somebody reaches for
// first. Saying so beats a bare "unknown method", which reads as a typo.
func TestUnbuiltVerbsSayTheyAreUnbuilt(t *testing.T) {
	ts, _, h := newRetryHarness(t)
	full := h.seedTransfer("4d882940-1111-2222-3333-444444444444")

	resp := postJSON(t, ts.URL+"/api/v1/transfers/"+full+":pause")
	defer func() { _ = resp.Body.Close() }()

	body, _ := readBody(resp)
	if !strings.Contains(body, "not built") {
		t.Errorf("pause returned %q, want it to say the verb is not built yet", body)
	}
	if !strings.Contains(body, "retry") {
		t.Errorf("the message does not point at the verb that IS available: %q", body)
	}
}

func TestRetryOfAnUnknownTransferIsNotFound(t *testing.T) {
	ts, q, _ := newRetryHarness(t)

	resp := postJSON(t, ts.URL+"/api/v1/transfers/deadbeef:retry")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if len(q.retried) != 0 {
		t.Errorf("the queue was asked to retry %v for a transfer that does not exist", q.retried)
	}
}

// The fleet-wide form: after an outage the caller wants everything restartable
// restarted, and one transfer that cannot be must not abandon the rest.
func TestFleetRetryReportsPerTransferRatherThanFailingWhole(t *testing.T) {
	ts, q, h := newRetryHarness(t)
	a := h.seedTransfer("4d882940-1111-2222-3333-444444444444")
	b := h.seedTransfer("9c1e8f2a-5555-6666-7777-888888888888")
	h.failEveryJob(a)
	h.failEveryJob(b)

	resp := postJSON(t, ts.URL+"/api/v1/transfers:retry")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out v1.RetryTransferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Transfers) != 2 {
		t.Fatalf("acted on %d transfer(s), want both", len(out.Transfers))
	}
	if out.Requeued != 8 {
		t.Errorf("requeued total = %d, want 8 across two transfers", out.Requeued)
	}
	if len(q.retried) != 2 {
		t.Errorf("queue calls = %v, want one per transfer", q.retried)
	}
}

func postJSON(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(resp *http.Response) (string, error) {
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// seedTransfer inserts a transfer with four blob jobs, directly, so a handler
// test does not need a planner.
func (h *apiHarness) seedTransfer(id string) string {
	h.t.Helper()

	pkgID := h.seedPackage("v"+id[:4], "sha256:"+strings.Repeat("b", 64))

	h.exec(`INSERT INTO transfer_requests (id, product_id, package_id, operation,
	                                       source_repo_id, idempotency_key)
	         VALUES (?, ?, ?, 'replicate', ?, ?)`,
		"req-"+id, h.productID, pkgID, h.repoID, "key-"+id)
	h.exec(`INSERT INTO transfers (id, request_id, package_id, source_repo_id, target_repo_id,
	                               state, priority)
	         VALUES (?, ?, ?, ?, ?, 'running', 50)`,
		id, "req-"+id, pkgID, h.repoID, h.repoID)

	for i := range 4 {
		h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                          target_repo_id, state, wave, attempts, max_attempts)
		         VALUES (?, 'blob', ?, 1024, ?, ?, 'pending', 0, 0, 8)`,
			id, "sha256:"+strings.Repeat(string(rune('0'+i)), 64), h.repoID, h.repoID)
	}
	return id
}

func (h *apiHarness) failEveryJob(transferID string) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET state='failed', attempts=8 WHERE transfer_id = ?`, transferID)
}

func (h *apiHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.store.DB().ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}
