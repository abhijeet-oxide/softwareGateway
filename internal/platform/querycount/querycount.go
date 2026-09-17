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
//
// # Why it also holds time
//
// Because "this endpoint is slow" has two answers and the count only
// distinguishes them when the count is obviously wrong. A route making one
// query and taking two seconds is a slow QUERY; a route making one query and
// taking two seconds of which one is in the database is a slow query AND a
// slow handler. Total latency alone cannot separate those, and the separation
// is most of the work of deciding what to fix.
//
// The time measured is the driver call - from handing the statement to the
// database to its first answer - which is the wait, not the row scanning that
// follows. That is deliberately the part a person cannot make faster by
// writing better Go.
package querycount

import (
	"context"
	"sync/atomic"
	"time"
)

type ctxKey struct{}

// Counter is the database work done under one context: how many round trips,
// and how long they waited.
type Counter struct {
	queries atomic.Int64
	nanos   atomic.Int64
}

// N is how many round trips have been made.
func (c *Counter) N() int64 {
	if c == nil {
		return 0
	}
	return c.queries.Load()
}

// Seconds is how long those round trips spent waiting on the database.
//
// Wall clock summed per statement, so on a handler that queries concurrently
// it can exceed the request's own duration. That is the honest reading -
// "database work done on behalf of this request" - and the alternative,
// measuring only the critical path, would report a fan-out that saturated the
// pool as cheap.
func (c *Counter) Seconds() float64 {
	if c == nil {
		return 0
	}
	return float64(c.nanos.Load()) / float64(time.Second)
}

// Record adds one round trip and what it cost. Safe on a nil counter, which is
// the case for every context nobody asked to count.
func (c *Counter) Record(d time.Duration) {
	if c == nil {
		return
	}
	c.queries.Add(1)
	c.nanos.Add(int64(d))
}

// With returns a context that counts, and the counter to read afterwards.
//
// IDEMPOTENT. A context that already carries a counter gets that same counter
// back, rather than a fresh one layered over it.
//
// This is not a nicety. The middleware installs a counter on every request, so
// a caller that installed its own first - which is exactly what a test
// measuring an endpoint's cost does - had it shadowed the moment the request
// entered the handler: the driver incremented the middleware's counter and the
// caller read its own, which nothing had touched.
//
// That made internal/api/apicost_test.go report zero round trips for every
// endpoint in the table. It passed, in full, for a listing making sixteen
// queries to draw eight rows - the precise failure the file exists to prevent.
// The rule that catches this class is below: a measurement that can silently
// become a constant has to be asserted against, not just recorded.
func With(ctx context.Context) (context.Context, *Counter) {
	if c := From(ctx); c != nil {
		return ctx, c
	}
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

// Observe times one statement against whatever counter the context carries.
//
// Called by the driver wrapper around every statement that reaches the
// database. A context nobody is counting takes the nil branch: one type
// assertion, no clock read, no allocation - which matters because this is the
// wrapper every query in the process passes through, including the ones served
// to a worker forty times a second.
func Observe[T any](ctx context.Context, call func() (T, error)) (T, error) {
	c := From(ctx)
	if c == nil {
		return call()
	}
	start := time.Now()
	out, err := call()
	c.Record(time.Since(start))
	return out, err
}
