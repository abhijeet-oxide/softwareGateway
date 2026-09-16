package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// What one page of the transfer listing costs as the estate grows.
//
// # The regression this exists to catch
//
// The listing the Downloads page polls projects a dozen aggregates over
// `jobs`, one correlated subquery each, evaluated per row of the page. The
// cost of a page is therefore the cost of reading the JOBS of the transfers on
// it - not the size of the page - and it grows with how much work the estate
// has done rather than with what the reader asked for.
//
// That is invisible on a developer's empty database and on every test in this
// package that seeds a handful of rows. It is what an operator means when they
// say the Downloads page takes seconds to open.
//
// # Why a benchmark rather than a test with a threshold
//
// transferperf_test.go explains why the durations there are not asserted: they
// are wall-clock numbers on whatever machine is running, and a bound tight
// enough to catch a real regression fails on a loaded CI box. A benchmark says
// the same thing without that problem - `go test -bench` records ns/op and
// B/op, and `benchstat` compares two runs and reports whether the difference
// is significant. That is the standard Go answer to "did this change make it
// slower", and it needs nothing installed to produce the numbers.
//
// Run it:
//
//	go test ./internal/store -run '^$' -bench BenchmarkTransferListing -benchmem
//
// Compare a change against main with golang.org/x/perf/cmd/benchstat.
//
// # Reading the result
//
// The two sub-benchmarks are the SAME page from the same data. `rollups`
// is what the Downloads page asks for; `summary` is `view=summary`, which the
// Overview uses and which skips the aggregates. The gap between them is the
// whole cost of the job counts, and it is the number worth watching: it is
// small on an empty estate and enormous on a real one.
func BenchmarkTransferListing(b *testing.B) {
	// Sized to a working estate rather than a maximum: sixty transfers of two
	// hundred and fifty jobs is fifteen thousand job rows, which a deployment
	// passes in a day or so, and it runs in a second here. The shape of the
	// cost is already visible at this size; making it larger measures the same
	// thing more slowly.
	const (
		transfers = 60
		jobsEach  = 250
		pageSize  = 25
	)

	st := openBenchStore(b)
	p := NewPackages(st)
	seedBenchEstate(b, st, transfers, jobsEach)

	for _, tc := range []struct {
		name             string
		withoutJobCounts bool
	}{
		{"rollups", false}, // the Downloads page
		{"summary", true},  // view=summary, the Overview
	} {
		b.Run(tc.name, func(b *testing.B) {
			filter := ListTransfersFilter{Limit: pageSize, WithoutJobCounts: tc.withoutJobCounts}
			b.ReportAllocs()
			for b.Loop() {
				rows, err := p.ListTransfers(b.Context(), filter)
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) != pageSize {
					b.Fatalf("got %d rows, want %d - the estate did not seed", len(rows), pageSize)
				}
			}
		})
	}
}

// openBenchStore is openTestStore for a benchmark.
//
// Separate rather than widening that one to testing.TB, because it and the
// schema template it reads are shared by every test in this package and a
// benchmark is not worth that churn.
func openBenchStore(b *testing.B) Store {
	b.Helper()

	ctx := context.Background()
	path := filepath.Join(b.TempDir(), "bench.db")
	st, err := Open(ctx, Config{Driver: DriverSQLite, DSN: path})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	if err := Migrate(ctx, st, nil); err != nil {
		_ = st.Close()
		b.Fatalf("migrate: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })
	return st
}

// seedBenchEstate writes the estate in ONE transaction, batched.
//
// The batching is not an optimisation of the benchmark - it is what keeps the
// seed off the measurement. See seedJobs in transferperf_test.go for the same
// reasoning and the same reason it must not call a store method: the SQLite
// pool is a single connection, so a BeginTx inside this one never returns.
func seedBenchEstate(b *testing.B, st Store, transfers, jobsEach int) {
	b.Helper()

	ctx := context.Background()
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	exec := func(q string, args ...any) {
		b.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			b.Fatalf("seed %.40s: %v", q, err)
		}
	}

	var productID, srcRepo, dstRepo, pkgID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('p','h','{}') RETURNING id`,
	).Scan(&productID); err != nil {
		b.Fatal(err)
	}
	for i, out := range []*int64{&srcRepo, &dstRepo} {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO repositories (product_id, name, role, registry_host, repository_path, registry_type, managed_by)
			      VALUES (?, ?, ?, 'registry.example.com', ?, 'generic', 'config') RETURNING id`,
			productID, fmt.Sprintf("r%d", i), []string{"source", "target"}[i],
			fmt.Sprintf("vendor/suite%d", i),
		).Scan(out); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO packages (product_id, source_repo_id, tag, manifest_digest, media_type, state)
		      VALUES (?, ?, 'v1', ?, 'application/json', 'verified') RETURNING id`,
		productID, srcRepo, "sha256:"+strings.Repeat("a", 64),
	).Scan(&pkgID); err != nil {
		b.Fatal(err)
	}

	// Rows per statement, for the reason seedJobs gives: a statement per row
	// through the pure-Go driver is slow enough to dominate the seed.
	const perStatement = 500
	var (
		values []string
		args   []any
	)
	flush := func() {
		if len(values) == 0 {
			return
		}
		exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
		                        target_repo_id, state, wave, bytes_transferred)
		      VALUES `+strings.Join(values, ","), args...)
		values, args = values[:0], args[:0]
	}

	states := []string{"succeeded", "pending", "failed", "leased"}
	for i := range transfers {
		id := fmt.Sprintf("%08d-aaaa-bbbb-cccc-dddddddddddd", i)
		requestID := fmt.Sprintf("%08d-eeee-ffff-0000-111111111111", i)

		exec(`INSERT INTO transfer_requests (id, product_id, package_id, operation,
		                                     source_repo_id, idempotency_key)
		      VALUES (?, ?, ?, 'replicate', ?, ?)`,
			requestID, productID, pkgID, srcRepo, fmt.Sprintf("bench-%d", i))
		exec(`INSERT INTO transfers (id, request_id, package_id, source_repo_id, target_repo_id,
		                             state, planned_job_count, created_at)
		      VALUES (?, ?, ?, ?, ?, 'running', ?, ?)`,
			id, requestID, pkgID, srcRepo, dstRepo, jobsEach,
			fmt.Sprintf("2026-01-01T%02d:00:00Z", i%24))

		for j := range jobsEach {
			values = append(values, "(?,?,?,?,?,?,?,?,?)")
			args = append(args, id, "blob",
				fmt.Sprintf("sha256:%064x", i*jobsEach+j),
				int64(100000+j), srcRepo, dstRepo, states[j%len(states)], j%4, int64(100000))
			if len(values) >= perStatement {
				flush()
			}
		}
	}
	flush()

	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}
