package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
)

// EVERY ROUTE THE ROUTER REGISTERS, and what it asks the policy engine.
//
// This list is maintained by hand against internal/api/router.go on purpose.
// The alternative - deriving it from chi at runtime - would test the mapping
// against itself, and the thing worth catching is a route that was added and
// never thought about, which a self-derived list would happily wave through.
//
// A route missing from here is not a failure of this test. A route whose
// mapping is WRONG is, and the reviewer's job when adding one is to write the
// line that says what it is.
var routePolicies = []struct {
	method, path string
	kind, action string
}{
	// The estate.
	{"GET", "/api/v1/auditEvents", "audit_event", "view"},
	{"GET", "/api/v1/reports/summary", "report", "view"},
	{"GET", "/api/v1/workers", "worker", "view"},
	{"GET", "/api/v1/policies", "policy_catalogue", "view"},
	{"GET", "/api/v1/policies/PRB-01", "policy_catalogue", "view"},
	{"GET", "/api/v1/system:healthCheck", "system", "view"},

	// Downloads.
	{"GET", "/api/v1/transfers", "software_download", "view"},
	{"GET", "/api/v1/transfers:activity", "software_download", "view"},
	{"GET", "/api/v1/transfers/t1", "software_download", "view"},
	{"GET", "/api/v1/transfers/t1/jobs", "software_download", "view"},
	{"GET", "/api/v1/transfers/t1/failures", "software_download", "view"},
	{"GET", "/api/v1/transfers/t1/present", "software_download", "view"},
	{"POST", "/api/v1/transfers", "software_download", "request"},
	{"POST", "/api/v1/transfers:retry", "software_download", "retry"},
	{"POST", "/api/v1/transfers/t1", "software_download", "cancel"},

	// Products.
	{"GET", "/api/v1/products", "product", "view"},
	{"GET", "/api/v1/products/software-01", "product", "view"},
	{"GET", "/api/v1/products/software-01/unavailable", "product", "view"},
	{"GET", "/api/v1/products/software-01/discovery", "product", "view"},
	{"POST", "/api/v1/products:discover", "product", "discover"},
	{"POST", "/api/v1/products/software-01/packages:discover", "product", "discover"},
	{"POST", "/api/v1/products:checkConnectivity", "product", "check_connectivity"},
	{"POST", "/api/v1/products/software-01:checkConnectivity", "product", "check_connectivity"},
	{"POST", "/api/v1/products/software-01:calibrate", "product", "calibrate"},

	// Packages.
	{"GET", "/api/v1/products/software-01/packages", "package", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1", "package", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/artifacts", "package", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/files", "package", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/files/content", "package", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/files/download", "package", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/promotionOptions", "package", "view"},
	{"POST", "/api/v1/products/software-01/packages/v1:inspect", "package", "inspect"},
	{"POST", "/api/v1/products/software-01/packages/v1:cancelAnalysis", "package", "inspect"},
	{"POST", "/api/v1/products/software-01/packages/v1:compare", "package", "inspect"},
	{"POST", "/api/v1/products/software-01/packages/v1:compareSecurity", "package", "inspect"},
	// Asking a scanner about a release, and stopping the asking. They reach a
	// third-party scanner and write what it says, so they are inspections
	// rather than reads - the same judgement package.yaml makes about walking a
	// manifest tree.
	{"POST", "/api/v1/products/software-01/packages/v1:syncSecurity", "package", "inspect"},
	{"POST", "/api/v1/products/software-01/packages/v1:cancelSecuritySync", "package", "inspect"},
	{"POST", "/api/v1/products/software-01/packages/v1:replicateSecurity", "package", "inspect"},
	{"POST", "/api/v1/products/software-01/packages/v1:cancelSecurityReplicate", "package", "inspect"},
	{"GET", "/api/v1/comparisons/tok", "package", "view"},

	// Security.
	{"GET", "/api/v1/products/software-01/packages/v1/security", "security_report", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/security/documents/sbom", "security_report", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/security/export", "security_report", "export"},
	{"GET", "/api/v1/products/software-01/packages/v1/security/compare/export", "security_report", "export"},
	{"GET", "/api/v1/products/software-01/security/search", "security_report", "view"},
	{"GET", "/api/v1/products/software-01/security/search/export", "security_report", "export"},

	// Compliance.
	{"GET", "/api/v1/products/software-01/packages/v1/compliance", "compliance_report", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/compliance/runs", "compliance_report", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/compliance/rendered", "compliance_report", "view"},
	{"GET", "/api/v1/products/software-01/packages/v1/compliance/export", "compliance_report", "export"},
	{"POST", "/api/v1/products/software-01/packages/v1/compliance:run", "compliance_report", "run"},
	{"POST", "/api/v1/products/software-01/packages/v1/compliance:cancel", "compliance_report", "cancel"},

	// Replication. The three verbs sit either side of two boundaries.
	{"GET", "/api/v1/products/software-01/replication", "replication", "view"},
	{"GET", "/api/v1/products/software-01/targets/t1/replication", "replication", "view"},
	{"GET", "/api/v1/products/software-01/targets/t1/syncs", "replication", "view"},
	{"POST", "/api/v1/products/software-01/targets/t1/replication:sync", "replication", "sync"},
	{"POST", "/api/v1/products/software-01/targets/t1/replication:cancelSync", "replication", "cancel_sync"},
	{"POST", "/api/v1/products/software-01/targets/t1/replication:apply", "replication", "apply"},

	// Rules and downloads under a product.
	{"GET", "/api/v1/products/software-01/autoDownloadRules", "download_rule", "view"},
	{"GET", "/api/v1/products/software-01/autoDownloadRules/r1/matches", "download_rule", "view"},
	{"GET", "/api/v1/products/software-01/downloads", "software_download", "view"},
	{"POST", "/api/v1/products/software-01/downloads:run", "software_download", "request"},
}

