package authz

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// TestRoleClaimedTwiceIsHeldOnce guards the shape of a real ZITADEL token.
//
// The same grant arrives under both the per-project claim and the flattened
// one, and both match the prefix the reader looks for. Counted twice it
// reached Cerbos twice and the Settings page rendered "org-admin, org-admin",
// which reads as two grants rather than one.
func TestRoleClaimedTwiceIsHeldOnce(t *testing.T) {
	all := map[string]json.RawMessage{
		"urn:zitadel:iam:org:project:roles": json.RawMessage(
			`{"org-admin":{"1":"default.localhost"},"software-01:product-owner":{"1":"default.localhost"}}`),
		"urn:zitadel:iam:org:project:389770422465331204:roles": json.RawMessage(
			`{"org-admin":{"1":"default.localhost"}}`),
		"urn:zitadel:iam:org:project:389770422465331205:roles": json.RawMessage(
			`{"software-01:product-owner":{"1":"default.localhost"}}`),
		"email": json.RawMessage(`"someone@example.com"`),
	}
	id := Identity{Products: map[string][]string{}}
	readRoles(all, &id)

	if want := []string{"org-admin"}; !slices.Equal(id.OrgRoles, want) {
		t.Fatalf("org roles: got %v, want %v", id.OrgRoles, want)
	}
	if want := []string{"product-owner"}; !slices.Equal(id.Products["software-01"], want) {
		t.Fatalf("product roles: got %v, want %v", id.Products["software-01"], want)
	}
	// The org name comes off the role's own value, so a token carrying only
	// role claims still names its tenant.
	if id.Tenant != "default" {
		t.Fatalf("tenant: got %q, want %q", id.Tenant, "default")
	}
}

// TestTokenFromAnotherTenantIsRefused is the boundary, stated as a test.
//
// An issuer that hosts more than one organization signs all of their tokens
// with the same keys, so signature, issuer and expiry - everything else this
// package checks - are satisfied by a token from a tenant this deployment has
// nothing to do with. The only thing left between it and the data was whether
// its bearer happened to hold a role of the same NAME, and `org-admin` granted
// in somebody else's organization is spelled exactly like `org-admin` here.
func TestTokenFromAnotherTenantIsRefused(t *testing.T) {
	if err := wrongTenant("acme", "default"); err == nil {
		t.Error("a token from tenant 'acme' was accepted by a deployment serving 'default'")
	}
	if err := wrongTenant("default", "default"); err != nil {
		t.Errorf("a token from this deployment's own tenant was refused: %v", err)
	}
	// ZITADEL org names are matched case-insensitively, as ZITADEL does.
	if err := wrongTenant("Default", "default"); err != nil {
		t.Errorf("tenant matching is case-sensitive: %v", err)
	}

	// A token that asserts NO tenant is the shape a caller has when the
	// sign-in did not ask for the claim that carries one. "Cannot tell" is not
	// "belongs here": a boundary that waves through what it cannot read is not
	// a boundary.
	err := wrongTenant("", "default")
	if err == nil {
		t.Fatal("a token asserting no tenant was accepted")
	}
	if !strings.Contains(err.Error(), "resourceowner") {
		t.Errorf("the refusal does not name the scope that fixes it: %v", err)
	}

	// Unset: single-tenant deployments and every stack that predates this keep
	// working, and the Coordinator warns about it at startup instead.
	if err := wrongTenant("anything", ""); err != nil {
		t.Errorf("an unconfigured deployment refused a token: %v", err)
	}
}

// TestResourceCarriesTheDeploymentsTenant guards a tenancy condition that was
// written eleven times and enforced nowhere.
//
// Every derived role in config/access/policies compares
// `R.attr.tenant == P.attr.tenant`. The resource was labelled with the
// PRINCIPAL's tenant, so that comparison compared a value with itself: always
// true, for every caller, in every rule.
func TestResourceCarriesTheDeploymentsTenant(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"results":[{"actions":{"view":"EFFECT_DENY"}}]}`))
	}))
	defer srv.Close()

	c := &Cerbos{Addr: srv.URL, Tenant: "default", HTTP: srv.Client()}
	_, err := c.Check(context.Background(),
		Identity{Subject: "u", Tenant: "acme", OrgRoles: []string{"org-admin"}},
		Resource{Kind: "product", ID: "*"}, "view")
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	var sent struct {
		Resources []struct {
			Resource struct {
				Attr map[string]any `json:"attr"`
			} `json:"resource"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if got := sent.Resources[0].Resource.Attr["tenant"]; got != "default" {
		t.Errorf("resource tenant = %v, want the DEPLOYMENT's 'default' rather than the "+
			"caller's 'acme' - a policy comparing the two must be able to disagree", got)
	}
}
