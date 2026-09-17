package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/download"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/querycount"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/replication"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// WHAT EVERY READ ENDPOINT COSTS, IN ROUND TRIPS, AT TWO SIZES OF ESTATE.
//
// # Why this exists, and why it is not a benchmark
//
// This repository shipped two N+1s. Both were a store method called in a LOOP
// by a handler, each call perfectly reasonable and the loop the mistake. Both
// were invisible to everything we had: the query-plan test cannot see a query
// that is not part of the listing's query, the store benchmarks measured the
// half already fixed, and the load test's thresholds are an order of magnitude
// loose on purpose. On a seeded laptop estate twenty-five extra round trips
// cost forty milliseconds; on a real deployment, over a network, they cost
// minutes.
//
// THE DIFFERENCE IS THAT TIME IS DATA-DEPENDENT AND A COUNT IS NOT. So this
// asserts nothing about duration - durations are reported, for a human, and
// never gated. What it asserts is SHAPE:
//
//	the number of round trips an endpoint makes must not grow
//	with the number of rows it returns.
//
// That is true on an empty database, on a full one, on the fastest hardware
// anybody will ever run this on, and on a loaded CI runner. An endpoint that
// asks once per row fails it at any size, which is precisely what the two we
// shipped would have done on day one.
//
// # How to read a failure
//
// The table names the endpoint, what it cost at each size, and the growth. A
// growth of roughly one query per extra row is an N+1: find the loop in the
// handler and give the store a call that takes the whole page. A growth of a
// FIXED number of queries is usually a second page being fetched and is worth
// understanding but rarely urgent.
//
// # How to add an endpoint
//
// Put it in the table. An endpoint nobody lists here is an endpoint nobody is
// holding to a shape, which is how the last two got in.
func TestEveryReadEndpointCostsAFixedNumberOfQueries(t *testing.T) {
	// Two sizes, four times apart. Four rather than two because a handler that
	// fetches a constant SECOND page would look like growth at 1->2 and is
	// unmistakable at 1->4.
	const small, large = 3, 12

	smallCost := sweepReadEndpoints(t, small)
	largeCost := sweepReadEndpoints(t, large)

	names := make([]string, 0, len(smallCost))
	for name := range smallCost {
		names = append(names, name)
	}
	sort.Strings(names)

	t.Log("endpoint cost, in database round trips:")
	t.Logf("  %-56s %7s %7s %8s", "endpoint", fmt.Sprintf("n=%d", small),
		fmt.Sprintf("n=%d", large), "growth")

	for _, name := range names {
		a, b := smallCost[name], largeCost[name]
		// A key present in one sweep and not the other is the bug described in
		// sweepReadEndpoints, and it must be a failure rather than a zero.
		if !b.measured {
			t.Errorf("%s was measured at n=%d and not at n=%d, so its growth is "+
				"not a measurement", name, small, large)
			continue
		}
		growth := b.queries - a.queries
		t.Logf("  %-56s %7d %7d %+8d   (%s / %s)", name, a.queries, b.queries, growth,
			a.took.Round(time.Microsecond), b.took.Round(time.Microsecond))

		if growth > 0 {
			t.Errorf("%s makes %d more round trips for %d more rows - it is asking "+
				"the database once per row. Give the store a call that takes the "+
				"whole page, as TransferContentBytesFor does.",
				name, growth, large-small)
		}
	}

	assertTheSweepMeasuredSomething(t, largeCost)
}

// mustCostQueries are endpoints that CANNOT answer without reading the
// database, and are therefore the canaries for the measurement itself.
//
// Several rows in the table above are legitimately zero - /api/v1/products and
// /api/v1/discovery answer from the in-memory product registry - so "some
// endpoint costs nothing" proves nothing. These cannot: a transfer listing that
// reads no rows is not a cheap listing, it is a broken measurement.
var mustCostQueries = []string{
	"/api/v1/transfers",
	"/api/v1/packages",
	"/api/v1/replication",
	"/api/v1/auditEvents",
}

// assertTheSweepMeasuredSomething is the guard against a vacuous pass.
//
// # The failure it exists for
//
// This table read ZERO for every endpoint in it, for weeks, and passed every
// time. The counter is installed by the logging middleware on the way in, and
// it used to overwrite whatever the caller had already put in the context - so
// this test's counter was shadowed the instant the request reached the
// handler, the driver incremented the middleware's, and every assertion was
// comparing zero against zero.
//
// A growth assertion cannot notice that: zero does not grow. Nor can a
// threshold on any single endpoint, because a legitimate zero exists. What
// notices it is asserting that the endpoints which MUST touch the database
// were seen to touch it, which is a statement about the apparatus rather than
// about the code under test.
//
// The general rule, and the second time this file has needed it: a measurement
// that can silently collapse to a constant has to be asserted against.
func assertTheSweepMeasuredSomething(t *testing.T, cost map[string]endpointCost) {
	t.Helper()
	for _, name := range mustCostQueries {
		c, ok := cost[name]
		if !ok {
			t.Errorf("%s is not in the sweep at all, so nothing holds it to a shape", name)
			continue
		}
		if c.queries == 0 {
			t.Errorf("%s was measured at zero database round trips.\n\n"+
				"It cannot answer without reading the database, so this is the\n"+
				"measurement failing rather than the endpoint being cheap - and a\n"+
				"sweep that measures zero passes for every N+1 in the table above.\n"+
				"Check that querycount.With is still returning the counter this\n"+
				"test installed rather than one the middleware layered over it.",
				name)
		}
	}
}

