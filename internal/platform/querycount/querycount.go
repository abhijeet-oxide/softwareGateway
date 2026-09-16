// Package querycount counts the database round trips one request makes.
//
// # Why this exists
//
// Because latency cannot see an N+1 and this can.
//
// Two listings in this application asked the database once PER ROW of the page
// they were drawing. Both were invisible to every check the repository had:
// the query-plan test cannot see a query that is not part of the listing's
// query, the store benchmarks measured the half that was already fixed, and
// the load test's thresholds are an order of magnitude loose on purpose. On a
// developer's seeded estate twenty-five extra round trips cost forty
// milliseconds and nothing complained. On a real deployment, over a network,
// the same twenty-five cost minutes.
//
// THE DIFFERENCE IS THAT TIME IS DATA-DEPENDENT AND A COUNT IS NOT. Twenty-five
// queries to draw twenty-five rows is wrong on an empty database, wrong on a
// full one, and wrong on the fastest hardware anybody will ever run this on. A
// test that asserts the count is a test that fails for the mistake rather than
// for the weather - see internal/api/apicost_test.go, which holds every read
// endpoint to a number of queries that does not grow with the size of its page.
//
// # How it is counted
//
// The counter lives in the request's CONTEXT, not in a global, because that is
// what makes it per-request rather than per-process and what lets tests run in
// parallel without seeing each other's queries. The driver wrapper in
// internal/store increments whatever counter the context carries, and a context
// without one costs an interface comparison.
package querycount

import (
	"context"
	"sync/atomic"
)

type ctxKey struct{}

// Counter is the number of database round trips made under one context.
type Counter struct {
	queries atomic.Int64
}

// N is how many round trips have been made.
func (c *Counter) N() int64 {
	if c == nil {
		return 0
	}
	return c.queries.Load()
}

// add records one round trip. Safe on a nil counter, which is the case for
// every context nobody asked to count.
func (c *Counter) add() {
	if c == nil {
		return
	}
	c.queries.Add(1)
}

// With returns a context that counts, and the counter to read afterwards.
func With(ctx context.Context) (context.Context, *Counter) {
	c := &Counter{}
	return context.WithValue(ctx, ctxKey{}, c), c
}

// From returns the counter this context carries, or nil.
func From(ctx context.Context) *Counter {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(ctxKey{}).(*Counter)
	return c
}

// Record adds one round trip to whatever counter the context carries.
//
// Called by the driver wrapper on every statement that reaches the database.
// A context nobody is counting costs one type assertion and no allocation.
func Record(ctx context.Context) {
	From(ctx).add()
}
