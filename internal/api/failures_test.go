package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The route, the short-ID resolution and the shape of the answer. The grouping
// itself is proven in the store; what matters here is that a caller who typed
// the ID the listing printed gets causes back rather than rows.
func TestFailuresRouteSummarisesByCause(t *testing.T) {
	ts, _, h := newRetryHarness(t)
	full := h.seedTransfer("4d882940-1111-2222-3333-444444444444")

	h.exec(`UPDATE jobs SET state='failed', attempts=8,
	            last_error = 'push manifest ' || digest || ': HTTP 400: manifest invalid',
	            last_error_class = 'unsupported'
	         WHERE transfer_id = ?`, full)

	resp, err := http.Get(ts.URL + "/api/v1/transfers/4d882940/failures") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out v1.ListFailuresResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.TransferID != full {
		t.Errorf("transferId = %q, want the resolved %s", out.TransferID, full)
	}
	if len(out.Failures) != 1 {
		t.Fatalf("returned %d causes for four jobs failing identically, want 1", len(out.Failures))
	}

	g := out.Failures[0]
	if g.Failed != 4 {
		t.Errorf("the cause holds %d jobs, want the 4 seeded", g.Failed)
	}
	if g.Retryable {
		t.Error("an unsupported rejection was reported as worth retrying")
	}
	if !strings.Contains(g.Message, "manifest invalid") {
		t.Errorf("message %q does not carry what the registry said", g.Message)
	}
	if g.ExampleDigest == "" {
		t.Error("no example job to go and look at")
	}
}

func TestFailuresOfAHealthyTransferIsEmptyRatherThanAnError(t *testing.T) {
	ts, _, h := newRetryHarness(t)
	h.seedTransfer("9c1e8f2a-5555-6666-7777-888888888888")

	resp, err := http.Get(ts.URL + "/api/v1/transfers/9c1e8f2a/failures") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out v1.ListFailuresResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Failures) != 0 {
		t.Errorf("a transfer with nothing failing reported %+v", out.Failures)
	}
}

func TestFailuresOfAnUnknownTransferIsNotFound(t *testing.T) {
	ts, _, _ := newRetryHarness(t)

	resp, err := http.Get(ts.URL + "/api/v1/transfers/deadbeef/failures") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
