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
// The SPA shows "This account is not enabled" when `permissions` comes back
// empty (web/src/App.tsx). permissionsFor asked whether the caller could act
// TENANT-WIDE, which a product-scoped caller correctly cannot - so somebody
// granted product-owner on one product, which is the only grant they need and
// the whole point of the product tier, signed in and was refused by a full
// screen carrying their own address. Granting any `org-` role appeared to fix
// it, by making them tenant-wide over everything.
func TestWhoAmIDoesNotLockOutAProductOnlyAccount(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Authenticator = fixedAuthenticator{id: productOwnerOfOne()}
	})

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")

	if !out.Authenticated {
		t.Fatal("authenticated is false for a caller the authenticator accepted")
	}
	// The condition the interface actually reads. Empty is the locked door.
	if len(out.Permissions) == 0 {
		t.Error("permissions is empty for an account holding product-owner, which the " +
			"interface renders as 'This account is not enabled'")
	}
	// And not the other failure: `*` means unrestricted, and the client
	// short-circuits on it BEFORE narrowing by product, so a product-scoped
	// caller reporting it would be handed the estate.
	if slices.Contains(out.Permissions, "*") {
		t.Errorf("permissions = %v: a product-scoped caller reports itself unrestricted",
			out.Permissions)
	}
	for _, want := range []string{"read", "operate", "apply", "admin"} {
		if !slices.Contains(out.Permissions, want) {
			t.Errorf("permissions = %v, missing %q which a product-owner holds on their product",
				out.Permissions, want)
		}
	}
	// Where those verbs apply. This is the half that keeps the answer safe.
	if !slices.Equal(out.Products, []string{"software-01"}) {
		t.Errorf("products = %v, want [software-01]", out.Products)
	}
}

// An account holding nothing is still refused, which is the other half of the
// same decision: the fix above must not turn the door into a formality.
func TestWhoAmIStillReportsNothingForAnAccountWithNoRoles(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Authenticator = fixedAuthenticator{id: middleware.Identity{
			Subject: "u2", Tenant: "default", Method: "oidc",
		}}
	})

	out := getJSON[v1.WhoAmIResponse](t, h.server.URL+"/api/v1/whoami")
	if len(out.Permissions) != 0 {
		t.Errorf("permissions = %v for an account holding no roles, want none", out.Permissions)
	}
}
