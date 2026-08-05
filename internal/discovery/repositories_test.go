package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// ---------------------------------------------------------------------------
// Repository set resolution — no registry needed
// ---------------------------------------------------------------------------

// stubCatalog stands in for a registry catalog.
type stubCatalog struct {
	repos []string
	err   error
	calls int
}

func (s *stubCatalog) ListAllRepositories(_ context.Context, maxEntries int) ([]string, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if len(s.repos) > maxEntries {
		return s.repos[:maxEntries], nil
	}
	return s.repos, nil
}

func TestRepositorySingularAndPluralMerge(t *testing.T) {
	src := product.Source{
		Repository:   "platform/core",
		Repositories: []string{"platform/db", "platform/core", "platform/ui"},
	}

	got := src.DeclaredRepositories()
	want := []string{"platform/core", "platform/db", "platform/ui"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v — `repository` and `repositories` must merge and dedupe", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRepositoryPathsAreNormalised(t *testing.T) {
	src := product.Source{Repositories: []string{"/platform/core/", "  platform/db  ", ""}}
	got := src.DeclaredRepositories()

	if len(got) != 2 {
		t.Fatalf("expected 2 repositories after trimming, got %v", got)
	}
	for _, r := range got {
		if strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") || strings.TrimSpace(r) != r {
			t.Errorf("%q was not normalised", r)
		}
	}
}

func TestResolveRepositoriesExplicitOnly(t *testing.T) {
	src := product.Source{Repositories: []string{"b/two", "a/one"}}
	f, _ := compileFilters("repositoryFilters", product.Filters{})

	res := resolveRepositories(t.Context(), src, f, nil)

	// Sorted, so scan order and therefore log and row-creation order are
	// stable across restarts.
	if len(res.Repositories) != 2 || res.Repositories[0] != "a/one" {
		t.Fatalf("expected a sorted set, got %v", res.Repositories)
	}
	if res.FromCatalog != 0 {
		t.Errorf("nothing came from a catalog, got %d", res.FromCatalog)
	}
}

func TestResolveRepositoriesFromCatalog(t *testing.T) {
	src := product.Source{RepositoryDiscovery: product.RepositoryDiscovery{Enabled: true}}
	cat := &stubCatalog{repos: []string{"platform/core", "platform/db", "other/thing"}}
	f, _ := compileFilters("repositoryFilters", product.Filters{Include: []string{`^platform/`}})

	res := resolveRepositories(t.Context(), src, f, cat)

	if len(res.Repositories) != 2 {
		t.Fatalf("expected the filter to keep 2, got %v", res.Repositories)
	}
	if res.FromCatalog != 2 {
		t.Errorf("expected 2 from the catalog, got %d", res.FromCatalog)
	}
	if res.Filtered != 1 {
		t.Errorf("expected 1 rejected, got %d", res.Filtered)
	}
}

// Filters apply to explicitly named repositories too. One rule, whichever way
// the set was obtained.
func TestFiltersApplyToExplicitRepositories(t *testing.T) {
	src := product.Source{Repositories: []string{"platform/core", "platform/core-test"}}
	f, _ := compileFilters("repositoryFilters", product.Filters{Exclude: []string{`-test$`}})

	res := resolveRepositories(t.Context(), src, f, nil)

	if len(res.Repositories) != 1 || res.Repositories[0] != "platform/core" {
		t.Fatalf("expected the -test repository excluded, got %v", res.Repositories)
	}
}

// A catalog that refuses enumeration must not stop the declared repositories
// from being scanned. This is the common vendor case: the credential is good
// for pulling a named repository and not for listing the registry.
func TestCatalogFailureKeepsDeclaredRepositories(t *testing.T) {
	src := product.Source{
		Repositories:        []string{"platform/core"},
		RepositoryDiscovery: product.RepositoryDiscovery{Enabled: true},
	}
	cat := &stubCatalog{err: registry.ErrForbidden}
	f, _ := compileFilters("repositoryFilters", product.Filters{})

	res := resolveRepositories(t.Context(), src, f, cat)

	if len(res.Repositories) != 1 || res.Repositories[0] != "platform/core" {
		t.Fatalf("declared repositories must survive a catalog failure, got %v", res.Repositories)
	}
	if res.CatalogErr == nil {
		t.Error("the catalog failure must still be reported")
	}
	if !strings.Contains(describeCatalogError(res.CatalogErr), "credential") {
		t.Errorf("a 403 should be explained as a credential scope problem, got: %s",
			describeCatalogError(res.CatalogErr))
	}
}

