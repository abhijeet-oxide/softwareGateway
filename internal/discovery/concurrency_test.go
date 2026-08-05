package discovery

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// gate records the highest number of callers inside it at once.
//
// The only honest way to test concurrency: count what actually overlapped,
// rather than asserting that a wall-clock measurement came in under some
// threshold, which is how a flaky test gets written.
type gate struct {
	mu      sync.Mutex
	current int
	peak    int
	// hold blocks each caller until enough have arrived, so the peak is a
	// property of the code rather than of the scheduler's mood.
	hold  chan struct{}
	once  sync.Once
	want  int
	total int
}

func newGate(want int) *gate {
	return &gate{hold: make(chan struct{}), want: want}
}

func (g *gate) enter(ctx context.Context) {
	g.mu.Lock()
	g.current++
	g.total++
	if g.current > g.peak {
		g.peak = g.current
	}
	reached := g.current >= g.want
	g.mu.Unlock()

	if reached {
		// Enough callers are inside simultaneously; release everyone.
		g.once.Do(func() { close(g.hold) })
	}

	select {
	case <-g.hold:
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		// Sequential code never reaches `want`, so this is the timeout that
		// turns a hang into a readable failure.
		g.once.Do(func() { close(g.hold) })
	}

	g.mu.Lock()
	g.current--
	g.mu.Unlock()
}

func (g *gate) stats() (peak, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak, g.total
}

// gatedSource is a registry.Source whose ListTags passes through a gate.
type gatedSource struct {
	registry.Source
	path string
	g    *gate
	tags []string
}

func (s *gatedSource) Name() string     { return "registry.example.com/" + s.path }
func (s *gatedSource) Registry() string { return "registry.example.com" }
func (s *gatedSource) Path() string     { return s.path }

func (s *gatedSource) Ping(context.Context) error { return nil }

func (s *gatedSource) Capabilities(context.Context) registry.Capabilities {
	return registry.DefaultCapabilities()
}

func (s *gatedSource) ListTags(ctx context.Context, _ string, _ int) ([]string, string, error) {
	s.g.enter(ctx)
	return s.tags, "", nil
}

func (s *gatedSource) ResolveTag(ctx context.Context, tag string) (registry.Descriptor, error) {
	s.g.enter(ctx)
	return registry.Descriptor{}, fmt.Errorf("resolve %s: not implemented in this test", tag)
}

// TestRepositoriesAreListedConcurrently pins the fix for the reported symptom:
// 48 repositories, and after two minutes the scan had not finished the first
// tag of the first repository, because everything ran one at a time.
func TestRepositoriesAreListedConcurrently(t *testing.T) {
	const repos = 12
	const perRegistry = 6

	g := newGate(perRegistry)
	paths := make([]string, repos)
	for i := range paths {
		paths[i] = fmt.Sprintf("orbs/component-%02d", i)
	}

	s := newGatedScanner(t, paths, perRegistry, func(path string) registry.Source {
		return &gatedSource{path: path, g: g}
	})

	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Repositories != repos {
		t.Errorf("scanned %d repositories, want %d", res.Repositories, repos)
	}

	peak, total := g.stats()
	if total != repos {
		t.Errorf("the gate saw %d callers, want one per repository (%d)", total, repos)
	}
	if peak < perRegistry {
		t.Errorf("peak concurrency was %d, want at least %d — repositories are still "+
			"being listed one at a time", peak, perRegistry)
	}
}

// TestTagsAreResolvedConcurrentlyAcrossRepositories covers the property the
// two-phase scan bought.
//
// Tags used to be bounded PER REPOSITORY, so a repository with three tags could
// only ever use three of the budget while the repository with three hundred
// waited its turn behind it. They now share one pool, which this pins: the gate
// wants more callers inside it at once than any single repository can supply,
// so it can only be reached by tags from different repositories overlapping.
func TestTagsAreResolvedConcurrentlyAcrossRepositories(t *testing.T) {
	const repos = 8
	const tagsEach = 2
	const perRegistry = repos * tagsEach // unreachable within one repository

	tags := make([]string, tagsEach)
	for i := range tags {
		tags[i] = fmt.Sprintf("orb_1.0.%d", i)
	}
	paths := make([]string, repos)
	for i := range paths {
		paths[i] = fmt.Sprintf("orbs/component-%02d", i)
	}

	resolveGate := newGate(perRegistry)
	// Listing is ungated: only ResolveTag contends, so the peak this measures is
	// unambiguously the resolve phase.
	listGate := newGate(1)

	s := newGatedScanner(t, paths, perRegistry, func(path string) registry.Source {
		return &splitGateSource{path: path, list: listGate, resolve: resolveGate, tags: tags}
	})

	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	peak, total := resolveGate.stats()
	if want := repos * tagsEach; total != want {
		t.Errorf("the gate saw %d resolves, want one per tag (%d)", total, want)
	}
	if peak <= tagsEach {
		t.Errorf("peak tag concurrency was %d, which one repository alone could reach "+
			"(%d tags each) — tags are still bounded per repository rather than "+
			"sharing the source's budget", peak, tagsEach)
	}
}

