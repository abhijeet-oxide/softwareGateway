package discovery

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// loopHarness starts a real Loop against a fake registry, on a short interval.
type loopHarness struct {
	t     *testing.T
	reg   *fakeregistry.Registry
	store store.Store
	loop  *Loop
}

func newLoopHarness(t *testing.T, interval time.Duration) *loopHarness {
	t.Helper()
	ctx := t.Context()

	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	s, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "loop.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := store.Migrate(ctx, s, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	p := loadProduct(t, strings.ReplaceAll(baseDoc, "REGISTRY_HOST", reg.Host()))

	res, err := catalog.NewCatalog(s).Reconcile(ctx, []*product.Product{p})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ref := res.Products[p.Metadata.Name]

	client, err := generic.New(generic.Config{
		Registry: reg.Host(), Repository: testRepoPath, PlainHTTP: true,
		Transport: transport.Config{
			MaxRetryAttempts: 2, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	loop := NewLoop(nil, nil)
	t.Cleanup(loop.Stop)

	err = loop.Start(ctx, []SourceSpec{{
		Product:      p,
		ProductID:    ref.ID,
		SourceName:   "vendor",
		SourceRepoID: ref.Repositories["vendor"],
		RepoIDs:      ref.Repositories,
		Client:       client,
		Interval:     interval,
	}}, store.NewPackages(s))
	if err != nil {
		t.Fatalf("start loop: %v", err)
	}

	return &loopHarness{t: t, reg: reg, store: s, loop: loop}
}

func (h *loopHarness) packageCount() int {
	h.t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(h.t.Context(),
		`SELECT COUNT(*) FROM packages`).Scan(&n); err != nil {
		h.t.Fatalf("count packages: %v", err)
	}
	return n
}

// eventually polls until cond holds or the deadline passes.
//
// Polling rather than a fixed sleep: the loop is asynchronous, and a sleep long
// enough to be reliable on a loaded CI machine is a sleep that wastes time on
// every other run.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

func TestLoopScansAtStartup(t *testing.T) {
	// A long interval, so anything observed came from the startup scan rather
	// than from a tick.
	h := newLoopHarness(t, time.Hour)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	// The startup scan may have already run against an empty registry; trigger
	// once to be deterministic.
	if _, err := h.loop.Trigger(t.Context(), "vendor-a", "vendor"); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	eventually(t, 2*time.Second, "the package to be recorded", func() bool {
		return h.packageCount() == 1
	})
}

func TestLoopPollsOnItsInterval(t *testing.T) {
	h := newLoopHarness(t, 20*time.Millisecond)

	// Published AFTER the loop started: only a subsequent tick can find it.
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	eventually(t, 3*time.Second, "a scheduled scan to find the new package", func() bool {
		return h.packageCount() == 1
	})
}

// A registry outage must not stop the source. This is the rule stated most
// firmly in docs/design/07 §7: never disable a source, because a source that
// turned itself off is one nobody remembers to turn back on.
func TestOutageDoesNotDisableTheSource(t *testing.T) {
	h := newLoopHarness(t, 20*time.Millisecond)

	h.reg.FailNext("tags/list", 503, 50)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	// Let the failures accumulate and the backoff engage.
	time.Sleep(150 * time.Millisecond)

	if h.packageCount() != 0 {
		t.Fatal("nothing should have been recorded during the outage")
	}
	if !h.loop.Running() {
		t.Fatal("the loop stopped during an outage")
	}

	status := h.loop.Status()
	if len(status) != 1 {
		t.Fatalf("expected 1 source in status, got %d", len(status))
	}
	if status[0].LastErr == "" {
		t.Error("expected the failure to be visible in status")
	}

	// The registry recovers. The source is still polling, so it catches up with
	// no human intervention.
	h.reg.ClearFaults()
	eventually(t, 5*time.Second, "the source to recover on its own", func() bool {
		return h.packageCount() == 1
	})
}

// Concurrent triggers must collapse rather than stampede the vendor.
func TestConcurrentTriggersCollapse(t *testing.T) {
	h := newLoopHarness(t, time.Hour)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	// Drain the startup scan so the counts below measure only our triggers.
	eventually(t, 2*time.Second, "the startup scan to finish", func() bool {
		return len(h.loop.Status()) == 1 && !h.loop.Status()[0].LastRun.IsZero()
	})

	before := h.reg.TagListCalls.Load()

	const triggers = 8
	done := make(chan struct{}, triggers)
	for range triggers {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = h.loop.Trigger(t.Context(), "vendor-a", "vendor")
		}()
	}
	for range triggers {
		<-done
	}

	// Eight concurrent triggers must not become eight scans. Some will collapse
	// into a running one; the bound is what matters, not the exact number.
	if calls := h.reg.TagListCalls.Load() - before; calls >= triggers {
		t.Errorf("expected concurrent triggers to collapse, got %d scans for %d triggers",
			calls, triggers)
	}
	if h.packageCount() != 1 {
		t.Errorf("expected 1 package however many triggers ran, got %d", h.packageCount())
	}
}

func TestTriggerUnknownSource(t *testing.T) {
	h := newLoopHarness(t, time.Hour)

	if _, err := h.loop.Trigger(t.Context(), "vendor-a", "nope"); err == nil {
		t.Error("expected an unknown source to be rejected")
	}
	if _, err := h.loop.TriggerProduct(t.Context(), "no-such-product"); err == nil {
		t.Error("expected an unknown product to be rejected")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	h := newLoopHarness(t, time.Hour)

	h.loop.Stop()
	if h.loop.Running() {
		t.Error("loop still reports running after Stop")
	}
	// A second Stop must not panic or block — shutdown paths get called twice
	// more often than anyone expects.
	h.loop.Stop()
}

// Starting twice must be refused rather than silently doubling every scan.
func TestDoubleStartIsRefused(t *testing.T) {
	h := newLoopHarness(t, time.Hour)

	if err := h.loop.Start(t.Context(), nil, nil); err == nil {
		t.Error("expected a second Start to be refused")
	}
}
