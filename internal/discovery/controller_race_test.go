package discovery

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// TestConcurrentReconcilesStartTheLoopOnce reproduces the interleaving that
// produced "discovery: failed to start: discovery loop is already running" on
// every dev startup: SetLeader runs on the leader-election goroutine and
// SetConfig on the config watcher, and both call reconcile.
//
// The noisy log turned out to be the mild symptom. With reconcileMu removed
// this test does not fail - it HANGS, because concurrent Start and Stop on the
// loop wedge each other. Measured: 1.7s with the lock, still running after two
// minutes without it.
//
// Run with -race, which is how `task test` runs.
func TestConcurrentReconcilesStartTheLoopOnce(t *testing.T) {
	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "controller.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	logs := &countingHandler{}
	c := NewController(store.NewPackages(st), product.NewSecretResolver(t.TempDir()),
		vendors.NewRegistry(), slog.New(logs), metrics.New("test"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run holds the context the goroutines inherit; it blocks until cancel.
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()

	products := []*product.Product{{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec: product.Spec{Sources: []product.Source{{
			Name: "primary", Registry: "registry.example.com",
			Repository: "platform/suite", Anonymous: true,
		}}},
	}}
	cat := map[string]ProductRef{
		"vendor-a": {ID: 1, Repositories: map[string]int64{"platform/suite": 1}},
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); c.SetConfig(products, cat) }()
		wg.Add(1)
		go func() { defer wg.Done(); c.SetLeader(true) }()
	}
	wg.Wait()

	cancel()
	<-done

	if n := logs.errors(); n > 0 {
		t.Errorf("%d error log(s) during concurrent reconciles; the loop was started twice", n)
	}
}

// countingHandler counts ERROR records without printing them.
type countingHandler struct {
	mu sync.Mutex
	n  int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.mu.Lock()
		h.n++
		h.mu.Unlock()
	}
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) errors() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}