type endpointCost struct {
	queries int64
	status  int
	took    time.Duration
	// measured distinguishes "this endpoint cost nothing" from "this endpoint
	// was never asked", which a zero value cannot.
	measured bool
}

// sweepReadEndpoints seeds an estate of `n` of everything and asks every read
// endpoint what it costs.
//
// DETERMINISTIC by construction: every seeded value is derived from its index,
// nothing is random, nothing reads the clock for content, and the estate is
// built the same way every run. Two runs at the same size cost the same.
func sweepReadEndpoints(t *testing.T, n int) map[string]endpointCost {
	t.Helper()

	h := newAPIHarnessWith(t, withEverythingRead, fourTargetDoc, secondFourTargetDoc)
	h.seedCostEstate(t, n)

	/*
	   Every endpoint the interface READS on the pages people leave open.

	   THE KEY IS THE TEMPLATE AND THE URL CARRIES THE PAGE SIZE, which is not
	   a detail: the page size has to grow with the estate for a whole page to
	   be measured, and an earlier version of this put the size in the key. The
	   two sweeps then had different keys, every lookup of the larger sweep
	   missed, and the growth was computed against a zero value - so the
	   assertion passed for every endpoint including an N+1. A test that cannot
	   fail is worse than no test, and the only reason this one was caught is
	   that the table it prints was read rather than trusted.
	*/
	size := n * 4 // a page larger than the estate, so one request sees all of it
	endpoints := []struct{ name, url string }{
		{"/api/v1/products", "/api/v1/products"},
		{"/api/v1/discovery", "/api/v1/discovery"},
		{"/api/v1/workers", "/api/v1/workers"},
		{"/api/v1/replication", "/api/v1/replication"},
		{"/api/v1/downloads", "/api/v1/downloads"},
		{"/api/v1/autoDownloadRules", "/api/v1/autoDownloadRules"},
		{"/api/v1/transfers", fmt.Sprintf("/api/v1/transfers?pageSize=%d", size)},
		{"/api/v1/transfers?view=summary",
			fmt.Sprintf("/api/v1/transfers?view=summary&pageSize=%d", size)},
		{"/api/v1/transfers:activity", "/api/v1/transfers:activity"},
		{"/api/v1/packages", fmt.Sprintf("/api/v1/packages?pageSize=%d", size)},
		{"/api/v1/products/{p}/packages",
			fmt.Sprintf("/api/v1/products/vendor-a/packages?pageSize=%d", size)},
		{"/api/v1/products/{p}/downloads", "/api/v1/products/vendor-a/downloads"},
		{"/api/v1/products/{p}/autoDownloadRules",
			"/api/v1/products/vendor-a/autoDownloadRules"},
		{"/api/v1/products/{p}/replication", "/api/v1/products/vendor-a/replication"},
		{"/api/v1/auditEvents", fmt.Sprintf("/api/v1/auditEvents?pageSize=%d", size)},
	}

	out := make(map[string]endpointCost, len(endpoints))
	for _, ep := range endpoints {
		out[ep.name] = h.cost(t, ep.url)
	}
	return out
}

