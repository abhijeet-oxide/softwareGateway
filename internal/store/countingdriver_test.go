package store

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/querycount"
)

// THE COUNTER COUNTS WHAT THE DATABASE ACTUALLY SAW.
//
// Everything downstream rests on this: internal/api/apicost_test.go asserts
// that an endpoint's query count does not grow with its page, and a counter
// that undercounts turns that assertion into a rubber stamp.
//
// The case worth naming is the PREPARED one. database/sql prepares statements
// and reuses them on its own, so a wrapper that only counted
// Conn.QueryContext would miss most of a busy pool's traffic and report an
// N+1 as a single query - which is exactly the failure this whole mechanism
// exists to catch.
func TestTheQueryCounterCountsEveryRoundTrip(t *testing.T) {
	st := openTestStore(t)

	ctx, counted := querycount.With(t.Context())

	const queries = 5
	for range queries {
		var n int
		if err := st.DB().QueryRowContext(ctx, `SELECT 1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if got := counted.N(); got != queries {
		t.Errorf("counted %d round trips for %d queries", got, queries)
	}

	// A PREPARED statement executed several times is several round trips.
	stmt, err := st.DB().PrepareContext(ctx, `SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()

	before := counted.N()
	const executions = 3
	for range executions {
		var n int
		if err := stmt.QueryRowContext(ctx).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if got := counted.N() - before; got != executions {
		t.Errorf("counted %d round trips for %d executions of one prepared "+
			"statement - the wrapper is counting connections rather than "+
			"statements, so an N+1 through a prepared statement reads as one query",
			got, executions)
	}
}

// A context nobody is counting is not counted, and does not panic.
//
// This is every query the application makes outside a request, which is most
// of them: the schedulers, the reapers, the sweepers.
func TestAnUncountedContextIsFree(t *testing.T) {
	st := openTestStore(t)

	var n int
	if err := st.DB().QueryRowContext(t.Context(), `SELECT 1`).Scan(&n); err != nil {
		t.Fatalf("a query on an uncounted context failed: %v", err)
	}
	if c := querycount.From(t.Context()); c != nil {
		t.Errorf("an uncounted context carried a counter: %v", c.N())
	}
}

// Two contexts count their own queries and not each other's, which is what
// makes the API cost test safe to run in parallel.
func TestCountersDoNotSeeEachOther(t *testing.T) {
	st := openTestStore(t)

	ctxA, a := querycount.With(t.Context())
	ctxB, b := querycount.With(t.Context())

	var n int
	for range 3 {
		if err := st.DB().QueryRowContext(ctxA, `SELECT 1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB().QueryRowContext(ctxB, `SELECT 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	if a.N() != 3 || b.N() != 1 {
		t.Errorf("counters interfered: a=%d (want 3), b=%d (want 1)", a.N(), b.N())
	}
}
