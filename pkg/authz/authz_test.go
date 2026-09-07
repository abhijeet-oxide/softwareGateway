package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitProductRoleSeparatesTheTwoTiers(t *testing.T) {
	cases := []struct {
		key, product, role string
		ok                 bool
	}{
		{"software-01:product-owner", "software-01", "product-owner", true},
		{"org-security", "", "", false},
		{"org-admin", "", "", false},
		{":leading", "", "", false},
		{"trailing:", "", "", false},
		{"a:b:c", "a", "b:c", true}, // product names cannot contain ':'
	}
	for _, c := range cases {
		p, r, ok := splitProductRole(c.key)
		if ok != c.ok || p != c.product || r != c.role {
			t.Errorf("%q -> (%q,%q,%v), want (%q,%q,%v)", c.key, p, r, ok, c.product, c.role, c.ok)
		}
	}
}

// An org-tier role must NOT appear in ProductNames. If it did, a policy that
// checks product membership would stop covering products created later, which
// is the exact failure the two-tier model exists to prevent.
func TestOrgRoleDoesNotBecomeAProductGrant(t *testing.T) {
	id := Identity{
		Tenant:   "default",
		OrgRoles: []string{"org-security"},
		Products: map[string][]string{"software-01": {"product-reader"}},
	}
	got := id.ProductNames()
	if len(got) != 1 || got[0] != "software-01" {
		t.Fatalf("ProductNames() = %v, want [software-01] only", got)
	}
	if !id.HasOrgRole("org-security") {
		t.Fatal("org-security should be held")
	}
}

func TestRolesOnIncludesOrgTier(t *testing.T) {
	id := Identity{
		OrgRoles: []string{"org-security"},
		Products: map[string][]string{"software-01": {"product-reader"}},
	}
	// An org role applies to a product the user has no explicit grant on.
	if got := id.RolesOn("software-99"); len(got) != 1 || got[0] != "org-security" {
		t.Fatalf("RolesOn(unknown product) = %v, want [org-security]", got)
	}
	if got := id.RolesOn("software-01"); len(got) != 2 {
		t.Fatalf("RolesOn(granted product) = %v, want both tiers", got)
	}
}

type denyAll struct{}

func (denyAll) Check(_ context.Context, _ Identity, _ Resource, actions ...string) (map[string]bool, error) {
	m := map[string]bool{}
	for _, a := range actions {
		m[a] = false
	}
	return m, nil
}

type boom struct{}

func (boom) Check(context.Context, Identity, Resource, ...string) (map[string]bool, error) {
	return nil, context.DeadlineExceeded
}

func TestRequireRefusesWhenEngineSaysNo(t *testing.T) {
	h := Require(denyAll{}, func(*http.Request) Resource { return Resource{Kind: "k"} }, "view")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// The engine being unreachable must never mean "allowed".
func TestRequireFailsClosedWhenEngineIsDown(t *testing.T) {
	h := Require(boom{}, func(*http.Request) Resource { return Resource{Kind: "k"} }, "view")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler must not run when the policy engine is unreachable")
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// A caller holding no roles is refused without troubling the engine.
func TestCerbosRefusesRolelessCallerWithoutACall(t *testing.T) {
	c := &Cerbos{Addr: "http://127.0.0.1:1", HTTP: &http.Client{}} // would fail if called
	got, err := c.Check(context.Background(), Identity{}, Resource{Kind: "k"}, "view")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["view"] {
		t.Fatal("a caller with no roles must not be allowed")
	}
}

func TestAuthenticateRefusesMissingToken(t *testing.T) {
	v := &Verifier{} // non-nil: authentication is ON
	h := Authenticate(Options{Verifier: v})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("must not run") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 must carry WWW-Authenticate")
	}
}

func TestAuthenticateDisabledYieldsAnonymous(t *testing.T) {
	var seen Identity
	h := Authenticate(Options{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if seen.Subject != "anonymous" || seen.Authenticated() {
		t.Fatalf("identity = %+v, want unauthenticated anonymous", seen)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc": "abc", "bearer abc": "abc", "BEARER  abc ": "abc",
		"Basic abc": "", "": "", "Bearer": "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
