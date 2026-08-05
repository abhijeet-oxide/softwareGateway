package catalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// openTestStore returns a migrated SQLite store on a temp file.
func openTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()

	s, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := store.Migrate(ctx, s, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func testProduct(name, registry, sourcePath, targetPath string) *product.Product {
	return &product.Product{
		APIVersion: product.APIVersion,
		Kind:       product.Kind,
		Metadata:   product.Metadata{Name: name, Owner: name + "@example.com"},
		ConfigHash: "hash-" + name,
		SourceFile: name + ".yaml",
		Spec: product.Spec{
			Sources: []product.Source{{
				Name: "primary", Registry: registry, Repository: sourcePath,
				Type: product.RegistryGeneric, Anonymous: true,
			}},
			Targets: []product.Target{{
				Name: "lab", Registry: "internal.example.com", Repository: targetPath,
				Type: product.RegistryGeneric, Anonymous: true, Default: true,
			}},
		},
	}
}

func countRows(t *testing.T, s store.Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestReconcileCreatesCatalogRows(t *testing.T) {
	// Without these rows discovery cannot insert anything: packages carries
	// foreign keys to both products and repositories.
	s := openTestStore(t)
	c := NewCatalog(s)

	res, err := c.Reconcile(t.Context(), []*product.Product{
		testProduct("vendor-a", "registry.a.example.com", "a/suite", "vendor-a/platform"),
		testProduct("vendor-b", "registry.b.example.com", "b/db", "vendor-b/database"),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := countRows(t, s, "products"); got != 2 {
		t.Fatalf("expected 2 products, got %d", got)
	}
	if got := countRows(t, s, "repositories"); got != 4 {
		t.Fatalf("expected 4 repositories (2 per product), got %d", got)
	}
	if len(res.Products) != 2 {
		t.Fatalf("expected 2 product refs, got %d", len(res.Products))
	}
	if id := res.Products["vendor-a"].Repositories["primary"]; id == 0 {
		t.Fatal("expected a resolved source repository id for vendor-a/primary")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	// It runs at startup and after every config reload; repeated runs against
	// unchanged config must not accumulate rows.
	s := openTestStore(t)
	c := NewCatalog(s)
	products := []*product.Product{
		testProduct("vendor-a", "registry.a.example.com", "a/suite", "vendor-a/platform"),
	}

	for i := 0; i < 3; i++ {
		if _, err := c.Reconcile(t.Context(), products); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	if got := countRows(t, s, "products"); got != 1 {
		t.Fatalf("expected 1 product after 3 reconciles, got %d", got)
	}
	if got := countRows(t, s, "repositories"); got != 2 {
		t.Fatalf("expected 2 repositories after 3 reconciles, got %d", got)
	}
}

func TestReconcileDeactivatesRatherThanDeletes(t *testing.T) {
	// Discovery history, transfers and audit records reference these rows by
	// ID. Deleting one would orphan the very history the audit trail exists to
	// preserve, so removal from config must only deactivate.
	s := openTestStore(t)
	c := NewCatalog(s)

	both := []*product.Product{
		testProduct("vendor-a", "registry.a.example.com", "a/suite", "vendor-a/platform"),
		testProduct("vendor-b", "registry.b.example.com", "b/db", "vendor-b/database"),
	}
	if _, err := c.Reconcile(t.Context(), both); err != nil {
		t.Fatal(err)
	}

	// vendor-b is removed from configuration.
	if _, err := c.Reconcile(t.Context(), both[:1]); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, s, "products"); got != 2 {
		t.Fatalf("rows must be retained, got %d products", got)
	}

	var active int
	if err := s.DB().QueryRowContext(t.Context(),
		"SELECT count(*) FROM products WHERE active = 1").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("expected exactly 1 active product, got %d", active)
	}

	if err := s.DB().QueryRowContext(t.Context(),
		"SELECT count(*) FROM repositories WHERE active = 1").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Fatalf("expected 2 active repositories (vendor-a only), got %d", active)
	}
}

func TestReconcileHandlesConfigEdit(t *testing.T) {
	// An edited product is a new config_hash. The partial unique index on
	// (name) WHERE active permits only one live row, so the prior version must
	// be deactivated — while remaining present, so an audit record from before
	// the edit still resolves to the configuration in force then.
	s := openTestStore(t)
	c := NewCatalog(s)

	v1 := testProduct("vendor-a", "registry.a.example.com", "a/suite", "vendor-a/platform")
	if _, err := c.Reconcile(t.Context(), []*product.Product{v1}); err != nil {
		t.Fatal(err)
	}

	v2 := testProduct("vendor-a", "registry.a.example.com", "a/suite", "vendor-a/platform")
	v2.ConfigHash = "hash-vendor-a-edited"
	v2.Metadata.Owner = "new-owner@example.com"
	if _, err := c.Reconcile(t.Context(), []*product.Product{v2}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, s, "products"); got != 2 {
		t.Fatalf("both config versions should be retained, got %d", got)
	}

	var owner string
	if err := s.DB().QueryRowContext(t.Context(),
		"SELECT owner FROM products WHERE active = 1").Scan(&owner); err != nil {
		t.Fatalf("exactly one active row expected: %v", err)
	}
	if owner != "new-owner@example.com" {
		t.Fatalf("the active row should be the edited one, got owner %q", owner)
	}
}

func TestReconcileRefusesToStealAnotherProductsRepository(t *testing.T) {
	// A physical repository belongs to exactly one product: repositories is
	// uniquely indexed on (registry_host, repository_path) with a NOT NULL
	// product_id. Configuration validation rejects this before it gets here;
	// this is defence in depth, because the alternative failure is product_id
	// flipping on every reload — silent and baffling.
	s := openTestStore(t)
	c := NewCatalog(s)

	a := testProduct("vendor-a", "registry.a.example.com", "a/suite", "shared/base")
	b := testProduct("vendor-b", "registry.b.example.com", "b/db", "shared/base")

	_, err := c.Reconcile(t.Context(), []*product.Product{a, b})
	if err == nil {
		t.Fatal("expected reconciliation to refuse a contested repository")
	}
	if !contains(err.Error(), "already owned by product") {
		t.Fatalf("error should name the conflict, got: %v", err)
	}

	// And the refusal must be atomic: the whole reconcile rolls back.
	if got := countRows(t, s, "products"); got != 0 {
		t.Fatalf("a failed reconcile must leave no partial state, got %d products", got)
	}
}

func TestResolveRepository(t *testing.T) {
	s := openTestStore(t)
	c := NewCatalog(s)
	if _, err := c.Reconcile(t.Context(), []*product.Product{
		testProduct("vendor-a", "registry.a.example.com", "a/suite", "vendor-a/platform"),
	}); err != nil {
		t.Fatal(err)
	}

	id, err := c.ResolveRepository(t.Context(), "vendor-a", "primary")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id == 0 {
		t.Fatal("expected a non-zero id")
	}

	if _, err := c.ResolveRepository(t.Context(), "vendor-a", "nope"); err == nil {
		t.Fatal("expected an error for an unknown repository")
	}
	if _, err := c.ResolveRepository(t.Context(), "nope", "primary"); err == nil {
		t.Fatal("expected an error for an unknown product")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
