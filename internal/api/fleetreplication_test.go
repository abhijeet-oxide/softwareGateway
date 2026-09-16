package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A second product, so an estate-wide route has an estate to read.
const secondFourTargetDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-b
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: vendor-b/platform
      anonymous: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-b
      anonymous: true
      default: true
    - name: lab
      registry: lab.example.com
      repository: mirror/vendor-b
      anonymous: true
    - name: staging
      registry: staging.example.com
      repository: mirror/vendor-b
      anonymous: true
    - name: production
      registry: production.example.com
      repository: mirror/vendor-b
      anonymous: true
`

// THE WHOLE ESTATE IN ONE REQUEST, which is what the drift banner needs.
//
// The Downloads page draws a banner naming any registry whose configuration has
// drifted from what Git says. Drift is a property of the estate, and with only
// the per-product route to read it from, the page asked once PER PRODUCT - a
// deployment with thirty products issued thirty requests to draw one banner,
// every time somebody navigated back to the page.
func TestFleetReplicationReadsEveryProduct(t *testing.T) {
	rep := &slowReplicator{}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Replication = rep },
		fourTargetDoc, secondFourTargetDoc)

	var out v1.ListReplicationResponse
	if code := h.get("/api/v1/replication", &out); code != http.StatusOK {
		t.Fatalf("fleet listing = %d, want 200", code)
	}

	// Two products of four targets each.
	if len(out.Targets) != 8 {
		t.Fatalf("listed %d targets, want 8 - two products of four", len(out.Targets))
	}
	seen := map[string]int{}
	for _, v := range out.Targets {
		seen[v.Product]++
	}
	if seen["vendor-a"] != 4 || seen["vendor-b"] != 4 {
		t.Errorf("targets by product = %v, want four each of vendor-a and vendor-b", seen)
	}
}

// THE CONCURRENCY BOUND IS OVER THE WHOLE CALL, not over each product's share.
//
// This is the property that makes the estate-wide route safe to add. The
// per-product handler bounded its own fan-out at eight, which bounded nothing
// once one request reads every product: thirty products of four targets would
// have opened a hundred and twenty registry connections at once, from a single
// unauthenticated-looking GET.
//
// Eight products of four targets is thirty-two reads. If the limit still
// applied per product they would all overlap; with one limit over the call, no
// more than maxConcurrentFleetTargetReads can ever be in flight - which is
// wider than the per-product bound on purpose (see the constant) and still a
// bound.
func TestFleetReplicationBoundsTheWholeFanOut(t *testing.T) {
	docs := make([]string, 0, 8)
	docs = append(docs, fourTargetDoc)
	for i := range 7 {
		docs = append(docs,
			strings.ReplaceAll(secondFourTargetDoc, "vendor-b", "vendor-"+string(rune('c'+i))))
	}

	// A delay, so the reads genuinely overlap rather than finishing one at a
	// time faster than the next one starts.
	rep := &slowReplicator{delay: 20 * time.Millisecond}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Replication = rep }, docs...)

	var out v1.ListReplicationResponse
	if code := h.get("/api/v1/replication", &out); code != http.StatusOK {
		t.Fatalf("fleet listing = %d, want 200", code)
	}
	if len(out.Targets) != 32 {
		t.Fatalf("listed %d targets, want 32 - eight products of four", len(out.Targets))
	}
	if peak := rep.peak.Load(); peak > maxConcurrentFleetTargetReads {
		t.Errorf("%d registry reads were in flight at once, and the limit is %d - "+
			"the bound is being applied per product rather than over the call",
			peak, maxConcurrentFleetTargetReads)
	}
}

// One unreachable registry must not blank the rest of the estate.
//
// The same property the per-product route has, and it matters more here: a
// banner drawn from every product must not disappear because one registry in
// one product is down. The failure is carried in its own row.
func TestFleetReplicationSurvivesOneBadRegistry(t *testing.T) {
	rep := &slowReplicator{fail: map[string]bool{"lab": true}}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Replication = rep },
		fourTargetDoc, secondFourTargetDoc)

	var out v1.ListReplicationResponse
	if code := h.get("/api/v1/replication", &out); code != http.StatusOK {
		t.Fatalf("fleet listing = %d, want 200", code)
	}
	if len(out.Targets) != 8 {
		t.Fatalf("listed %d targets, want 8 even with a registry down", len(out.Targets))
	}

	var unreachable, readable int
	for _, v := range out.Targets {
		if v.Unreachable != "" {
			unreachable++
			if v.Target != "lab" {
				t.Errorf("target %s/%s is unreachable, and only `lab` should be",
					v.Product, v.Target)
			}
			continue
		}
		readable++
	}
	// One `lab` per product, and every other target still answered.
	if unreachable != 2 || readable != 6 {
		t.Errorf("%d unreachable and %d readable, want 2 and 6 - a failure in one "+
			"target is cancelling the reads of the others", unreachable, readable)
	}
}

// THE DRIFT BANNER MAY NOT HOLD THE PAGE, whatever a registry does.
//
// Every delegated target is a round trip to somebody else's registry, and one
// that accepts the connection and then never answers costs
// transport.DefaultRetryMaxElapsed - ninety seconds - before it gives up. Read
// behind a concurrency limit that is ninety seconds PER BATCH, and a
// deployment with one unresponsive registry watched the Downloads page sit for
// minutes.
//
// The per-product fan-out this route replaced hid that: the browser issued
// those requests in parallel, so the wall-clock was the slowest single target
// rather than the sum of the batches. Reading the estate in one request is
// right, and it has to carry the bound the browser used to provide.
//
// So the whole read is capped, and every row still comes back - a target that
// did not answer says so rather than the page waiting for it.
func TestTheDriftBannerIsBoundedWhenARegistryHangs(t *testing.T) {
	// Longer than the budget by a wide margin: this stands in for a registry
	// that accepts the connection and then says nothing.
	rep := &slowReplicator{delay: 10 * time.Minute}

	docs := make([]string, 0, 4)
	docs = append(docs, fourTargetDoc)
	for i := range 3 {
		docs = append(docs,
			strings.ReplaceAll(secondFourTargetDoc, "vendor-b", "vendor-"+string(rune('c'+i))))
	}
	h := newAPIHarnessWith(t, func(d *Deps) { d.Replication = rep }, docs...)

	start := time.Now()
	var out v1.ListReplicationResponse
	code := h.get("/api/v1/replication", &out)
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("fleet listing = %d, want 200 - a hanging registry must not "+
			"fail the banner either", code)
	}
	// Generous over the budget, because a loaded machine is not a stopwatch -
	// and still far below the ninety seconds one hanging target used to cost,
	// let alone the minutes a whole estate of them did.
	if limit := fleetReplicationBudget * 3; elapsed > limit {
		t.Errorf("the drift banner took %s against a registry that never answers; "+
			"the budget is %s", elapsed.Round(time.Millisecond), fleetReplicationBudget)
	}
	if len(out.Targets) != 16 {
		t.Errorf("listed %d targets, want 16 - every row must come back, "+
			"carrying its own reason", len(out.Targets))
	}
	for _, v := range out.Targets {
		if v.Unreachable == "" {
			t.Errorf("target %s/%s came back with no reason, against a registry "+
				"that never answered", v.Product, v.Target)
		}
	}
}
