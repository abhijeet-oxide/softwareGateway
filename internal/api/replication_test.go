package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/replication"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A product with four targets, because one target cannot show a fan-out.
const fourTargetDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: vendor-a/platform
      anonymous: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      anonymous: true
      default: true
    - name: lab
      registry: lab.example.com
      repository: mirror/vendor-a
      anonymous: true
    - name: staging
      registry: staging.example.com
      repository: mirror/vendor-a
      anonymous: true
    - name: production
      registry: production.example.com
      repository: mirror/vendor-a
      anonymous: true
`

// slowReplicator stands in for four registries, each answering after a delay.
type slowReplicator struct {
	delay time.Duration
	// peak is the most reads that were ever in flight at once, which is the
	// only direct evidence that they overlapped.
	inFlight atomic.Int64
	peak     atomic.Int64
	fail     map[string]bool
}

func (s *slowReplicator) Status(
	ctx context.Context, p *product.Product, t product.Target,
) (*replication.Status, error) {
	n := s.inFlight.Add(1)
	for {
		peak := s.peak.Load()
		if n <= peak || s.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	defer s.inFlight.Add(-1)

	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.fail[t.Name] {
		return nil, context.DeadlineExceeded
	}
	return &replication.Status{
		Product: p.Metadata.Name, Target: t.Name, Mode: t.ReplicationMode(),
	}, nil
}

func (s *slowReplicator) Apply(context.Context, *product.Product, product.Target,
	replication.ApplyOptions) (*replication.ApplyResult, error) {
	return nil, nil //nolint:nilnil // unused by these tests
}

func (s *slowReplicator) Sync(context.Context, *product.Product, product.Target,
	string) (*replication.SyncOutcome, error) {
	return nil, nil //nolint:nilnil // unused by these tests
}

func (s *slowReplicator) CancelSync(context.Context, *product.Product, product.Target,
	string) (*replication.SyncOutcome, error) {
	return nil, nil //nolint:nilnil // unused by these tests
}

// EACH TARGET IS ITS OWN REGISTRY, so they are read at the same time.
//
// This listing is asked of every enabled product at once, on every visit to
// the Downloads page, to draw its drift banner. Read one target after another,
// a four-target product spent four registry latencies before answering, and the
// page spent the slowest registry in the estate.
func TestTargetsAreReadConcurrently(t *testing.T) {
	const delay = 150 * time.Millisecond
	rep := &slowReplicator{delay: delay}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Replication = rep }, fourTargetDoc)

	start := time.Now()
	var out v1.ListReplicationResponse
	if code := h.get("/api/v1/products/vendor-a/replication", &out); code != http.StatusOK {
		t.Fatalf("listing = %d, want 200", code)
	}
	elapsed := time.Since(start)

	if len(out.Targets) != 4 {
		t.Fatalf("listed %d targets, want 4", len(out.Targets))
	}
	if got := rep.peak.Load(); got < 4 {
		t.Errorf("at most %d reads overlapped, want all 4 - they are being "+
			"read one after another", got)
	}
	// Generous, because a loaded CI machine is not a stopwatch: serially this
	// is 600ms and concurrently it is 150ms, so anything under half a second
	// can only be the concurrent path.
	if elapsed > delay*3 {
		t.Errorf("the listing took %s for 4 targets of %s each - that is serial",
			elapsed.Round(time.Millisecond), delay)
	}
}

// One unreachable registry must not cancel the reads of the others, and must
// not move the rows.
//
// The fan-out runs under an errgroup, whose context is cancelled the moment any
// goroutine returns an error - so a per-target failure reported that way would
// abort the targets still in flight and blank rows that were perfectly
// readable. The order is the configuration's order, written by index, so a slow
// registry does not reshuffle a listing somebody is comparing against the last
// one.
func TestOneUnreachableTargetDoesNotDisturbTheOthers(t *testing.T) {
	rep := &slowReplicator{delay: time.Millisecond, fail: map[string]bool{"lab": true}}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Replication = rep }, fourTargetDoc)

	var out v1.ListReplicationResponse
	if code := h.get("/api/v1/products/vendor-a/replication", &out); code != http.StatusOK {
		t.Fatalf("listing = %d, want 200", code)
	}

	want := []string{"internal", "lab", "staging", "production"}
	if len(out.Targets) != len(want) {
		t.Fatalf("listed %d targets, want %d", len(out.Targets), len(want))
	}
	for i, name := range want {
		if out.Targets[i].Target != name {
			t.Fatalf("target %d is %q, want %q - the rows are in the order the "+
				"registries answered rather than the order they are configured",
				i, out.Targets[i].Target, name)
		}
	}
	if out.Targets[1].Unreachable == "" {
		t.Error("the failing target came back without a reason")
	}
	for i, tr := range out.Targets {
		if i == 1 {
			continue
		}
		if tr.Unreachable != "" {
			t.Errorf("%s was reported unreachable (%q) because a DIFFERENT "+
				"target failed", tr.Target, tr.Unreachable)
		}
	}
}
