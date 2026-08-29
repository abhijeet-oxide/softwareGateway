package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A product whose document is refused must still be REPORTED.
//
// It used to be dropped from the API, which meant the screen simply did not
// mention it: no row, no name, no reason, and the only record a line in the
// Coordinator's log. These tests pin the two cases apart, because conflating
// them is the failure mode - one product does nothing at all, the other is
// running perfectly well and merely ignoring somebody's last edit.

const validDoc = `apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: good
  displayName: Good Product
spec:
  sources:
    - name: vendor
      registry: registry.example.com:9443
      repository: vendor/good
      anonymous: true
  targets:
    - name: lab
      registry: registry.example.com:9444
      repository: lab/good
      anonymous: true
      default: true
`

// An unknown field: the loader parses strictly, so this never yields a
// product at all - the failure has no structure and no name of its own.
const unparseableDoc = `apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: broken
spec:
  sources:
    - name: vendor
      registry: registry.example.com:9443
      repository: vendor/broken
      anonymous: true
      tagPatern: "v*"
`

// Parses cleanly and fails VALIDATION, so the failure arrives as product.Errors
// and each part names the field it is about.
const invalidDoc = `apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: rejected
  displayName: Rejected Product
spec:
  sources:
    - name: vendor
      registry: ""
      repository: vendor/rejected
      anonymous: true
`

func loadDir(t *testing.T, docs map[string]string) *product.Registry {
	t.Helper()
	dir := t.TempDir()
	for name, doc := range docs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := product.NewLoader(dir, product.NewSecretResolver(t.TempDir())).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := product.NewRegistry()
	reg.Swap(res)
	return reg
}

func listProducts(t *testing.T, reg *product.Registry) map[string]v1.Product {
	t.Helper()
	ts := httptest.NewServer(NewServer(Deps{Products: reg}).Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/products") //nolint:noctx // test
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var out v1.ListProductsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]v1.Product{}
	for _, p := range out.Products {
		byID[p.ProductID] = p
	}
	return byID
}

func TestListProductsIncludesRejected(t *testing.T) {
	reg := loadDir(t, map[string]string{"good.yaml": validDoc, "rejected.yaml": invalidDoc})
	got := listProducts(t, reg)

	if len(got) != 2 {
		t.Fatalf("listed %d products, want 2 (the valid one AND the rejected one): %v", len(got), got)
	}
	if p := got["good"]; !p.Enabled || p.ConfigError != nil {
		t.Errorf("valid product reported as broken: enabled=%v configError=%+v", p.Enabled, p.ConfigError)
	}

	bad, ok := got["rejected"]
	if !ok {
		t.Fatal("the rejected product is missing from the listing, which is the whole bug")
	}
	if bad.Enabled {
		t.Error("a product that never loaded is not enabled")
	}
	if bad.ConfigError == nil {
		t.Fatal("no configError on a rejected product")
	}
	if bad.ConfigError.Loaded {
		t.Error("loaded = true, but no version of this product has ever loaded")
	}
	if bad.ConfigError.File != "rejected.yaml" {
		t.Errorf("file = %q, want the document to open", bad.ConfigError.File)
	}
	// The structure is the point: a joined string cannot be read back apart,
	// and the field is what sends somebody to the right line.
	if len(bad.ConfigError.Details) == 0 {
		t.Fatal("a validation failure must keep its per-field detail")
	}
	if bad.ConfigError.Details[0].Field == "" {
		t.Error("a detail with no field cannot be acted on")
	}
}

// A bad edit to a WORKING product leaves the previous version running. The
// product is fine; the change is what failed, and saying "not loaded" over it
// would send somebody to fix an outage that is not happening.
func TestListProductsReportsRejectedEditSeparately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(path, []byte(validDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := product.NewLoader(dir, product.NewSecretResolver(t.TempDir()))

	first, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg := product.NewRegistry()
	reg.Swap(first)

	// Now break the same file and reload, as the watcher does on every save.
	broken := validDoc + "  thisFieldDoesNotExist: true\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Swap(second)

	p, ok := listProducts(t, reg)["good"]
	if !ok {
		t.Fatal("a product with a rejected EDIT vanished; it is still running")
	}
	if !p.Enabled {
		t.Error("enabled = false, but the previous configuration is still running")
	}
	if p.ConfigError == nil {
		t.Fatal("the rejected edit was not reported at all")
	}
	if !p.ConfigError.Loaded {
		t.Error("loaded = false, but this product is running its previous version")
	}
	// The retained configuration is the OLD one, so its facts must still be there.
	if p.DisplayName != "Good Product" {
		t.Errorf("displayName = %q, want the retained configuration's", p.DisplayName)
	}
}

// A document that fails to PARSE never yields a product name, so it is filed
// under its file. It must still reach the listing - a file somebody has just
// created and got wrong is exactly the case they need to see.
func TestListProductsIncludesUnparseable(t *testing.T) {
	got := listProducts(t, loadDir(t, map[string]string{"broken.yaml": unparseableDoc}))

	p, ok := got["broken"]
	if !ok {
		t.Fatalf("an unparseable document is missing from the listing: %v", got)
	}
	if p.ConfigError == nil || p.ConfigError.Loaded {
		t.Fatalf("configError = %+v, want a not-loaded failure", p.ConfigError)
	}
	// No structure to report: a parse error is one message, and printing it
	// verbatim is the honest thing to do.
	if len(p.ConfigError.Details) != 0 {
		t.Errorf("details = %v, want none for a parse error", p.ConfigError.Details)
	}
	if p.ConfigError.Message == "" {
		t.Error("a parse error with no message tells the reader nothing")
	}
}

// The Get method must agree with the List method. A resource the listing
// returns cannot be missing from the collection it came from, and "not found"
// is untrue of a product that is configured and merely not running.
func TestGetRejectedProductReturnsItRatherThan404(t *testing.T) {
	reg := loadDir(t, map[string]string{"rejected.yaml": invalidDoc})
	ts := httptest.NewServer(NewServer(Deps{Products: reg}).Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/products/rejected") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the product and its reason", resp.StatusCode)
	}
	var p v1.Product
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.ProductID != "rejected" || p.ConfigError == nil {
		t.Fatalf("got %+v, want the rejected product carrying its configError", p)
	}
	if p.Enabled {
		t.Error("a product that never loaded is not enabled")
	}
}

// A name nobody configured is still a 404. Reporting every typo as a rejected
// product would make the new state meaningless.
func TestGetUnknownProductIsStillNotFound(t *testing.T) {
	reg := loadDir(t, map[string]string{"good.yaml": validDoc})
	ts := httptest.NewServer(NewServer(Deps{Products: reg}).Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/products/never-heard-of-it") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