func TestEveryRouteAsksForSomethingSpecific(t *testing.T) {
	for _, c := range routePolicies {
		got := PolicyFor(httptest.NewRequest(c.method, c.path, nil))
		if got.Kind != c.kind || got.Action != c.action {
			t.Errorf("%s %s -> %s/%s, want %s/%s",
				c.method, c.path, got.Kind, got.Action, c.kind, c.action)
		}
	}
}

// The product is carried through to the policy, because the whole product tier
// turns on R.attr.product.
func TestAProductScopedRouteCarriesItsProduct(t *testing.T) {
	got := PolicyFor(httptest.NewRequest("GET", "/api/v1/products/software-01/packages", nil))
	if got.Product != "software-01" {
		t.Errorf("product = %q, want software-01", got.Product)
	}
	got = PolicyFor(httptest.NewRequest("GET", "/api/v1/auditEvents", nil))
	if got.Product != "" {
		t.Errorf("an estate route carried a product: %q", got.Product)
	}
}

// A route nobody mapped is refused rather than waved through, and a write on
// one is refused to everybody - which is the failure that gets it mapped.
func TestAnUnmappedRouteAsksSomethingNobodyCanAnswer(t *testing.T) {
	read := PolicyFor(httptest.NewRequest("GET", "/api/v1/inventedLater", nil))
	if read.Kind != "system" || read.Action != "view" {
		t.Errorf("unmapped read -> %s/%s", read.Kind, read.Action)
	}
	write := PolicyFor(httptest.NewRequest("POST", "/api/v1/inventedLater", nil))
	if write.Action != "write" {
		t.Errorf("unmapped write -> %s/%s, want an action no policy grants", write.Kind, write.Action)
	}
}

// ---------------------------------------------------------------------------
// Against a REAL Cerbos.
//
// Skipped unless CERBOS_ADDR is set, because a unit test must not need a
// container. It is run in this repository against the PDP in docker-compose,
// and it is the only thing that proves the policies and this mapping agree -
// a table test of the mapping alone proves the table.
// ---------------------------------------------------------------------------