// TestConcurrencyIsBoundedByPerRegistry pins the other half: the limit is a
// limit, not a suggestion. Without a bound the scan would open one connection
// per tag against someone else's registry.
func TestConcurrencyIsBoundedByPerRegistry(t *testing.T) {
	const repos = 10
	const tagsEach = 10
	const perRegistry = 4

	tags := make([]string, tagsEach)
	for i := range tags {
		tags[i] = fmt.Sprintf("orb_1.0.%d", i)
	}
	paths := make([]string, repos)
	for i := range paths {
		paths[i] = fmt.Sprintf("orbs/component-%02d", i)
	}

	// want is set to the bound, so the gate releases as soon as the bound is
	// reached rather than deadlocking against a ceiling it can never exceed.
	resolveGate := newGate(perRegistry)
	listGate := newGate(1)

	s := newGatedScanner(t, paths, perRegistry, func(path string) registry.Source {
		return &splitGateSource{path: path, list: listGate, resolve: resolveGate, tags: tags}
	})

	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	peak, _ := resolveGate.stats()
	if peak > perRegistry {
		t.Errorf("peak concurrency was %d, above the configured perRegistry of %d",
			peak, perRegistry)
	}
}

// splitGateSource gates listing and resolving separately.
type splitGateSource struct {
	registry.Source
	path          string
	list, resolve *gate
	tags          []string
}

func (s *splitGateSource) Name() string     { return "registry.example.com/" + s.path }
func (s *splitGateSource) Registry() string { return "registry.example.com" }
func (s *splitGateSource) Path() string     { return s.path }

func (s *splitGateSource) Ping(context.Context) error { return nil }

func (s *splitGateSource) Capabilities(context.Context) registry.Capabilities {
	return registry.DefaultCapabilities()
}

func (s *splitGateSource) ListTags(ctx context.Context, _ string, _ int) ([]string, string, error) {
	s.list.enter(ctx)
	return s.tags, "", nil
}

func (s *splitGateSource) ResolveTag(ctx context.Context, tag string) (registry.Descriptor, error) {
	s.resolve.enter(ctx)
	return registry.Descriptor{}, fmt.Errorf("resolve %s: not implemented in this test", tag)
}

// staticCatalog returns a fixed repository list.
type staticCatalog struct{ repos []string }

func (c staticCatalog) ListAllRepositories(context.Context, int) ([]string, error) {
	return c.repos, nil
}

func newGatedScanner(
	t *testing.T, repos []string, perRegistry int, newSource func(string) registry.Source,
) *Scanner {
	t.Helper()

	p := &product.Product{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec: product.Spec{Sources: []product.Source{{
			Name: "primary", Registry: "registry.example.com", Anonymous: true,
			Concurrency: product.Concurrency{PerRegistry: perRegistry},
		}}},
	}

	// Real catalog rows: `repositories` has a NOT NULL product_id, so a scanner
	// pointed at an unreconciled product fails on the foreign key before it
	// reaches the registry at all.
	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "concurrency.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rec, err := catalog.NewCatalog(st).Reconcile(t.Context(), []*product.Product{p})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	s, err := NewScanner(ScannerConfig{
		Packages:   store.NewPackages(st),
		Product:    p,
		ProductID:  rec.Products[p.Metadata.Name].ID,
		SourceName: "primary",
		Catalog:    staticCatalog{repos: repos},
		RepoIDs:    map[string]int64{},
		NewClient: func(path string) (registry.Source, error) {
			return newSource(path), nil
		},
	})
	if err != nil {
		t.Fatalf("new scanner: %v", err)
	}
	return s
}
