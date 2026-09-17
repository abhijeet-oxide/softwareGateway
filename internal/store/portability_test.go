package store

import (
	"os"
	"testing"
	"time"
)

// EVERY READ THAT TOUCHES A TYPED COLUMN, RUN ON BOTH DATABASES.
//
// # The bug
//
// Four queries coalesced `package_artifacts.annotations` with the empty
// string. SQLite stores that column as TEXT and was perfectly happy; Postgres
// declares it JSONB and must read ” AS JSON, which it refuses:
//
//	ERROR: invalid input syntax for type json (SQLSTATE 22P02)
//
// So listing a release's files, reading one of its files, listing its chart
// candidates and summarising what a transfer skipped could not work on
// Postgres at all - and the whole suite passed, because the whole suite runs on
// SQLite.
//
// # Why this is not covered by the tests that already exist
//
// Those queries ARE tested, thoroughly, for what they return. What they were
// never tested for is whether the database would accept them, and that is not
// a behaviour anybody thinks to assert: it is the thing a test takes for
// granted in order to test something else. This asserts exactly that, and
// nothing else, which is why it can afford to call every one of them.
//
// The companion is TestNoJSONColumnIsCoalescedWithAString, which catches the
// same mistake by reading the source and runs everywhere. This catches the
// mistakes that one cannot see - a cast, a comparison, an operator one dialect
// does not have - and needs a server.
//
// Skipped rather than failed without `TEST_POSTGRES_DSN`, so `go test ./...`
// on a laptop stays green and container-free. `task test:postgres` is what runs
// it, and CI is where it must.
func TestTypedColumnReadsRunOnBothDialects(t *testing.T) {
	run := func(t *testing.T, st Store) {
		p := NewPackages(st)
		ctx := t.Context()

		// A package id nothing has ever written under. THE ROWS DO NOT MATTER:
		// Postgres rejects these statements when it plans them, so an empty
		// table proves the same point as a full one and costs no fixtures.
		const absent = int64(-1)

		if _, _, err := p.PackageFiles(ctx, absent); err != nil {
			t.Errorf("PackageFiles: %v", err)
		}
		if _, err := p.FileInPackage(ctx, absent, "sha256:"+
			"0000000000000000000000000000000000000000000000000000000000000000"); err != nil &&
			err != ErrNotFound {
			t.Errorf("FileInPackage: %v", err)
		}
		if _, err := p.ChartCandidates(ctx, absent); err != nil {
			t.Errorf("ChartCandidates: %v", err)
		}
		// The two transfer summaries, which read the same column through a
		// GROUP BY - a shape the others do not have.
		if _, err := p.SkipBreakdown(ctx, "00000000-0000-0000-0000-000000000000"); err != nil {
			t.Errorf("SkipBreakdown: %v", err)
		}
		if _, err := p.PresentComponents(ctx, "00000000-0000-0000-0000-000000000000"); err != nil {
			t.Errorf("PresentComponents: %v", err)
		}

		// The availability record, whose reads compare a timestamp column the
		// two dialects store differently.
		av := NewAvailability(st)
		if _, err := av.Beat(ctx, "coordinator", "one", "1.0.0",
			AvailabilityHealthy, AvailabilityContinuity); err != nil {
			t.Errorf("Beat: %v", err)
		}
		if _, err := av.Beat(ctx, "coordinator", "one", "1.0.0",
			AvailabilityHealthy, AvailabilityContinuity); err != nil {
			t.Errorf("Beat again: %v", err)
		}
		runs, err := av.Runs(ctx, "coordinator", time.Hour)
		if err != nil {
			t.Errorf("Runs: %v", err)
		}
		if len(runs) != 1 {
			t.Errorf("%d runs after two beats, want 1 - the second must extend the first", len(runs))
		}
		if _, err := av.FirstRecorded(ctx, "coordinator"); err != nil {
			t.Errorf("FirstRecorded: %v", err)
		}
		if _, err := av.Prune(ctx, "coordinator", 24*time.Hour); err != nil {
			t.Errorf("Prune: %v", err)
		}
	}

	t.Run("sqlite", func(t *testing.T) { run(t, openTestStore(t)) })

	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Log("TEST_POSTGRES_DSN is unset, so the Postgres form of these " +
			"statements was not exercised - which is how the JSONB coalesce shipped")
		return
	}
	t.Run("postgres", func(t *testing.T) { run(t, openPostgresTestStore(t, dsn)) })
}
