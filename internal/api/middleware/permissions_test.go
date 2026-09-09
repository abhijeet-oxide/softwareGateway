package middleware

import (
	"context"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
)

// EVERY ROUTE'S QUESTION HAS A PERMISSION, and this is what keeps the two in
// step.
//
// A route added without a catalogue entry is not a broken route: it is
// governed, refused correctly, and INVISIBLE to the interface, which cannot
// name a permission it has never heard of. That failure is silent in a way a
// missing policy is not, so it is caught here rather than in a deployment.
func TestEveryRouteQuestionHasAPermission(t *testing.T) {
	for _, c := range routePolicies {
		if _, ok := PermissionFor(c.kind, c.action); !ok {
			t.Errorf("%s %s asks %s/%s, which no permission in the catalogue names",
				c.method, c.path, c.kind, c.action)
		}
	}
}

// And the reverse: a catalogue entry nobody enforces is a permission an
// interface can hide a control on and no request will ever require, which is
// how a control disappears for everybody.
//
// `system.write` is the exception and it is named here rather than left out of
// the catalogue: it is the FALLBACK for any write to a path the resource table
// has never heard of - see PolicyFor's default branch - so it is enforced on
// every route that does not exist yet.
func TestEveryPermissionIsAskedBySomeRoute(t *testing.T) {
	asked := map[[2]string]bool{}
	for _, c := range routePolicies {
		asked[[2]string{c.kind, c.action}] = true
	}
	// The verbs the route table above does not carry an example of, each
	// enforced by PolicyFor and each with a rule in config/access/policies.
	for _, extra := range [][2]string{
		{"system", "write"},
		{"software_download", "promote"},
		{"software_download", "apply"},
	} {
		asked[extra] = true
	}
	for _, def := range Catalogue {
		if !asked[[2]string{def.Kind, def.Action}] {
			t.Errorf("permission %s (%s/%s) is in the catalogue and no route asks for it",
				def.Name, def.Kind, def.Action)
		}
	}
}

// A permission's name is API. Two entries sharing one, or an entry whose name
// does not match the question it asks, would send an interface looking for a
// control that is never granted.
func TestPermissionNamesAreTheirQuestion(t *testing.T) {
	seen := map[Permission]bool{}
	for _, def := range Catalogue {
		if seen[def.Name] {
			t.Errorf("permission %s appears twice", def.Name)
		}
		seen[def.Name] = true
		if want := Permission(def.Kind + "." + def.Action); def.Name != want {
			t.Errorf("permission %s asks %s/%s, so it should be named %s", def.Name, def.Kind, def.Action, want)
		}
		if def.Title == "" {
			t.Errorf("permission %s has no title, so a refusal cannot name what was refused", def.Name)
		}
	}
}

// A product owner holds their products and nothing wider. This is the shape the
// interface renders from, so it is worth stating as a fact rather than as a
// consequence of two other tests.
func TestAProductOwnerHoldsTheirProductsAndNotTheEstate(t *testing.T) {
	id := fromAuthz(authz.Identity{
		Subject: "u1", Tenant: "default",
		OrgRoles: []string{"org-member"},
		Products: map[string][]string{"software-01": {"product-owner"}},
	})

	set := ResolveAccess(context.Background(), nil, id)

	if len(set.Global) != 0 {
		t.Errorf("a product owner holds %v tenant-wide; the estate is not theirs", set.Global)
	}
	held := set.ByProduct["software-01"]
	for _, want := range []Permission{PermProductDiscover, PermProductView, PermDownloadRequest, PermAuditView} {
		if !contains(held, string(want)) {
			t.Errorf("a product owner does not hold %s on their own product: %v", want, held)
		}
	}
	// The estate resources have no product tier, so they must not appear under
	// one - see PermissionDef.Scoped.
	for _, never := range []Permission{PermReportView, PermWorkerView, PermPolicyView, PermSystemView} {
		if contains(held, string(never)) {
			t.Errorf("a product owner was given the estate permission %s on a product", never)
		}
	}
	if _, other := set.ByProduct["software-02"]; other {
		t.Error("a product owner holds a product they were never granted")
	}
}

