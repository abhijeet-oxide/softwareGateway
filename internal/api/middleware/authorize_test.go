package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
)

// The identities are built through fromAuthz, from role names, rather than by
// hand-writing grants. A test that constructs the grants it then asserts on
// proves the assertion and not the mapping, and the mapping - role name to
// actions - is the half somebody will edit.
func identity(tenant string, orgRoles []string, products map[string][]string) Identity {
	return fromAuthz(authz.Identity{
		Subject: "u1", Tenant: tenant, OrgRoles: orgRoles, Products: products,
	})
}

// TestNobodyWithoutRolesReachesAnything is the reported hole, stated as a test.
//
// A person signed in through the identity provider, provisioned nothing, and
// could read the whole estate: every product, every transfer, and the audit
// trail. The token was valid, so authentication passed; nothing then asked
// whether they were allowed.
func TestNobodyWithoutRolesReachesAnything(t *testing.T) {
	nobody := identity("default", nil, nil)
	if len(nobody.Grants) != 0 {
		t.Fatalf("an account with no roles was given grants: %v", nobody.Grants)
	}

	for _, path := range []string{
		"/api/v1/products",
		"/api/v1/products/software-01/packages",
		"/api/v1/auditEvents",
		"/api/v1/transfers",
		"/api/v1/workers",
		"/api/v1/reports/summary",
	} {
		if allowed, _ := attempt(nobody, http.MethodGet, path); allowed {
			t.Errorf("GET %s was served to an account holding no roles", path)
		}
	}
	if allowed, _ := attempt(nobody, http.MethodPost, "/api/v1/transfers"); allowed {
		t.Error("an account holding no roles could request a transfer")
	}

	// And it is told something it can act on, rather than a bare refusal.
	_, detail := attempt(nobody, http.MethodGet, "/api/v1/products")
	if !strings.Contains(detail, "no roles") || !strings.Contains(detail, "administrator") {
		t.Errorf("the refusal does not tell them what to do: %q", detail)
	}
}

// The two routes that must answer whatever the caller holds, because they are
// how the application explains the refusal above. Gate them and a person with
// no roles gets a service-unavailable screen instead of a page naming the
// problem.
func TestTheExplainingRoutesAreAlwaysReachable(t *testing.T) {
	nobody := identity("default", nil, nil)
	for _, path := range []string{"/api/v1/whoami", "/api/v1/system/version"} {
		if allowed, detail := attempt(nobody, http.MethodGet, path); !allowed {
			t.Errorf("GET %s refused an account with no roles: %q", path, detail)
		}
	}
}

func TestAuthorizationMatrix(t *testing.T) {
	var (
		admin    = identity("default", []string{"org-admin"}, nil)
		operator = identity("default", []string{"org-operator"}, nil)
		reader   = identity("default", []string{"org-reader"}, nil)
		security = identity("default", []string{"org-security"}, nil)
		owner    = identity("default", nil, map[string][]string{"software-01": {"product-owner"}})
	)

	cases := []struct {
		name         string
		id           Identity
		method, path string
		want         bool
	}{
		// An administrator reaches everything.
		{"admin reads products", admin, http.MethodGet, "/api/v1/products", true},
		{"admin reads the audit trail", admin, http.MethodGet, "/api/v1/auditEvents", true},
		{"admin requests a transfer", admin, http.MethodPost, "/api/v1/transfers", true},
		{"admin applies replication", admin, http.MethodPost,
			"/api/v1/products/software-01/targets/t1/replication:apply", true},

		// An operator may request work and may not push configuration into
		// somebody else's registry - that is what separates apply from operate.
		{"operator requests a transfer", operator, http.MethodPost, "/api/v1/transfers", true},
		{"operator discovers", operator, http.MethodPost, "/api/v1/products/software-01/packages:discover", true},
		{"operator applies replication", operator, http.MethodPost,
			"/api/v1/products/software-01/targets/t1/replication:apply", false},

		// A reader reads, and changes nothing.
		{"reader reads packages", reader, http.MethodGet, "/api/v1/products/software-01/packages", true},
		{"reader requests a transfer", reader, http.MethodPost, "/api/v1/transfers", false},
		{"reader runs discovery", reader, http.MethodPost, "/api/v1/products/software-01/packages:discover", false},
		{"reader retries transfers", reader, http.MethodPost, "/api/v1/transfers:retry", false},

		// The audit trail is a READ, and org-security is the role that exists
		// for exactly that. It is the most sensitive read in the system and it
		// is still a read.
		{"security reads the audit trail", security, http.MethodGet, "/api/v1/auditEvents", true},
		{"security changes nothing", security, http.MethodPost, "/api/v1/transfers", false},

		// A product-scoped owner: everything on their product, nothing on
		// another, and the self-filtering listings are reachable so they can
		// see the one product they hold.
		{"owner reads their product", owner, http.MethodGet, "/api/v1/products/software-01/packages", true},
		{"owner operates their product", owner, http.MethodPost,
			"/api/v1/products/software-01/packages:discover", true},
		{"owner reads another product", owner, http.MethodGet, "/api/v1/products/software-02/packages", false},
		{"owner operates another product", owner, http.MethodPost,
			"/api/v1/products/software-02/packages:discover", false},
		{"owner lists products", owner, http.MethodGet, "/api/v1/products", true},
		{"owner reads the audit trail", owner, http.MethodGet, "/api/v1/auditEvents", true},

		// Not filtered by product, so it stays tenant-wide: a listing that
		// cannot narrow its answer must not widen its door.
		{"owner lists every transfer", owner, http.MethodGet, "/api/v1/transfers", false},

		// With authentication off every caller is Anonymous and holds admin, so
		// a deployment that has not switched it on behaves exactly as before.
		{"anonymous reads", Anonymous, http.MethodGet, "/api/v1/products", true},
		{"anonymous writes", Anonymous, http.MethodPost, "/api/v1/transfers", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, detail := attempt(c.id, c.method, c.path)
			if allowed != c.want {
				t.Fatalf("%s %s allowed=%v want=%v (%s)", c.method, c.path, allowed, c.want, detail)
			}
		})
	}
}

