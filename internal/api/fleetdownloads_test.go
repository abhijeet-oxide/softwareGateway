package api

import (
	"net/http"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/download"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// THE WHOLE ESTATE'S DOWNLOADS IN ONE REQUEST.
//
// The Downloads page's Rules tab is built from these, and with only the
// per-product route it asked once PER PRODUCT the moment somebody clicked the
// tab - thirty requests on a real deployment, fired together, competing with
// each other and with the listing beside them for the browser's six
// connections per host. A download's status is derived from the product
// document in memory, so the fan-out bought nothing at all.
func TestFleetDownloadsCoverEveryProduct(t *testing.T) {
	h := newAPIHarnessWith(t, withDownloads, fourTargetDoc, secondFourTargetDoc)

	var out v1.ListDownloadsResponse
	if code := h.get("/api/v1/downloads", &out); code != http.StatusOK {
		t.Fatalf("fleet downloads = %d, want 200", code)
	}

	seen := map[string]bool{}
	for _, d := range out.Downloads {
		seen[d.Product] = true
	}
	for _, want := range []string{"vendor-a", "vendor-b"} {
		if !seen[want] {
			t.Errorf("no download for %s; the listing covered %v", want, seen)
		}
	}
}

func TestFleetAutoDownloadRulesCoverEveryProduct(t *testing.T) {
	h := newAPIHarnessWith(t, withDownloads, fourTargetDoc, secondFourTargetDoc)

	var out v1.ListAutoDownloadRulesResponse
	if code := h.get("/api/v1/autoDownloadRules", &out); code != http.StatusOK {
		t.Fatalf("fleet rules = %d, want 200", code)
	}
	// The shape is what matters here - the sample documents declare no rules,
	// and an estate with none must answer with an empty listing rather than a
	// null the browser has to guard.
	if out.Rules == nil {
		t.Error("the fleet rules listing answered null rather than an empty list")
	}
}

// THE ESTATE ROUTES MUST NOT SWALLOW THE PER-PRODUCT ONES.
//
// `/api/v1/downloads` and `/api/v1/products/{p}/downloads` differ by a prefix,
// and the policy table is a list of prefix matches evaluated in order. Put the
// estate case in the wrong place and a per-product read stops carrying its
// product - which does not fail, it just asks the policy engine a question
// about the whole tenant instead of about one product, and quietly answers it.
func TestAPerProductDownloadStillCarriesItsProduct(t *testing.T) {
	for _, tc := range []struct {
		path        string
		wantKind    string
		wantProduct string
	}{
		{"/api/v1/downloads", "software_download", ""},
		{"/api/v1/autoDownloadRules", "download_rule", ""},
		{"/api/v1/products/vendor-a/downloads", "software_download", "vendor-a"},
		{"/api/v1/products/vendor-a/autoDownloadRules", "download_rule", "vendor-a"},
	} {
		req, err := http.NewRequest(http.MethodGet, "http://x"+tc.path, nil) //nolint:noctx // table test
		if err != nil {
			t.Fatal(err)
		}
		got := middleware.PolicyFor(req)
		if got.Kind != tc.wantKind {
			t.Errorf("%s asks about %q, want %q", tc.path, got.Kind, tc.wantKind)
		}
		if got.Product != tc.wantProduct {
			t.Errorf("%s carries product %q, want %q", tc.path, got.Product, tc.wantProduct)
		}
	}
}

// The estate routes are reachable by a caller who holds the action on ANY
// product, which is only safe because the handlers narrow to VisibleProducts.
// The two change together or not at all.
func TestTheFleetDownloadRoutesAreAnyScope(t *testing.T) {
	for _, path := range []string{"/api/v1/downloads", "/api/v1/autoDownloadRules"} {
		req, err := http.NewRequest(http.MethodGet, "http://x"+path, nil) //nolint:noctx // table test
		if err != nil {
			t.Fatal(err)
		}
		if !middleware.RequiredFor(req).AnyScope {
			t.Errorf("%s is not AnyScope, so a caller scoped to one product is "+
				"refused the estate-wide read the page needs", path)
		}
	}
	// And the per-product routes are NOT, because they name their product.
	req, err := http.NewRequest(http.MethodGet, //nolint:noctx // table test
		"http://x/api/v1/products/vendor-a/downloads", nil)
	if err != nil {
		t.Fatal(err)
	}
	if middleware.RequiredFor(req).AnyScope {
		t.Error("a per-product download read is AnyScope, which would let a " +
			"caller scoped to one product read another's")
	}
}

// withDownloads wires the download surface, which the shared harness leaves
// out - so the routes that need it are not registered at all without this.
func withDownloads(d *Deps) {
	d.Downloads = download.NewService(d.Packages, nil, nil)
}