// cost serves one request and reports what it asked the database for.
//
// The counter is installed on the REQUEST's context and the handler is served
// directly rather than over the loopback, so what is counted is this request's
// own round trips and nothing else - which is what lets the sweep run in
// parallel with the rest of the suite.
func (h *apiHarness) cost(t *testing.T, target string) endpointCost {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx, counted := querycount.With(req.Context())
	rec := httptest.NewRecorder()

	start := time.Now()
	h.api.Handler().ServeHTTP(rec, req.WithContext(ctx))
	took := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Errorf("%s answered %d, so its cost below means nothing: %s",
			target, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return endpointCost{queries: counted.N(), status: rec.Code, took: took, measured: true}
}

// seedCostEstate builds an estate of n of everything, deterministically.
func (h *apiHarness) seedCostEstate(t *testing.T, n int) {
	t.Helper()

	for i := range n {
		pkgID := h.seedPackage(fmt.Sprintf("v1.%d", i),
			fmt.Sprintf("sha256:%064x", i))
		h.seedArtifact(pkgID, fmt.Sprintf("sha256:%064x", 1000+i),
			"application/vnd.oci.image.manifest.v1+json")

		id := fmt.Sprintf("%08d-cccc-dddd-eeee-ffffffffffff", i)
		h.exec(`INSERT INTO transfer_requests (id, product_id, package_id, operation,
		                                       source_repo_id, idempotency_key)
		         VALUES (?, ?, ?, 'replicate', ?, ?)`,
			"req-"+id, h.productID, pkgID, h.repoID, "key-"+id)
		h.exec(`INSERT INTO transfers (id, request_id, package_id, source_repo_id,
		                               target_repo_id, state, priority, created_at)
		         VALUES (?, ?, ?, ?, ?, 'running', 50, ?)`,
			id, "req-"+id, pkgID, h.repoID, h.repoID,
			fmt.Sprintf("2026-01-01T%02d:00:00Z", i%24))

		// Several jobs each, so a per-row rollup has something to be slow over.
		for j := range 4 {
			h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes,
			                          source_repo_id, target_repo_id, state, wave,
			                          attempts, max_attempts, bytes_transferred)
			         VALUES (?, 'blob', ?, 1024, ?, ?, ?, 0, 0, 8, 512)`,
				id, fmt.Sprintf("sha256:%064x", i*10+j), h.repoID, h.repoID,
				[]string{"succeeded", "pending", "failed", "leased"}[j])
		}
	}
}

// withEverythingRead wires every dependency the read endpoints need.
//
// Without this the routes that need them are not registered at all and the
// sweep measures a 404, which costs no queries and would pass silently - an
// endpoint absent from the table is an endpoint nobody is holding to a shape.
// The sweep asserts the status is 200 for exactly that reason.
func withEverythingRead(d *Deps) {
	d.Downloads = download.NewService(d.Packages, nil, nil)
	// Only to register the worker-plane routes; /workers reads the store.
	d.Queue = &fakeQueue{}

	// THE REAL REPLICATION SERVICE, OVER THE REAL STORE.
	//
	// This was a fake that returned a status without touching the database,
	// and it is the reason this sweep reported zero queries for
	// /api/v1/replication while a running deployment was measuring twenty per
	// request. The endpoint was in the table, the assertion passed, and the
	// number it was asserting about was a fake's.
	//
	// A fake is the right call for the REGISTRY - the sweep is about the shape
	// of what the Coordinator asks its own database, and a real registry would
	// make the numbers depend on the network - but it was never the right call
	// for the database reads. It does not need to be either: the targets in
	// these documents are copy targets, so Status answers from the store and
	// returns before any registry is contacted.
	//
	// The general lesson, which cost two N+1s in production: a cost test whose
	// subject is a fake measures the fake.
	d.Replication = replication.NewService(
		replication.NewResolver(product.NewSecretResolver(""), slog.New(slog.DiscardHandler), "test"),
		store.NewReplication(d.Store),
		slog.New(slog.DiscardHandler),
	)
}

// THE SECOND DIMENSION, and the one that was missing.
//
// The sweep above varies the size of the ESTATE - packages, transfers, jobs -
// because that is where the two N+1s this repository shipped lived. It cannot
// see a handler whose cost grows with the CONFIGURATION instead, and
// /api/v1/replication is exactly that: it reads every target of every visible
// product, so its query count tracks the target count in the product documents
// and does not move at all when the estate grows.
//
// It shipped making sixteen round trips for eight targets - the product id
// resolved once per target, the applied record read once per target, and the
// same id resolved a second time to write the observation down. A running
// deployment reported 20.6 on the api_request_queries histogram. This sweep
// reported nothing, because the Replicator it measured was a fake with no
// database behind it.
//
// Both halves are fixed: withEverythingRead now builds the real service over
// the real store, and this test varies the dimension that endpoint actually
// scales on.
func TestReplicationCostDoesNotGrowWithTargetCount(t *testing.T) {
	const few, many = 2, 8

	cost := func(targets int) endpointCost {
		h := newAPIHarnessWith(t, withEverythingRead, targetCountDoc("vendor-a", targets))
		return h.cost(t, "/api/v1/replication")
	}

	small, large := cost(few), cost(many)
	t.Logf("replication listing: %d targets = %d round trips, %d targets = %d",
		few, small.queries, many, large.queries)

	if large.queries > small.queries {
		t.Errorf("/api/v1/replication makes %d round trips for %d targets and %d "+
			"for %d - it is asking the database once per target.\n\n"+
			"Every target of one product resolves the same product id and reads "+
			"from the same table. Prefetch them: replication.Service.Snapshot "+
			"reads the lot in two queries, and StatusWith takes the result.",
			large.queries, many, small.queries, few)
	}
}

// targetCountDoc is one product with n copy targets.
//
// Copy targets rather than delegated ones, so Status answers from the store and
// returns before any registry is contacted - the sweep is about round trips to
// our own database, and a delegated target would put somebody else's network in
// the measurement.
func targetCountDoc(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: %s
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: %s/platform
      anonymous: true
  targets:
`, name, name)
	for i := range n {
		fmt.Fprintf(&b, `    - name: target-%d
      registry: target-%d.example.com
      repository: mirror/%s
      anonymous: true
`, i, i, name)
		if i == 0 {
			b.WriteString("      default: true\n")
		}
	}
	return b.String()
}