// A route this file has never heard of is governed anyway, which is the
// property that stops this gap coming back the next time somebody adds one.
func TestAnUnknownRouteStillNeedsAPermission(t *testing.T) {
	nobody := identity("default", nil, nil)
	reader := identity("default", []string{"org-reader"}, nil)

	if allowed, _ := attempt(nobody, http.MethodGet, "/api/v1/somethingInventedLater"); allowed {
		t.Error("an unknown route was open to an account with no roles")
	}
	if allowed, _ := attempt(reader, http.MethodPost, "/api/v1/somethingInventedLater"); allowed {
		t.Error("an unknown write was open to a reader")
	}
	if allowed, _ := attempt(reader, http.MethodGet, "/api/v1/somethingInventedLater"); !allowed {
		t.Error("an unknown read was refused to a reader, which would break every new route")
	}
}

func TestProductIn(t *testing.T) {
	cases := map[string]string{
		"/api/v1/products":                               "",
		"/api/v1/products:discover":                      "",
		"/api/v1/products/software-01":                   "software-01",
		"/api/v1/products/software-01/packages":          "software-01",
		"/api/v1/products/software-01:calibrate":         "software-01",
		"/api/v1/products/software-01:checkConnectivity": "software-01",
		"/api/v1/products/a%2Fb/packages":                "a/b",
		"/api/v1/transfers":                              "",
		"/api/v1/auditEvents":                            "",
		"/healthz":                                       "",
	}
	for path, want := range cases {
		if got := productIn(path); got != want {
			t.Errorf("productIn(%q) = %q, want %q", path, got, want)
		}
	}
}

// attempt runs one request through the gate and reports whether it reached the
// handler, and what it was told if it did not.
func attempt(id Identity, method, path string) (bool, string) {
	reached := false
	var denied string
	h := Authorize(nil, func(w http.ResponseWriter, _ *http.Request, detail string) {
		denied = detail
		w.WriteHeader(http.StatusForbidden)
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	r := httptest.NewRequest(method, path, nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, id))
	h.ServeHTTP(httptest.NewRecorder(), r)
	return reached, denied
}

// TestAProductOnlyAccountHoldsSomething states the situation that produced a
// locked door, so the next person reading these tests can see it.
//
// Somebody granted `product-owner` on one product and nothing else is
// correctly provisioned: that is the only grant they need, and the two-tier
// model exists so it can be the only one. They hold every action ON THAT
// PRODUCT and none of them tenant-wide, and both halves matter - the first is
// what the interface must enable, the second is what stops it enabling the
// estate.
func TestAProductOnlyAccountHoldsSomething(t *testing.T) {
	only := identity("default", nil, map[string][]string{"software-01": {"product-owner"}})

	for _, a := range []Action{ActionRead, ActionOperate, ActionApply, ActionAdmin} {
		if !only.Can(a, Scope{Tenant: "default", Product: "software-01"}) {
			t.Errorf("a product-owner may not %s their own product", a)
		}
		if !only.CanAny(a) {
			t.Errorf("CanAny(%s) is false for a product-owner who may %s their product", a, a)
		}
		if only.Can(a, Scope{Tenant: "default"}) {
			t.Errorf("a product-owner answers the tenant-wide question for %s", a)
		}
	}
	if got := only.VisibleProducts(); len(got) != 1 || got[0] != "software-01" {
		t.Errorf("VisibleProducts = %v, want [software-01]", got)
	}
}

// TestAnOrgRoleIsNotNarrowedByAProductRole guards the tier boundary in the one
// direction that fails quietly.
//
// An `org-` role names no product on purpose: it covers products that do not
// exist yet. Holding one AND a product role must not shrink what the org role
// covers - but VisibleProducts asked the estate-wide question, which a
// tenant-scoped grant deliberately cannot answer, so it fell through to the
// product list and returned that one product. Every scoped store filter takes
// this list, so an org-reader who also owned one product would have been shown
// only that product's data.
//
// It could not be seen until product roles reached a token at all, which is
// why it is being written now rather than then.
func TestAnOrgRoleIsNotNarrowedByAProductRole(t *testing.T) {
	both := identity("default", []string{"org-reader"},
		map[string][]string{"software-01": {"product-owner"}})
	if got := both.VisibleProducts(); got != nil {
		t.Errorf("VisibleProducts = %v for an org-reader, want nil meaning unrestricted", got)
	}

	orgOnly := identity("default", []string{"org-reader"}, nil)
	if got := orgOnly.VisibleProducts(); got != nil {
		t.Errorf("VisibleProducts = %v for a plain org-reader, want nil", got)
	}
}