func TestCatalogCapTruncates(t *testing.T) {
	src := product.Source{RepositoryDiscovery: product.RepositoryDiscovery{
		Enabled: true, MaxRepositories: 2,
	}}
	cat := &stubCatalog{repos: []string{"a/1", "a/2", "a/3", "a/4"}}
	f, _ := compileFilters("repositoryFilters", product.Filters{})

	res := resolveRepositories(t.Context(), src, f, cat)

	if len(res.Repositories) != 2 {
		t.Fatalf("expected the cap to bound the set, got %v", res.Repositories)
	}
	if !res.Truncated {
		t.Error("truncation must be reported so the operator can widen the cap")
	}
}

func TestCatalogNotConsultedWhenDisabled(t *testing.T) {
	src := product.Source{Repositories: []string{"a/one"}}
	cat := &stubCatalog{repos: []string{"b/two"}}
	f, _ := compileFilters("repositoryFilters", product.Filters{})

	res := resolveRepositories(t.Context(), src, f, cat)

	if cat.calls != 0 {
		t.Error("repositoryDiscovery is off; the catalog must not be called")
	}
	if len(res.Repositories) != 1 || res.Repositories[0] != "a/one" {
		t.Errorf("got %v", res.Repositories)
	}
}

func TestDescribeCatalogError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"forbidden": {registry.ErrForbidden, "credential"},
		"not found": {registry.ErrNotFound, "does not implement"},
		"other":     {errors.New("boom"), "boom"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := describeCatalogError(tc.err); !strings.Contains(got, tc.want) {
				t.Errorf("describeCatalogError(%v) = %q, want it to mention %q", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// End to end against a real Distribution v2 server
// ---------------------------------------------------------------------------

// multiRepoHarness wires a scanner over a source covering several repositories.
type multiRepoHarness struct {
	t       *testing.T
	reg     *fakeregistry.Registry
	store   store.Store
	scanner *Scanner
}

func newMultiRepoHarness(t *testing.T, doc string) *multiRepoHarness {
	t.Helper()
	ctx := t.Context()

	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	st, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "multi.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(ctx, st, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	p := loadProduct(t, strings.ReplaceAll(doc, "REGISTRY_HOST", reg.Host()))
	rec, err := catalog.NewCatalog(st).Reconcile(ctx, []*product.Product{p})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ref := rec.Products[p.Metadata.Name]

	src, _ := p.Source("vendor")

	fastTransport := transport.Config{
		MaxRetryAttempts: 2, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
	}

	newClient := func(repoPath string) (registry.Source, error) {
		return generic.New(generic.Config{
			Registry: reg.Host(), Repository: repoPath, PlainHTTP: true, Transport: fastTransport,
		})
	}

	var cat CatalogLister
	if src.RepositoryDiscovery.Enabled {
		c, err := generic.NewCatalog(generic.CatalogConfig{
			Registry: reg.Host(), PlainHTTP: true, Transport: fastTransport,
		})
		if err != nil {
			t.Fatalf("build catalog client: %v", err)
		}
		cat = c
	}

	scanner, err := NewScanner(ScannerConfig{
		Packages:   store.NewPackages(st),
		Product:    p,
		ProductID:  ref.ID,
		SourceName: "vendor",
		NewClient:  newClient,
		Catalog:    cat,
		RepoIDs:    ref.Repositories,
	})
	if err != nil {
		t.Fatalf("build scanner: %v", err)
	}

	return &multiRepoHarness{t: t, reg: reg, store: st, scanner: scanner}
}

func (h *multiRepoHarness) scan() ScanResult {
	h.t.Helper()
	res, err := h.scanner.Scan(h.t.Context())
	if err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	for _, e := range res.RepositoryErrors {
		h.t.Errorf("unexpected repository error: %v", e)
	}
	for _, e := range res.TagErrors {
		h.t.Errorf("unexpected tag error: %v", e)
	}
	return res
}

func (h *multiRepoHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

const multiRepoDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      repositories:
        - platform/core
        - platform/db
        - platform/ui
      anonymous: true
      discovery:
        enabled: true
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repository: mirror/vendor-a
      anonymous: true
      default: true
`

func TestScanCoversEveryDeclaredRepository(t *testing.T) {
	h := newMultiRepoHarness(t, multiRepoDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.reg.AddImage("platform/core", "v1.1.0", fakeregistry.NewLayer("core2"))
	h.reg.AddImage("platform/db", "v2.0.0", fakeregistry.NewLayer("db"))
	h.reg.AddImage("platform/ui", "v3.0.0", fakeregistry.NewLayer("ui"))

	res := h.scan()

	if res.Repositories != 3 {
		t.Fatalf("expected 3 repositories scanned, got %d", res.Repositories)
	}
	if res.New != 4 {
		t.Fatalf("expected 4 packages across the three repositories, got %d", res.New)
	}
	if got := h.count(`SELECT COUNT(*) FROM repositories WHERE role='source'`); got != 3 {
		t.Errorf("expected 3 source repository rows, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages`); got != 4 {
		t.Errorf("expected 4 package rows, got %d", got)
	}
}

// The same tag in two repositories is two independent packages. Supersession
// is scoped to one repository, so v1.0.0 in platform/db must not touch v1.0.0
// in platform/core.
func TestSameTagInDifferentRepositoriesIsIndependent(t *testing.T) {
	h := newMultiRepoHarness(t, multiRepoDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.reg.AddImage("platform/db", "v1.0.0", fakeregistry.NewLayer("db"))
	h.reg.AddImage("platform/ui", "v1.0.0", fakeregistry.NewLayer("ui"))

	res := h.scan()

	if res.New != 3 {
		t.Fatalf("expected 3 packages, got %d", res.New)
	}
	if res.Superseded != 0 {
		t.Fatalf("the same tag in different repositories must not supersede, got %d", res.Superseded)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE state='superseded'`); got != 0 {
		t.Errorf("expected no superseded packages, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE tag='v1.0.0'`); got != 3 {
		t.Errorf("expected 3 coexisting v1.0.0 packages, got %d", got)
	}
}

func TestRescanAcrossRepositoriesIsIdempotent(t *testing.T) {
	h := newMultiRepoHarness(t, multiRepoDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.reg.AddImage("platform/db", "v2.0.0", fakeregistry.NewLayer("db"))

	if first := h.scan(); first.New != 2 {
		t.Fatalf("first scan: expected 2 new, got %d", first.New)
	}
	for i := range 3 {
		if res := h.scan(); res.New != 0 {
			t.Fatalf("re-scan %d: expected 0 new, got %d", i+1, res.New)
		}
	}
	if got := h.count(`SELECT COUNT(*) FROM packages`); got != 2 {
		t.Errorf("expected 2 packages after 4 scans, got %d", got)
	}
}

// One unreachable repository must not hide the others.
func TestOneBadRepositoryDoesNotStopTheScan(t *testing.T) {
	h := newMultiRepoHarness(t, multiRepoDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.reg.AddImage("platform/db", "v2.0.0", fakeregistry.NewLayer("db"))
	h.reg.AddImage("platform/ui", "v3.0.0", fakeregistry.NewLayer("ui"))

	h.reg.FailNext("platform/db/tags/list", 500, 100)

	res, err := h.scanner.Scan(t.Context())
	if err != nil {
		t.Fatalf("one bad repository must not fail the scan: %v", err)
	}
	if len(res.RepositoryErrors) != 1 {
		t.Fatalf("expected 1 repository error, got %d", len(res.RepositoryErrors))
	}
	if res.New != 2 {
		t.Errorf("expected the other 2 repositories discovered anyway, got %d", res.New)
	}
}

// But if EVERY repository fails, the scan failed — otherwise the last-success
// gauge would keep advancing through an outage.
func TestTotalOutageIsAFailedScan(t *testing.T) {
	h := newMultiRepoHarness(t, multiRepoDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.reg.AddImage("platform/db", "v2.0.0", fakeregistry.NewLayer("db"))
	h.reg.AddImage("platform/ui", "v3.0.0", fakeregistry.NewLayer("ui"))

	h.reg.FailNext("tags/list", 503, 1000)

	if _, err := h.scanner.Scan(t.Context()); err == nil {
		t.Fatal("a scan where every repository failed must report failure")
	}

	h.reg.ClearFaults()
	if res := h.scan(); res.New != 3 {
		t.Errorf("expected recovery to find all 3, got %d", res.New)
	}
}

const catalogDiscoveryDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      anonymous: true
      repositoryDiscovery:
        enabled: true
      repositoryFilters:
        include: ['^platform/']
        exclude: ['-test$']
      discovery:
        enabled: true
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repository: mirror/vendor-a
      anonymous: true
      default: true
`

func TestRepositoryDiscoveryFromCatalog(t *testing.T) {
	h := newMultiRepoHarness(t, catalogDiscoveryDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.reg.AddImage("platform/db", "v2.0.0", fakeregistry.NewLayer("db"))
	h.reg.AddImage("platform/db-test", "v0.1.0", fakeregistry.NewLayer("test"))
	h.reg.AddImage("other/thing", "v9.0.0", fakeregistry.NewLayer("other"))

	res := h.scan()

	if res.Repositories != 2 {
		t.Fatalf("expected 2 repositories after filtering, got %d", res.Repositories)
	}
	if res.RepositoriesFromCatalog != 2 {
		t.Errorf("expected both to come from the catalog, got %d", res.RepositoriesFromCatalog)
	}
	if res.RepositoriesFiltered != 2 {
		t.Errorf("expected the -test and other/ repositories rejected, got %d", res.RepositoriesFiltered)
	}
	if res.New != 2 {
		t.Errorf("expected 2 packages, got %d", res.New)
	}

	// Rows created by discovery are marked so, and reconciliation must not own
	// them.
	if got := h.count(`SELECT COUNT(*) FROM repositories WHERE managed_by='discovery'`); got != 2 {
		t.Errorf("expected 2 discovery-managed rows, got %d", got)
	}
}

// A repository published after the loop started must be found on the next scan,
// without a restart or a configuration reload.
func TestNewRepositoryIsFoundOnTheNextScan(t *testing.T) {
	h := newMultiRepoHarness(t, catalogDiscoveryDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	if res := h.scan(); res.Repositories != 1 {
		t.Fatalf("expected 1 repository initially, got %d", res.Repositories)
	}

	h.reg.AddImage("platform/newcomer", "v1.0.0", fakeregistry.NewLayer("new"))

	res := h.scan()
	if res.Repositories != 2 {
		t.Fatalf("a repository published since the last scan must be found, got %d", res.Repositories)
	}
	if res.New != 1 {
		t.Errorf("expected the newcomer's package, got %d new", res.New)
	}
}

// Reconciliation must not deactivate rows discovery created, or every config
// reload would flap them.
func TestReconcileDoesNotTouchDiscoveredRepositories(t *testing.T) {
	h := newMultiRepoHarness(t, catalogDiscoveryDoc)

	h.reg.AddImage("platform/core", "v1.0.0", fakeregistry.NewLayer("core"))
	h.scan()

	if got := h.count(`SELECT COUNT(*) FROM repositories WHERE managed_by='discovery' AND active=1`); got != 1 {
		t.Fatalf("expected 1 active discovered row, got %d", got)
	}

	// Simulate a configuration reload.
	p := loadProduct(t, strings.ReplaceAll(catalogDiscoveryDoc, "REGISTRY_HOST", h.reg.Host()))
	if _, err := catalog.NewCatalog(h.store).Reconcile(t.Context(), []*product.Product{p}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := h.count(`SELECT COUNT(*) FROM repositories WHERE managed_by='discovery' AND active=1`); got != 1 {
		t.Errorf("reconciliation deactivated a discovered repository; it must not own those rows")
	}
}

func TestScannerRejectsSourceWithNoRepositories(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      anonymous: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/x
      anonymous: true
      default: true
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := product.NewLoader(dir, product.NewSecretResolver(t.TempDir()))
	res, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Validation should reject it before a scanner is ever built.
	if len(res.Invalid) != 1 {
		t.Fatalf("expected the document to be rejected, got %d invalid", len(res.Invalid))
	}
	if !strings.Contains(res.Invalid[0].Err.Error(), "no repositories declared") {
		t.Errorf("error should name the problem, got: %v", res.Invalid[0].Err)
	}
}