// An org reader holds reads tenant-wide, which covers products that do not
// exist yet - so nothing appears per product.
func TestAnOrgReaderHoldsReadsTenantWide(t *testing.T) {
	id := fromAuthz(authz.Identity{
		Subject: "u2", Tenant: "default", OrgRoles: []string{"org-reader"},
	})
	set := ResolveAccess(context.Background(), nil, id)
	if !contains(set.Global, string(PermAuditView)) {
		t.Errorf("an org reader may not view the audit trail: %v", set.Global)
	}
	if contains(set.Global, string(PermProductDiscover)) {
		t.Error("an org reader was given discovery, which is an operator's")
	}
	if len(set.ByProduct) != 0 {
		t.Errorf("a tenant-wide grant was copied into %v", set.ByProduct)
	}
}

// An account nobody provisioned holds nothing at all.
func TestAStrangerHoldsNothing(t *testing.T) {
	set := ResolveAccess(context.Background(), nil, Identity{Subject: "u3", Method: "oidc"})
	if len(set.Global) != 0 || len(set.ByProduct) != 0 {
		t.Errorf("an unprovisioned account holds %v / %v", set.Global, set.ByProduct)
	}
}

// A policy engine that cannot answer resolves to nothing, and SAYS it could not
// resolve rather than reporting an empty set as a fact about the account.
func TestAnUnreachableEngineSaysSoRatherThanReportingNoAccess(t *testing.T) {
	id := fromAuthz(authz.Identity{
		Subject: "u4", Tenant: "default", OrgRoles: []string{"org-admin"},
	})
	set := ResolveAccess(context.Background(), authz.NewCerbos("http://127.0.0.1:1"), id)
	if !set.Unavailable {
		t.Error("an unreachable policy engine was reported as an account with no access")
	}
	if len(set.Global) != 0 {
		t.Errorf("an unreachable policy engine granted %v", set.Global)
	}
}

// The refusal names the permission, so it can be quoted into a ticket and
// grepped for in config/access/policies.
func TestARefusalNamesThePermissionAndTheProduct(t *testing.T) {
	id := fromAuthz(authz.Identity{
		Subject: "u5", Tenant: "default",
		OrgRoles: []string{"org-member"},
		Products: map[string][]string{"software-01": {"product-reader"}},
	})
	req := RequiredFor(httptest.NewRequest("POST", "/api/v1/products/software-02:calibrate", nil))
	got := Refusal(id, req)
	for _, want := range []string{"Access denied", "product.calibrate", `"software-02"`} {
		if !containsSub(got, want) {
			t.Errorf("refusal %q does not name %q", got, want)
		}
	}
}

// A stranger is told they are a stranger, not that they lack one permission:
// the two have different answers and only one of them is actionable.
func TestAStrangerIsToldTheyAreNotProvisioned(t *testing.T) {
	got := Refusal(Identity{Subject: "u6", Method: "oidc"},
		RequiredFor(httptest.NewRequest("GET", "/api/v1/auditEvents", nil)))
	if !containsSub(got, "not provisioned") {
		t.Errorf("a stranger was told %q", got)
	}
}

// The catalogue's order must not leak into the answer: an interface diffing
// two reads of /whoami would see a change that is not one.
func TestTheAnswerIsSorted(t *testing.T) {
	id := fromAuthz(authz.Identity{
		Subject: "u7", Tenant: "default", OrgRoles: []string{"org-admin"},
	})
	set := ResolveAccess(context.Background(), nil, id)
	if !sort.StringsAreSorted(set.Global) {
		t.Errorf("global permissions are not sorted: %v", set.Global)
	}
}

func containsSub(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
