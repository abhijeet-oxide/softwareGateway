package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func probeServer(reg *health.Registry) http.Handler {
	return NewServer(Deps{
		Products:  product.NewRegistry(),
		Component: "coordinator",
		Health:    reg,
	}).Handler()
}

// TestLivenessAnswersUnderBothNames guards a probe that was documented,
// exempted from authentication, covered by a test and never registered.
//
// /livez answered 404, so a deployment following Kubernetes' own component
// convention configured a liveness probe against a path that did not exist -
// and a 404 is a failing probe, which restarts a perfectly healthy process.
func TestLivenessAnswersUnderBothNames(t *testing.T) {
	reg := health.New()
	reg.AddLiveness("process", func() error { return nil })
	h := probeServer(reg)

	for _, path := range []string{"/healthz", "/livez"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, rec.Code)
		}
	}
}

// TestDegradedIsStillReady is the rule that keeps one bad product file from
// taking every replica out of the Service.
//
// The Coordinator is built to stay up and serve the API when a product fails
// to load, precisely so it can tell somebody WHICH product is broken
// (docs/design/02 section 7). Readiness answered 503 for anything short of
// healthy, which meant the screen naming the broken file went away with the
// replicas that would have drawn it.
func TestDegradedIsStillReady(t *testing.T) {
	reg := health.New()
	reg.AddReadiness("configuration", func(context.Context) health.Result {
		return health.Degraded("1 product(s) failed to load")
	})
	rec := httptest.NewRecorder()
	probeServer(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var body v1.ReadyResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Ready, and honest about why it is not happy: a 200 that hid the
	// degradation would be no better than the 503 that hid the reason.
	if body.Status != v1.HealthStatus(health.StatusDegraded) {
		t.Fatalf("status %q, want %q", body.Status, health.StatusDegraded)
	}
	if len(body.Checks) != 1 || body.Checks[0].Name != "configuration" {
		t.Fatalf("checks %+v, want the configuration check named", body.Checks)
	}
}

// TestDownRefusesTraffic is the other half: a dependency without which nothing
// can be served at all still takes the replica out of the endpoints.
func TestDownRefusesTraffic(t *testing.T) {
	reg := health.New()
	reg.AddReadiness("database", func(context.Context) health.Result {
		return health.Down(errors.New("connection refused"))
	})
	rec := httptest.NewRecorder()
	probeServer(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	var body v1.ReadyResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The body names the failing dependency and its error, so the 503 is
	// actionable without going to a log first.
	if len(body.Checks) != 1 || body.Checks[0].Error == "" {
		t.Fatalf("checks %+v, want the failure named", body.Checks)
	}
}
