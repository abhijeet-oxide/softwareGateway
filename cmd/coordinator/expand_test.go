package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// The Coordinator freeze.
//
// EnsureTarget opens a transaction and then needed the product's row. It read
// that row through the POOL. On SQLite the pool is one connection by
// construction (internal/store/sqlite.go), which the open transaction was
// already holding - so the read waited for a connection only this goroutine
// could release, and waited forever. The handler never returned, the
// connection was never given back, and every request that touched the database
// afterwards queued behind it: worker heartbeats, job leases, completions and
// the whole UI, until the process was restarted. The Coordinator logs for it
// are a wall of `begin lease transaction: context canceled` and `could not
// record job completion`, one per request that gave up on its own deadline.
//
// EnsureTarget is on the PROMOTION path - a target with no catalog row is
// exactly a target nothing has been promoted into yet - so the trigger was
// pressing Promote for the first time on a fresh destination.
//
// The test is written as "does it finish", not "does it return the right ID",
// because finishing is the property that was lost. It runs on its own clock:
// a failure here would otherwise be the whole package timing out ten minutes
// later with no indication of which test hung.
func TestEnsureTargetDoesNotDeadlockOnTheSingleConnection(t *testing.T) {
	r := newResolverForTest(t)

	done := make(chan int64, 1)
	errs := make(chan error, 1)
	go func() {
		id, err := r.EnsureTarget(t.Context(), "vendor-a", transfer.RepoView{
			Name: "prod", Role: "target", Registry: "eu.jfrog.io",
			Repository: "vendor-prod", RegistryType: "generic",
		})
		if err != nil {
			errs <- err
			return
		}
		done <- id
	}()

	select {
	case err := <-errs:
		t.Fatalf("EnsureTarget: %v", err)
	case id := <-done:
		if id == 0 {
			t.Fatal("EnsureTarget returned row 0; a transfer cannot point at it")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("EnsureTarget deadlocked: it is holding the only database " +
			"connection while waiting for another one, which is the freeze " +
			"that takes the whole Coordinator down until it is restarted")
	}
}

// A second call returns the SAME row rather than a second one. EnsureRepository
// is idempotent and must stay so: promoting into a target twice is ordinary,
// and a duplicate row would split one target's history in two.
func TestEnsureTargetIsIdempotent(t *testing.T) {
	r := newResolverForTest(t)
	target := transfer.RepoView{
		Name: "prod", Role: "target", Registry: "eu.jfrog.io",
		Repository: "vendor-prod", RegistryType: "generic",
	}

	first, err := r.EnsureTarget(t.Context(), "vendor-a", target)
	if err != nil {
		t.Fatalf("first EnsureTarget: %v", err)
	}
	second, err := r.EnsureTarget(t.Context(), "vendor-a", target)
	if err != nil {
		t.Fatalf("second EnsureTarget: %v", err)
	}
	if first != second {
		t.Errorf("EnsureTarget created a second row: %d then %d", first, second)
	}
}

// newResolverForTest builds the two fields EnsureTarget uses - the configured
// products and the store - against a real migrated SQLite database, because
// the single-connection pool IS the thing under test.
func newResolverForTest(t *testing.T) *resolverImpl {
	t.Helper()

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "expand.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a','h','{}')`); err != nil {
		t.Fatal(err)
	}

	products := product.NewRegistry()
	products.Swap(product.LoadResult{Valid: []*product.Product{{
		Metadata: product.Metadata{Name: "vendor-a"},
	}}})

	return &resolverImpl{products: products, packages: store.NewPackages(st)}
}
