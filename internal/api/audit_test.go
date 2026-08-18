package api

import (
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// getJSON fetches a path and decodes it, failing on a non-200.
func getJSON[T any](t *testing.T, url string) T {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

func (h *apiHarness) seedAuditEvents() {
	h.t.Helper()
	h.exec(`INSERT INTO audit_events
	    (occurred_at,event_type,actor,actor_kind,product_name,subject_kind,subject_id,outcome,detail)
	  VALUES ('2026-08-10T10:00:00.000Z','PackageDiscovered','system','system','vendor-a','package','25.8.1','success','{"tag":"25.8.1"}'),
	         ('2026-08-10T11:00:00.000Z','DownloadRunRequested','ab','user','vendor-a','package','25.8.1','success',NULL),
	         ('2026-08-10T12:00:00.000Z','PackageDiscovered','system','system','vendor-a','package','25.7.4','failure','not json at all')`)
}

func TestAuditEventsRouteReturnsNewestFirst(t *testing.T) {
	h := newAPIHarness(t)
	h.seedAuditEvents()

	out := getJSON[v1.ListAuditEventsResponse](t, h.server.URL+"/api/v1/auditEvents")
	if len(out.AuditEvents) != 3 {
		t.Fatalf("got %d events, want 3", len(out.AuditEvents))
	}
	if out.AuditEvents[0].SubjectID != "25.7.4" {
		t.Errorf("first event is %q, want the newest (25.7.4)",
			out.AuditEvents[0].SubjectID)
	}
	if out.AuditEvents[0].Name != "auditEvents/"+out.AuditEvents[0].ID {
		t.Errorf("resource name %q does not match id %q",
			out.AuditEvents[0].Name, out.AuditEvents[0].ID)
	}
}

// A row whose detail is not JSON must not break the page. It was written a
// year ago by something that has since been fixed, and the other 40 000 rows
// are still worth reading.
func TestAuditEventsSurviveUnparseableDetail(t *testing.T) {
	h := newAPIHarness(t)
	h.seedAuditEvents()

	out := getJSON[v1.ListAuditEventsResponse](t, h.server.URL+"/api/v1/auditEvents")
	if len(out.AuditEvents) != 3 {
		t.Fatalf("got %d events, want 3", len(out.AuditEvents))
	}
	if out.AuditEvents[0].Detail != nil {
		t.Errorf("non-JSON detail was passed through as %s", out.AuditEvents[0].Detail)
	}
	// The row that DOES carry JSON keeps it.
	if string(out.AuditEvents[2].Detail) != `{"tag":"25.8.1"}` {
		t.Errorf("valid detail = %s, want it preserved", out.AuditEvents[2].Detail)
	}
}

func TestAuditEventsFilterAndPaginate(t *testing.T) {
	h := newAPIHarness(t)
	h.seedAuditEvents()

	byType := getJSON[v1.ListAuditEventsResponse](t,
		h.server.URL+"/api/v1/auditEvents?eventType=DownloadRunRequested")
	if len(byType.AuditEvents) != 1 || byType.AuditEvents[0].Actor != "ab" {
		t.Errorf("eventType filter returned %+v", byType.AuditEvents)
	}

	first := getJSON[v1.ListAuditEventsResponse](t, h.server.URL+"/api/v1/auditEvents?pageSize=2")
	if len(first.AuditEvents) != 2 {
		t.Fatalf("page held %d events, want 2", len(first.AuditEvents))
	}
	if first.NextPageToken == "" {
		t.Fatal("no nextPageToken with a third row waiting")
	}

	second := getJSON[v1.ListAuditEventsResponse](t,
		h.server.URL+"/api/v1/auditEvents?pageSize=2&pageToken="+first.NextPageToken)
	if len(second.AuditEvents) != 1 {
		t.Errorf("second page held %d events, want 1", len(second.AuditEvents))
	}
	if second.NextPageToken != "" {
		t.Error("a nextPageToken was offered past the end of the trail")
	}
}

// A filter the server cannot honour is rejected rather than ignored: silently
// dropping it shows a wider trail than was asked for, and the caller cannot
// tell from the response.
func TestAuditEventsRejectABadTimeBound(t *testing.T) {
	h := newAPIHarness(t)

	resp, err := http.Get(h.server.URL + "/api/v1/auditEvents?since=last-tuesday") //nolint:noctx // test
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

// Nothing is authenticated yet, so the answer is anonymous — and the endpoint
// has to say so rather than imply a session.
func TestWhoAmIReportsAnonymousHonestly(t *testing.T) {
	h := newAPIHarness(t)

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")
	if out.Subject != "anonymous" {
		t.Errorf("subject = %q, want anonymous", out.Subject)
	}
	if out.Method != "none" {
		t.Errorf("method = %q, want none", out.Method)
	}
	if out.Authenticated {
		t.Error("authenticated is true on a Coordinator with no authenticator")
	}
	if len(out.Permissions) != 1 || out.Permissions[0] != "*" {
		t.Errorf("permissions = %v, want [*]", out.Permissions)
	}
	if len(out.Products) != 0 {
		t.Errorf("products = %v, want empty meaning unrestricted", out.Products)
	}
}
