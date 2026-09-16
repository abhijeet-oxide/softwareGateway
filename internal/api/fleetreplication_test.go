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
// applied per product they would all overlap; with one limit over the call,
// no more than maxConcurrentTargetReads can ever be in flight.
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
	if peak := rep.peak.Load(); peak > maxConcurrentTargetReads {
		t.Errorf("%d registry reads were in flight at once, and the limit is %d - "+
			"the bound is being applied per product rather than over the call",
			peak, maxConcurrentTargetReads)
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
