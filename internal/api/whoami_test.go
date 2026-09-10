package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// fixedAuthenticator answers with one identity, whoever calls.
//
// A named type rather than a closure so that what it stands in for - a caller
// arriving with a verified token - is legible at the call site.
type fixedAuthenticator struct{ id middleware.Identity }

func (f fixedAuthenticator) Authenticate(*http.Request) (middleware.Identity, error) {
	return f.id, nil
}

// productOwnerOfOne is what a token carrying a single product role produces.
//
// The grants are written out rather than derived, because deriving them here
// would mean testing this package's assertion against this package's own
// construction. That role names map onto these four actions is pinned next
// door, by TestAProductOnlyAccountHoldsSomething in internal/api/middleware.
func productOwnerOfOne() middleware.Identity {
	id := middleware.Identity{
		Subject:      "u1",
		Email:        "test@domain2.com",
		Tenant:       "default",
		Method:       "oidc",
		ProductRoles: map[string][]string{"software-01": {"product-owner"}},
	}
	for _, a := range []middleware.Action{
		middleware.ActionRead, middleware.ActionOperate,
		middleware.ActionApply, middleware.ActionAdmin,
	} {
		id.Grants = append(id.Grants, middleware.Grant{
			Action: a,
			Scope:  middleware.Scope{Tenant: "default", Product: "software-01"},
		})
	}
	return id
}

// TestWhoAmIDoesNotLockOutAProductOnlyAccount is a locked door, stated as a
// test.
//
// Somebody granted product-owner on one product is correctly provisioned: that
// is the only grant they need, and the product tier exists so it can be the
// only one. They were shown a full-screen "This account is not enabled"
// carrying their own address, because the interface reads an empty permission
// list as "not enabled" and permissionsFor answers the TENANT-WIDE question,
// which a product-scoped caller correctly cannot.
//
// Empty tenant-wide permissions is the RIGHT answer for them. What was wrong
// was using it to decide whether they exist. Membership answers that now, and
// the verbs they hold on their own product are reported where a client can
// pair them with the product they apply to.
func TestWhoAmIDoesNotLockOutAProductOnlyAccount(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Authenticator = fixedAuthenticator{id: productOwnerOfOne()}
	})

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")

	if !out.Authenticated {
		t.Fatal("authenticated is false for a caller the authenticator accepted")
	}
	// The condition the interface reads to decide whether to show the door.
	if !out.Member {
		t.Error("member is false for an account holding product-owner, so the interface " +
			"shows 'This account is not enabled' to somebody who is provisioned")
	}
	// Tenant-wide: nothing, and that is correct. They hold one product.
	if len(out.Permissions) != 0 {
		t.Errorf("permissions = %v, want none: a product-scoped caller holds nothing "+
			"tenant-wide", out.Permissions)
	}
	// Where their verbs actually apply.
	for _, want := range []string{"read", "operate", "apply", "admin"} {
		if !slices.Contains(out.ProductPermissions["software-01"], want) {
			t.Errorf("productPermissions[software-01] = %v, missing %q which a "+
				"product-owner holds", out.ProductPermissions["software-01"], want)
		}
	}
	if _, wrong := out.ProductPermissions["software-02"]; wrong {
		t.Error("verbs are reported for a product this caller holds nothing on")
	}
	if !slices.Equal(out.Products, []string{"software-01"}) {
		t.Errorf("products = %v, want [software-01]", out.Products)
	}
}

// TestWhoAmIDoesNotFlattenVerbsAcrossProducts is the failure the shape above
// prevents, and the reason `permissions` is not a union.
//
// A caller who READS product A and OWNS product B holds four verbs and two
// products. Reported as one flat list they read as four verbs on both, and an
// interface pairing them offers "approve download" on A - which the server
// then refuses, because Cerbos checks the product against the caller's own.
// An interface that offers what the API denies is not a cosmetic defect: it is
// a permission model the screen and the server disagree about.
func TestWhoAmIDoesNotFlattenVerbsAcrossProducts(t *testing.T) {
	id := middleware.Identity{
		Subject: "u3", Tenant: "default", Method: "oidc",
		ProductRoles: map[string][]string{
			"software-01": {"product-reader"},
			"software-02": {"product-owner"},
		},
		Grants: []middleware.Grant{
			{Action: middleware.ActionRead, Scope: middleware.Scope{Tenant: "default", Product: "software-01"}},
			{Action: middleware.ActionRead, Scope: middleware.Scope{Tenant: "default", Product: "software-02"}},
			{Action: middleware.ActionOperate, Scope: middleware.Scope{Tenant: "default", Product: "software-02"}},
			{Action: middleware.ActionApply, Scope: middleware.Scope{Tenant: "default", Product: "software-02"}},
			{Action: middleware.ActionAdmin, Scope: middleware.Scope{Tenant: "default", Product: "software-02"}},
		},
	}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Authenticator = fixedAuthenticator{id: id} })

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")

	if got := out.ProductPermissions["software-01"]; !slices.Equal(got, []string{"read"}) {
		t.Errorf("productPermissions[software-01] = %v, want [read]: this caller only "+
			"reads that product", got)
	}
	if got := out.ProductPermissions["software-02"]; !slices.Contains(got, "apply") {
		t.Errorf("productPermissions[software-02] = %v, missing apply", got)
	}
	if len(out.Permissions) != 0 {
		t.Errorf("permissions = %v, want none tenant-wide", out.Permissions)
	}
}

