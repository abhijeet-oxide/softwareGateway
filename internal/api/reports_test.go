package api

import (
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// seedReports writes one succeeded copy transfer that moved 1000 bytes over
// ten seconds and skipped 3000, so every derived figure has a known answer.
func (h *apiHarness) seedReports() {
	h.t.Helper()
	pkg := h.seedPackage("25.8.1", "sha256:"+
		"1111111111111111111111111111111111111111111111111111111111111111")

	h.exec(`INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
	        VALUES ('r1',?,?,'replicate',?,'k1')`, h.productID, pkg, h.repoID)
	h.exec(`INSERT INTO transfers
	    (id,request_id,package_id,source_repo_id,target_repo_id,state,strategy,
	     dedupe_skipped_bytes,started_at,completed_at)
	  VALUES ('t1','r1',?,?,?,'succeeded','copy',500,
	          '2026-08-10T10:00:00.000Z','2026-08-10T10:00:10.000Z')`,
		pkg, h.repoID, h.repoID)
	h.exec(`INSERT INTO jobs (transfer_id,kind,digest,size_bytes,source_repo_id,target_repo_id,state,bytes_transferred)
	        VALUES ('t1','blob','sha256:b1',1000,?,?,'succeeded',1000),
	               ('t1','blob','sha256:b2',3000,?,?,'skipped',0)`,
		h.repoID, h.repoID, h.repoID, h.repoID)
}

func TestReportSummaryTotalsMatchPerProduct(t *testing.T) {
	h := newAPIHarness(t)
	h.seedReports()

	out := getJSON[v1.ReportSummary](t,
		h.server.URL+"/api/v1/reports/summary?since=2026-08-01T00:00:00Z")

	if len(out.Products) != 1 {
		t.Fatalf("got %d products, want 1", len(out.Products))
	}
	if out.Products[0].Product != "vendor-a" {
		t.Errorf("product = %q", out.Products[0].Product)
	}
	// The estate total is the sum of the per-product rows, so with one product
	// they must be identical. Anything else means two code paths.
	// Compared as JSON because the derived figures are pointers, and pointer
	// equality would pass this test for two structs holding different numbers
	// and fail it for two holding the same.
	estate, _ := json.Marshal(out.Totals)
	only, _ := json.Marshal(out.Products[0].Totals)
	if string(estate) != string(only) {
		t.Errorf("estate totals %s differ from the only product's %s", estate, only)
	}
	if out.Totals.DownloadsCompleted != 1 {
		t.Errorf("completed = %d, want 1", out.Totals.DownloadsCompleted)
	}
	if out.Totals.BytesTransferred != "1000" {
		t.Errorf("bytesTransferred = %q, want 1000", out.Totals.BytesTransferred)
	}
	if out.Totals.SavedBytes != "3500" {
		t.Errorf("savedBytes = %q, want 3500 (500 deduped + 3000 skipped)",
			out.Totals.SavedBytes)
	}
	// 1000 bytes over 10 seconds.
	if out.Totals.AverageBytesPerSecond == nil {
		t.Error("no averageBytesPerSecond for a transfer we timed")
	} else if *out.Totals.AverageBytesPerSecond != "100" {
		t.Errorf("averageBytesPerSecond = %q, want 100", *out.Totals.AverageBytesPerSecond)
	}
	if out.Period.Since == "" {
		t.Error("the period the server applied was not echoed back")
	}
}

// The honest-numbers rule, at the only place it can be tested from outside: a
// period in which nothing was measured must OMIT the speed, not report zero.
func TestReportSummaryOmitsSpeedItDidNotMeasure(t *testing.T) {
	h := newAPIHarness(t)
	h.seedReports()

	out := getJSON[v1.ReportSummary](t,
		h.server.URL+"/api/v1/reports/summary?since=2026-09-01T00:00:00Z")

	if out.Totals.AverageBytesPerSecond != nil {
		t.Errorf("a speed of %q was stated for a period with no transfers",
			*out.Totals.AverageBytesPerSecond)
	}
	if out.Totals.SavedPercent != nil {
		t.Errorf("a saved percentage of %v was stated with nothing in play",
			*out.Totals.SavedPercent)
	}
	if out.Totals.SuccessRate != nil {
		t.Errorf("a success rate of %v was stated with nothing settled",
			*out.Totals.SuccessRate)
	}
	if out.Totals.DownloadsCompleted != 0 {
		t.Errorf("completed = %d, want 0", out.Totals.DownloadsCompleted)
	}
}

func TestReportSummaryDefaultsToThirtyDays(t *testing.T) {
	h := newAPIHarness(t)

	out := getJSON[v1.ReportSummary](t, h.server.URL+"/api/v1/reports/summary")
	if out.Period.Label != "30d" {
		t.Errorf("label = %q, want the 30d default stated explicitly", out.Period.Label)
	}
	if out.Period.Since == "" {
		t.Error("a default period was applied without saying what it was")
	}
}

// Two spellings of one window is a caller mistake worth naming, not one to
// resolve by precedence and show them a period they did not ask for.
func TestReportSummaryRejectsTwoSpellingsOfThePeriod(t *testing.T) {
	h := newAPIHarness(t)

	resp, err := http.Get( //nolint:noctx // test
		h.server.URL + "/api/v1/reports/summary?period=7d&since=2026-08-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Code != v1.CodeInvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", p.Code)
	}
}
