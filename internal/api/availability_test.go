package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/store/storetest"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func availabilityServer(t *testing.T) (http.Handler, *store.Availability) {
	t.Helper()
	st := storetest.Open(t)
	av := store.NewAvailability(st)
	return NewServer(Deps{
		Logger:       slog.New(slog.DiscardHandler),
		Products:     product.NewRegistry(),
		Component:    "coordinator",
		Availability: av,
	}).Handler(), av
}

func getAvailability(t *testing.T, h http.Handler, query string) v1.AvailabilityResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/v1/system/availability"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out v1.AvailabilityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// A DEPLOYMENT THAT HAS RECORDED NOTHING SAYS SO. Reporting the window as an
// outage would have the product raising a false alarm about itself the first
// time anybody opened the page after an upgrade; reporting it as healthy would
// be a reassurance nothing supports.
func TestAvailabilityWithNoRecordIsUnknown(t *testing.T) {
	h, _ := availabilityServer(t)

	got := getAvailability(t, h, "")

	if got.Status != v1.AvailabilityUnknown {
		t.Fatalf("status %q, want UNKNOWN", got.Status)
	}
	if got.RecordedFrom != "" {
		t.Fatalf("recordedFrom %q, want empty", got.RecordedFrom)
	}
	if got.DownSeconds != 0 {
		t.Fatalf("downSeconds %d, want 0 - nothing known is not an outage", got.DownSeconds)
	}
	// Never null. An empty list is "no outages"; a null leaves each client to
	// decide what it means, and they will not agree.
	if got.Outages == nil {
		t.Error("outages must be an empty list rather than null")
	}
}

// A SERVICE THAT IS SERVING REPORTS NO OUTAGE, which is the one answer this
// panel gets wrong most expensively: the interface it replaces inferred an
// outage from a single failing endpoint and put it over the whole application.
func TestAvailabilityReportsAServingCoordinator(t *testing.T) {
	h, av := availabilityServer(t)

	if _, err := av.Beat(t.Context(), "coordinator", "one", "1.2.3",
		store.AvailabilityHealthy, store.AvailabilityContinuity); err != nil {
		t.Fatal(err)
	}

	got := getAvailability(t, h, "?window=24h")

	if got.Status != v1.AvailabilityHealthy {
		t.Fatalf("status %q, want HEALTHY", got.Status)
	}
	if len(got.Outages) != 0 {
		t.Fatalf("outages %+v, want none", got.Outages)
	}
	if got.Starts != 1 {
		t.Fatalf("starts %d, want 1", got.Starts)
	}
	if got.Uptime < 0.99 {
		t.Fatalf("uptime %.4f, want ~1", got.Uptime)
	}
	if got.RecordedFrom == "" {
		t.Error("recordedFrom must say when the record begins")
	}
	// The resolution is part of the answer: without it a reader concludes a
	// thirty-second interruption did not happen because it is not listed.
	if got.BeatSeconds <= 0 {
		t.Errorf("beatSeconds %d, want the recording interval", got.BeatSeconds)
	}
	// Every second of the window is accounted for, so no client has to derive
	// one of these by subtraction and disagree with this one.
	if sum := got.UpSeconds + got.DegradedSeconds + got.DownSeconds; sum != got.WindowSeconds {
		t.Errorf("up+degraded+down = %d, window = %d", sum, got.WindowSeconds)
	}
	// Clipped to the record: a day was asked for and the record is seconds old.
	if got.WindowSeconds > 60 {
		t.Errorf("window %ds, want the few seconds this record covers", got.WindowSeconds)
	}
}

// A window this service will not summarise is refused by name rather than
// quietly answered with a different one.
func TestAvailabilityRefusesAWindowItDoesNotKeep(t *testing.T) {
	h, _ := availabilityServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/v1/system/availability?window=1y", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	var problem v1.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != v1.CodeInvalidArgument {
		t.Fatalf("code %q, want INVALID_ARGUMENT", problem.Code)
	}
}

// A Coordinator that records nothing does not carry the route. A panel reading
// "no outages" off a table nothing writes to would be the most reassuring lie
// in the product.
func TestAvailabilityRouteIsAbsentWithoutTheRecord(t *testing.T) {
	h := NewServer(Deps{
		Logger:    slog.New(slog.DiscardHandler),
		Products:  product.NewRegistry(),
		Component: "coordinator",
	}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/v1/system/availability", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

// The gap between two runs is served as an outage with its real length.
func TestAvailabilityServesARecordedOutage(t *testing.T) {
	h, av := availabilityServer(t)
	ctx := t.Context()

	// One run, then a beat too late to continue it: the space between them is
	// time no replica recorded, which is what an outage is here.
	if _, err := av.Beat(ctx, "coordinator", "one", "1.2.3",
		store.AvailabilityHealthy, store.AvailabilityContinuity); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := av.Beat(ctx, "coordinator", "one", "1.2.3",
		store.AvailabilityHealthy, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	got := getAvailability(t, h, "?window=1h")

	if len(got.Outages) != 1 {
		t.Fatalf("outages %+v, want one", got.Outages)
	}
	if got.Outages[0].Ongoing {
		t.Error("an outage the service came back from is not ongoing")
	}
	if got.Starts != 2 {
		t.Fatalf("starts %d, want 2", got.Starts)
	}
	if got.Status != v1.AvailabilityHealthy {
		t.Fatalf("status %q, want HEALTHY - it came back", got.Status)
	}
}