// TestWhoAmITellsAMemberFromAStranger is the distinction the baseline role
// exists to make.
//
// An account the identity provider let in and nobody provisioned holds
// nothing. A colleague who has been provisioned and not yet given a product
// holds the baseline role and nothing else. Both may do nothing at all; only
// the first is a stranger, and only the first should meet a closed door.
func TestWhoAmITellsAMemberFromAStranger(t *testing.T) {
	member := middleware.Identity{
		Subject: "u4", Tenant: "default", Method: "oidc",
		Roles: []middleware.Role{"org-member"},
	}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Authenticator = fixedAuthenticator{id: member} })
	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")
	if !out.Member {
		t.Error("a provisioned account holding only the baseline role is not reported as a member")
	}
	if len(out.Permissions) != 0 || len(out.ProductPermissions) != 0 {
		t.Errorf("the baseline role carries permissions: %v / %v",
			out.Permissions, out.ProductPermissions)
	}

	stranger := middleware.Identity{Subject: "u5", Tenant: "default", Method: "oidc"}
	h2 := newAPIHarnessWith(t, func(d *Deps) { d.Authenticator = fixedAuthenticator{id: stranger} })
	out2 := getJSON[v1.WhoAmIResponse](t, h2.server.URL+"/api/v1/whoami")
	if out2.Member {
		t.Error("an account nobody provisioned is reported as a member of this tenant")
	}
	if len(out2.Permissions) != 0 {
		t.Errorf("permissions = %v for an account holding no roles", out2.Permissions)
	}
}

// An org-wide role still reports `*`, because the client short-circuits on it
// before narrowing by product and an org-admin is exactly what it means.
func TestWhoAmIReportsUnrestrictedForAnOrgAdmin(t *testing.T) {
	id := middleware.Identity{
		Subject: "u6", Tenant: "default", Method: "oidc",
		Roles: []middleware.Role{"org-admin"},
	}
	for _, a := range []middleware.Action{
		middleware.ActionRead, middleware.ActionOperate,
		middleware.ActionApply, middleware.ActionAdmin,
	} {
		id.Grants = append(id.Grants, middleware.Grant{Action: a, Scope: middleware.Scope{Tenant: "default"}})
	}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Authenticator = fixedAuthenticator{id: id} })

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")
	if !slices.Equal(out.Permissions, []string{"*"}) {
		t.Errorf("permissions = %v, want [*] for an org-admin", out.Permissions)
	}
	if len(out.Products) != 0 {
		t.Errorf("products = %v, want empty meaning unrestricted", out.Products)
	}
}

// THE INTERFACE'S OWN ANSWER, and the defect it was reported for.
//
// A product owner was shown a disabled Discover button on their own product.
// The interface was reading `permissions`, which is the tenant-wide question
// and correctly answers nothing for them, and there was no other answer to
// read: four coarse verbs cannot say "may run discovery on software-01".
//
// `access` is that answer, in the vocabulary of config/access/policies, so the
// browser can gate one control on one permission over one product.
func TestWhoAmIReportsWhatEachProductActuallyAllows(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Authenticator = fixedAuthenticator{id: productOwnerOfOne()}
	})

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")

	// Nothing tenant-wide. They hold one product, and the estate is not theirs.
	if len(out.Access.Global) != 0 {
		t.Errorf("access.global = %v, want none for a product-scoped caller", out.Access.Global)
	}
	held := out.Access.ByProduct["software-01"]
	for _, want := range []string{
		"product.discover", "product.view", "package.view",
		"software_download.request", "audit_event.view",
	} {
		if !slices.Contains(held, want) {
			t.Errorf("access.byProduct[software-01] = %v, missing %q which a product-owner "+
				"holds - the control it gates is hidden from somebody entitled to it", held, want)
		}
	}
	// The ESTATE permissions have no product tier, so they must not turn up
	// under one. An interface reading them there would offer this person the
	// fleet, the rollups and the deployment's own settings.
	for _, never := range []string{"report.view", "worker.view", "policy_catalogue.view", "system.view"} {
		if slices.Contains(held, never) {
			t.Errorf("access.byProduct[software-01] carries the estate permission %q", never)
		}
	}
	if _, wrong := out.Access.ByProduct["software-02"]; wrong {
		t.Error("permissions are reported on a product this caller holds nothing on")
	}
	if out.Access.Unavailable {
		t.Error("access reports itself unresolvable on a deployment with no policy engine")
	}
}

// An org-tier caller holds their permissions TENANT-WIDE, which covers products
// that do not exist yet - so nothing is copied under today's product names.
//
// Copying them in is the mistake this shape exists to prevent: it turns "covers
// everything" into "covers these", and the day a product is added the interface
// silently stops offering it.
func TestWhoAmIDoesNotNarrowATenantWideCallerToTodaysProducts(t *testing.T) {
	admin := middleware.Identity{
		Subject: "u9", Tenant: "default", Method: "oidc",
		Roles: []middleware.Role{"org-admin"},
	}
	for _, a := range []middleware.Action{
		middleware.ActionRead, middleware.ActionOperate,
		middleware.ActionApply, middleware.ActionAdmin,
	} {
		admin.Grants = append(admin.Grants, middleware.Grant{
			Action: a, Scope: middleware.Scope{Tenant: "default"},
		})
	}
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Authenticator = fixedAuthenticator{id: admin}
	})

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")

	for _, want := range []string{"system.view", "report.view", "product.discover"} {
		if !slices.Contains(out.Access.Global, want) {
			t.Errorf("access.global = %v, missing %q", out.Access.Global, want)
		}
	}
	if len(out.Access.ByProduct) != 0 {
		t.Errorf("a tenant-wide grant was copied into %v", out.Access.ByProduct)
	}
}
