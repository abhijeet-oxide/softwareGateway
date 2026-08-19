package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// blockingCatalog holds a scan open until it is released, so a test can be
// certain a second trigger arrives while the first is still running.
type blockingCatalog struct {
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func newBlockingCatalog() *blockingCatalog {
	return &blockingCatalog{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingCatalog) ListAllRepositories(ctx context.Context, _ int) ([]string, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	select {
	case b.entered <- struct{}{}:
	default:
	}

	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

func (b *blockingCatalog) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestConcurrentTriggersCollapseIntoTheRunningScan is the regression test for
// the reported symptom: `packages discover` sometimes scanned, and sometimes
// returned instantly having done nothing.
//
// The old implementation handed the trigger over through a one-deep buffered
// channel and, when it found the slot occupied, returned w.last - the previous
// scan's result, or the ZERO VALUE if none had completed - with no error. The
// caller saw a successful scan of zero repositories in 0ms and the CLI printed
// "Nothing new. A scan that finds nothing is the normal steady state."
//
// Now a second trigger joins the running scan and gets its real result, marked
// Collapsed.
func TestConcurrentTriggersCollapseIntoTheRunningScan(t *testing.T) {
	cat := newBlockingCatalog()
	h := newCollapseHarness(t, cat)

	// First trigger: starts a scan, which blocks inside the catalog call.
	first := make(chan ScanResult, 1)
	go func() {
		res, err := h.loop.TriggerProduct(t.Context(), "vendor-a")
		if err != nil {
			t.Errorf("first trigger: %v", err)
		}
		first <- res
	}()

	select {
	case <-cat.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first scan never reached the registry")
	}

	// Second trigger, while the first is unambiguously still running.
	second := make(chan ScanResult, 1)
	go func() {
		res, err := h.loop.TriggerProduct(t.Context(), "vendor-a")
		if err != nil {
			t.Errorf("second trigger: %v", err)
		}
		second <- res
	}()

	// It must NOT return before the running scan finishes. That immediate
	// return is the whole bug.
	select {
	case res := <-second:
		t.Fatalf("the second trigger returned while a scan was still running: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}

	close(cat.release)

	got1 := recvScan(t, first)
	got2 := recvScan(t, second)

	// The second trigger arrived while a scan was demonstrably running, so it
	// must report that it joined one rather than claiming it ran its own.
	if !got2.Collapsed {
		t.Error("the second trigger is not marked Collapsed, so a caller cannot " +
			"tell a joined scan from one it started")
	}
	// Same scan, so the same numbers - not a zero value pretending to be one.
	if got1.Duration != got2.Duration {
		t.Errorf("the two callers saw different scans: %v vs %v", got1.Duration, got2.Duration)
	}
	if got1.Duration == 0 {
		t.Error("the scan reports zero duration; it did not really run")
	}
	if n := cat.count(); n != 1 {
		t.Errorf("the registry was enumerated %d times; concurrent triggers must collapse into one", n)
	}
}

// TestEnumeratingSourceWithoutACatalogFails covers the other path that produced
// an instant, empty, successful scan: a source that names no repositories and
// has no catalog client silently resolved to zero repositories, made no network
// call, and reported success.
func TestEnumeratingSourceWithoutACatalogFails(t *testing.T) {
	src := product.Source{
		Name: "vendor", Registry: "registry.example.com", Anonymous: true,
	}
	if !src.EnumeratesRepositories() {
		t.Fatal("a source naming no repositories must enumerate")
	}

	set := resolveRepositories(t.Context(), src, filter{}, nil)

	if set.CatalogErr == nil {
		t.Fatal("expected an error: enumeration was skipped and the scan would " +
			"have reported success having looked at nothing")
	}
	if len(set.Repositories) != 0 {
		t.Errorf("expected no repositories, got %v", set.Repositories)
	}
}

// ---- harness ----

type collapseHarness struct {
	loop *Loop
}

func newCollapseHarness(t *testing.T, cat CatalogLister) *collapseHarness {
	t.Helper()

	packages := newCollapseStore(t)

	p := &product.Product{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec: product.Spec{Sources: []product.Source{{
			Name: "primary", Registry: "registry.example.com", Anonymous: true,
		}}},
	}

	loop := NewLoop(nil, nil)
	t.Cleanup(loop.Stop)

	err := loop.Start(t.Context(), []SourceSpec{{
		Product:    p,
		ProductID:  1,
		SourceName: "primary",
		RepoIDs:    map[string]int64{},
		NewClient: func(string) (registry.Source, error) {
			return nil, errors.New("no repository should ever be opened in this test")
		},
		Catalog: cat,
		// Long enough that the interval timer never interferes with what the
		// test is measuring. The startup scan still fires immediately.
		Interval: time.Hour,
	}}, packages)
	if err != nil {
		t.Fatalf("start loop: %v", err)
	}

	return &collapseHarness{loop: loop}
}

func recvScan(t *testing.T, ch <-chan ScanResult) ScanResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("a trigger never returned")
		return ScanResult{}
	}
}

func newCollapseStore(t *testing.T) *store.Packages {
	t.Helper()

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "collapse.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.NewPackages(st)
}
