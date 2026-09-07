package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type alwaysFails struct{}

func (alwaysFails) Authenticate(*http.Request) (Identity, error) {
	return Identity{}, http.ErrNoCookie
}

// A probe that needs a token makes every replica restart-loop while the
// service is healthy. This test is the guard against that regression.
func TestProbesAnswerWithoutCredentials(t *testing.T) {
	reached := false
	h := Auth(alwaysFails{}, func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusUnauthorized)
	}, PublicPaths)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, p := range []string{"/healthz", "/readyz", "/livez", "/metrics"} {
		reached = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK || !reached {
			t.Errorf("%s: status %d, handler reached %v; want 200 and reached", p, rec.Code, reached)
		}
	}

	// Everything else still requires credentials.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/products", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/products: status %d, want 401", rec.Code)
	}
}
