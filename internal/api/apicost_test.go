package api

import (
	"context"
	"fmt"
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
	d.Replication = &instantReplicator{}
}

// instantReplicator answers without a registry behind it.
//
// The sweep is about the SHAPE of what the Coordinator asks its own database,
// not about how long somebody else's registry takes - and a real one here
// would make the numbers depend on the network.
type instantReplicator struct{}

func (*instantReplicator) Status(
	_ context.Context, p *product.Product, t product.Target,
) (*replication.Status, error) {
	return &replication.Status{
		Product: p.Metadata.Name, Target: t.Name, Mode: t.ReplicationMode(),
	}, nil
}

func (*instantReplicator) Apply(context.Context, *product.Product, product.Target,
	replication.ApplyOptions) (*replication.ApplyResult, error) {
	return nil, nil //nolint:nilnil // unused by this sweep
}

func (*instantReplicator) Sync(context.Context, *product.Product, product.Target,
	string) (*replication.SyncOutcome, error) {
	return nil, nil //nolint:nilnil // unused by this sweep
}

func (*instantReplicator) CancelSync(context.Context, *product.Product, product.Target,
	string) (*replication.SyncOutcome, error) {
	return nil, nil //nolint:nilnil // unused by this sweep
}