func TestPolicyDecisionsAgainstCerbos(t *testing.T) {
	addr := os.Getenv("CERBOS_ADDR")
	if addr == "" {
		t.Skip("set CERBOS_ADDR to run against a live policy engine")
	}
	engine := authz.NewCerbos(addr)
	if err := engine.Health(context.Background()); err != nil {
		t.Fatalf("policy engine at %s: %v", addr, err)
	}

	var (
		admin    = identity("default", []string{"org-admin"}, nil)
		operator = identity("default", []string{"org-operator"}, nil)
		reader   = identity("default", []string{"org-reader"}, nil)
		security = identity("default", []string{"org-security"}, nil)
		owner    = identity("default", nil, map[string][]string{"software-01": {"product-owner"}})
		nobody   = identity("default", nil, nil)
	)

	cases := []struct {
		name         string
		id           Identity
		method, path string
		want         bool
	}{
		{"nobody reads products", nobody, "GET", "/api/v1/products", false},
		{"nobody reads the audit trail", nobody, "GET", "/api/v1/auditEvents", false},
		{"nobody requests a download", nobody, "POST", "/api/v1/transfers", false},

		{"reader reads packages", reader, "GET", "/api/v1/products/software-01/packages", true},
		{"reader reads the fleet", reader, "GET", "/api/v1/workers", true},
		{"reader requests a download", reader, "POST", "/api/v1/transfers", false},
		{"reader discovers", reader, "POST", "/api/v1/products/software-01/packages:discover", false},

		{"operator requests a download", operator, "POST", "/api/v1/transfers", true},
		{"operator discovers", operator, "POST", "/api/v1/products/software-01/packages:discover", true},
		{"operator syncs replication", operator, "POST",
			"/api/v1/products/software-01/targets/t1/replication:sync", true},
		// The line download.yaml draws and the ladder used to cross.
		{"operator applies replication", operator, "POST",
			"/api/v1/products/software-01/targets/t1/replication:apply", false},

		{"security reads the audit trail", security, "GET", "/api/v1/auditEvents", true},
		{"security exports security", security, "GET",
			"/api/v1/products/software-01/packages/v1/security/export", true},
		{"security requests a download", security, "POST", "/api/v1/transfers", false},

		{"admin applies replication", admin, "POST",
			"/api/v1/products/software-01/targets/t1/replication:apply", true},
		{"admin reads everything", admin, "GET", "/api/v1/reports/summary", true},

		{"owner reads their product", owner, "GET", "/api/v1/products/software-01/packages", true},
		{"owner reads another product", owner, "GET", "/api/v1/products/software-02/packages", false},
		{"owner applies on their product", owner, "POST",
			"/api/v1/products/software-01/targets/t1/replication:apply", true},
		{"owner applies on another", owner, "POST",
			"/api/v1/products/software-02/targets/t1/replication:apply", false},
		// The estate resources have no product tier, deliberately.
		{"owner reads the fleet", owner, "GET", "/api/v1/workers", false},
		{"owner reads the rollups", owner, "GET", "/api/v1/reports/summary", false},
		// ... except the audit trail, which the handler narrows by product.
		{"owner reads the audit trail", owner, "GET", "/api/v1/auditEvents", true},
		{"owner lists products", owner, "GET", "/api/v1/products", true},
		// THE HEADLINE CASE. A fleet-wide scan is one product owner asking
		// about the fleet they hold, and the handler narrows it to exactly
		// that - see handleDiscoverAll. Refusing it disabled the one control
		// this product exists to offer for the people most likely to use it.
		{"owner runs a fleet-wide scan", owner, "POST", "/api/v1/products:discover", true},
		{"owner scans their own product", owner, "POST",
			"/api/v1/products/software-01/packages:discover", true},
		{"owner scans another product", owner, "POST",
			"/api/v1/products/software-02/packages:discover", false},
		{"nobody runs a fleet-wide scan", nobody, "POST", "/api/v1/products:discover", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, detail := attemptWith(engine, c.id, c.method, c.path)
			if allowed != c.want {
				res := PolicyFor(httptest.NewRequest(c.method, c.path, nil))
				t.Fatalf("%s %s (%s/%s) allowed=%v want=%v %s",
					c.method, c.path, res.Kind, res.Action, allowed, c.want, detail)
			}
		})
	}
}

// An engine that cannot answer refuses. Cannot know is not yes.
func TestAnUnreachablePolicyEngineRefuses(t *testing.T) {
	// A port nothing is listening on, so Check returns a transport error.
	engine := authz.NewCerbos("http://127.0.0.1:1")
	allowed, detail := attemptWith(engine,
		identity("default", []string{"org-admin"}, nil), "GET", "/api/v1/products")
	if allowed {
		t.Fatal("a request was served while the policy engine was unreachable")
	}
	if detail == "" {
		t.Error("refused without saying why")
	}
}

func attemptWith(engine authz.Engine, id Identity, method, path string) (bool, string) {
	reached := false
	var denied string
	h := Authorize(engine, func(w http.ResponseWriter, _ *http.Request, detail string) {
		denied = detail
		w.WriteHeader(http.StatusForbidden)
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	r := httptest.NewRequest(method, path, nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, id))
	h.ServeHTTP(httptest.NewRecorder(), r)
	return reached, denied
}
