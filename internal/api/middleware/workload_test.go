package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerPlaneIsTheWorkerRoutesAndNothingElse(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/v1/jobs:lease", true},
		{http.MethodPost, "/api/v1/jobs/job-1:complete", true},
		{http.MethodPost, "/api/v1/jobs/job-1:fail", true},
		{http.MethodPost, "/api/v1/workers/worker-1:heartbeat", true},

		// The fleet listing is a person looking at a screen. Same path shape
		// as the heartbeat, opposite side of the fence, and the method is the
		// only thing that tells them apart.
		{http.MethodGet, "/api/v1/workers", false},
		{http.MethodGet, "/api/v1/workers/worker-1", false},
		{http.MethodGet, "/api/v1/jobs:lease", false},

		{http.MethodPost, "/api/v1/transfers", false},
		{http.MethodGet, "/api/v1/auditEvents", false},
		{http.MethodGet, "/api/v1/products", false},
		{http.MethodGet, "/healthz", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := WorkerPlane(r); got != c.want {
			t.Errorf("WorkerPlane(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestWorkloadOnly(t *testing.T) {
	worker := Identity{Grants: []Grant{{Action: ActionWork, Scope: Scope{Tenant: "default"}}}}
	if !worker.WorkloadOnly() {
		t.Error("an identity granted only the work action is not recognised as a workload")
	}

	admin := Identity{Grants: []Grant{
		{Action: ActionRead}, {Action: ActionOperate}, {Action: ActionAdmin}, {Action: ActionWork},
	}}
	if admin.WorkloadOnly() {
		t.Error("an administrator was mistaken for a workload")
	}

	// A person whose roles have not been set up yet. Refusing them as "a
	// machine that may only lease jobs" would be wrong and unactionable.
	if (Identity{}).WorkloadOnly() {
		t.Error("an identity with no grants was treated as a workload")
	}
}

// The fence, from both sides, through the middleware that enforces it.
func TestConfine(t *testing.T) {
	worker := Identity{
		Tenant: "default",
		Roles:  []Role{"org-worker"},
		Grants: []Grant{{Action: ActionWork, Scope: Scope{Tenant: "default"}}},
	}
	reader := Identity{
		Tenant: "default",
		Roles:  []Role{"org-reader"},
		Grants: []Grant{{Action: ActionRead, Scope: Scope{Tenant: "default"}}},
	}

	cases := []struct {
		name         string
		id           Identity
		method, path string
		wantAllowed  bool
	}{
		{"worker leases", worker, http.MethodPost, "/api/v1/jobs:lease", true},
		{"worker heartbeats", worker, http.MethodPost, "/api/v1/workers/w1:heartbeat", true},
		{"worker reads products", worker, http.MethodGet, "/api/v1/products", false},
		{"worker reads the audit trail", worker, http.MethodGet, "/api/v1/auditEvents", false},
		{"worker requests a transfer", worker, http.MethodPost, "/api/v1/transfers", false},

		{"reader reads", reader, http.MethodGet, "/api/v1/products", true},
		{"reader leases", reader, http.MethodPost, "/api/v1/jobs:lease", false},

		// With authentication off every caller is Anonymous and holds admin.
		// A deployment that has not switched it on must behave exactly as it
		// did before this middleware existed.
		{"anonymous leases", Anonymous, http.MethodPost, "/api/v1/jobs:lease", true},
		{"anonymous reads", Anonymous, http.MethodGet, "/api/v1/products", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached := false
			var denied string
			h := Confine(func(w http.ResponseWriter, _ *http.Request, detail string) {
				denied = detail
				w.WriteHeader(http.StatusForbidden)
			})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

			r := httptest.NewRequest(c.method, c.path, nil)
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, c.id))
			h.ServeHTTP(httptest.NewRecorder(), r)

			if reached != c.wantAllowed {
				t.Fatalf("reached handler = %v, want %v (refusal: %q)", reached, c.wantAllowed, denied)
			}
			// A refusal a reader cannot act on is a refusal reported twice.
			if !c.wantAllowed && denied == "" {
				t.Error("refused without saying why")
			}
		})
	}
}
