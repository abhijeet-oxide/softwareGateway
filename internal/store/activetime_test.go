package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The bug these exist for.
//
// Elapsed was `completed_at - started_at`. A worker that crashed at midnight
// and came back at noon left a transfer reporting twelve hours, of which
// perhaps twenty minutes were spent moving bytes - and every throughput derived
// from it wrong by a factor of thirty-six.
//
// The tests below are written against the two things that make the sweep
// correct: it credits only what it can SEE happening, and it refuses a gap it
// cannot vouch for.

const sweepEvery = 30 * time.Second

// eachDialect runs a case against SQLite always, and against Postgres when one
// is reachable.
//
// # Why this file, of all of them, insists on both
//
// The accrual is the only thing in the store written almost entirely in
// dialect-specific SQL: three statements built out of SecondsBetween, whose
// SQLite form goes through julianday on TEXT and whose Postgres form subtracts
// two timestamptz values. Those have different failure modes, and the dangerous
// one is silent: an expression SQLite answers NULL to does not raise, it simply
// credits nothing. A suite that only ever saw one dialect would pass while the
// other recorded zero seconds for every download ever run.
//
// Skipped rather than failed when there is no server, so `go test ./...` on a
// laptop stays green. `SWGW_TEST_POSTGRES` is the DSN.
func eachDialect(t *testing.T, run func(t *testing.T, h *activeHarness)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) { run(t, newActiveHarness(t, openTestStore)) })

	dsn := os.Getenv("SWGW_TEST_POSTGRES")
	if dsn == "" {
		t.Log("SWGW_TEST_POSTGRES is unset, so the Postgres form of these " +
			"statements was not exercised")
		return
	}
	t.Run("postgres", func(t *testing.T) {
		run(t, newActiveHarness(t, func(t *testing.T) Store {
			return openPostgresTestStore(t, dsn)
		}))
	})
}

// openPostgresTestStore migrates a SCHEMA of its own inside the target
// database, so runs cannot see each other's rows and nothing has to be dropped
// between them.
func openPostgresTestStore(t *testing.T, dsn string) Store {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), rand.IntN(1<<20))
	admin, err := Open(ctx, Config{Driver: DriverPostgres, DSN: dsn})
	if err != nil {
		t.Skipf("no Postgres at %s: %v", dsn, err)
	}
	if _, err := admin.DB().ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Skipf("could not create a test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.DB().ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := Open(ctx, Config{
		Driver: DriverPostgres,
		DSN:    dsn + sep + "search_path=" + schema,
	})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := Migrate(ctx, s, nil); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}
	return s
}

// activeHarness builds its own fixtures rather than reusing transferRefHarness,
// which is SQLite-only: it reads back generated keys with LastInsertId, and the
// Postgres driver does not implement it. Everything here goes through RETURNING
// and the dialect's own placeholder rewriting instead.
type activeHarness struct {
	t        *testing.T
	st       Store
	packages *Packages
	pkgID    int64
	repoID   int64
	n        int
}

func newActiveHarness(t *testing.T, open func(*testing.T) Store) *activeHarness {
	t.Helper()

	st := open(t)
	h := &activeHarness{t: t, st: st, packages: NewPackages(st)}

	var productID int64
	if err := h.query(`INSERT INTO products (name, config_hash, config)
	                        VALUES ('p','h','{}') RETURNING id`).Scan(&productID); err != nil {
		t.Fatal(err)
	}

	tx, err := st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	h.repoID, err = h.packages.EnsureRepository(t.Context(), tx, productID, "source", "src",
		"registry.example.com", "vendor/suite", "generic", "config", "")
	if err != nil {
		t.Fatal(err)
	}
	h.pkgID, err = h.packages.InsertPackage(t.Context(), tx, PackageRow{
		ProductID: productID, SourceRepoID: h.repoID, Tag: "v1",
		ManifestDigest: "sha256:" + strings.Repeat("a", 64), MediaType: "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *activeHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(
		h.t.Context(), h.packages.dialect.Rewrite(query), args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *activeHarness) query(query string, args ...any) *rowScanner {
	return &rowScanner{row: h.st.DB().QueryRowContext(
		h.t.Context(), h.packages.dialect.Rewrite(query), args...)}
}

// running creates a running transfer whose anchor is `anchoredAgo` in the past,
// with or without a job in a worker's hands.
func (h *activeHarness) running(anchoredAgo time.Duration, leased bool) string {
	h.t.Helper()

	// Both ids have to be well-formed UUIDs. SQLite stores them as text and
	// accepts anything; Postgres declares the columns UUID and rejects a
	// readable label, which is one of the two dialects' disagreements this file
	// exists to keep honest.
	h.n++
	id := fmt.Sprintf("%08d-aaaa-bbbb-cccc-dddddddddddd", h.n)
	requestID := fmt.Sprintf("%08d-eeee-ffff-0000-111111111111", h.n)

	h.exec(`INSERT INTO transfer_requests
	            (id, product_id, package_id, operation, source_repo_id, idempotency_key)
	     SELECT ?, product_id, id, 'replicate', source_repo_id, ? FROM packages WHERE id = ?`,
		requestID, "key-"+id, h.pkgID)

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := h.packages.CreateTransfer(h.t.Context(), tx, TransferRow{
		ID: id, RequestID: requestID, PackageID: h.pkgID,
		SourceRepoID: h.repoID, TargetRepoID: h.repoID, Priority: 50,
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}

	// The anchor is written as a timestamp the dialect will read back, which is
	// the whole reason these tests run twice: SQLite compares the text and
	// Postgres parses it.
	anchor := time.Now().UTC().Add(-anchoredAgo).Format("2006-01-02T15:04:05.000Z")
	h.exec(`UPDATE transfers SET state='running', started_at=?, last_active_at=?
	         WHERE id = ?`, anchor, anchor, id)

	state := "pending"
	if leased {
		state = "leased"
	}
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
	                          target_repo_id, state, wave, attempts, max_attempts)
	         VALUES (?, 'blob', ?, 1024, ?, ?, ?, 0, 1, 8)`,
		id, "sha256:"+strings.Repeat(strconv.Itoa(h.n%10), 64), h.repoID, h.repoID, state)
	return id
}

func (h *activeHarness) settle(id, state string, completedAgo time.Duration) {
	h.t.Helper()
	at := time.Now().UTC().Add(-completedAgo).Format("2006-01-02T15:04:05.000Z")
	h.exec(`UPDATE transfers SET state=?, completed_at=? WHERE id = ?`, state, at, id)
}

func (h *activeHarness) active(id string) float64 {
	h.t.Helper()
	var seconds float64
	if err := h.query(`SELECT active_seconds FROM transfers WHERE id = ?`, id).
		Scan(&seconds); err != nil {
		h.t.Fatal(err)
	}
	return seconds
}

func (h *activeHarness) anchored(id string) bool {
	h.t.Helper()
	// `any` because the two dialects hand back different Go types for the same
	// column - SQLite a string, Postgres a time.Time - and the only thing this
	// asks is whether there is a value at all.
	var anchor any
	if err := h.query(
		`SELECT last_active_at FROM transfers WHERE id = ?`, id).Scan(&anchor); err != nil {
		h.t.Fatal(err)
	}
	return anchor != nil
}

func (h *activeHarness) accrue() int {
	h.t.Helper()
	credited, err := h.packages.AccrueActiveTime(h.t.Context(), sweepEvery)
	if err != nil {
		h.t.Fatal(err)
	}
	return credited
}

// The ordinary pass: a worker is holding a job, so the interval since the last
// pass was time somebody spent waiting for this download.
func TestTimeIsCreditedWhileAWorkerHoldsAJob(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(sweepEvery, true)

		if credited := h.accrue(); credited != 1 {
			t.Fatalf("credited %d transfers, want 1", credited)
		}
		// Within a second or two of the interval: the anchor was set by wall
		// clock, so exact equality would be a test of the machine's scheduler.
		if got := h.active(id); got < 29 || got > 33 {
			t.Errorf("credited %.1fs for a %s pass, want about %.0fs",
				got, sweepEvery, sweepEvery.Seconds())
		}
	})
}

// THE CASE THIS WAS BUILT FOR. A download that has been asked for and is
// waiting - no worker holding any of it - is not spending time downloading, and
// an hour of waiting must add nothing.
func TestWaitingForAWorkerIsNotTimeSpentDownloading(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(sweepEvery, false)

		h.accrue()
		if got := h.active(id); got != 0 {
			t.Errorf("credited %.1fs to a transfer no worker was touching, want 0", got)
		}
		// And it is re-anchored, so the wait does not land on the next pass the
		// moment a worker does pick it up.
		if !h.anchored(id) {
			t.Error("the transfer lost its anchor, so the next pass has nothing to measure from")
		}
	})
}

// THE OTHER CASE. The Coordinator was down: the anchor is stale by the whole
// outage and the leases are stale with it. Believing the anchor here is exactly
// how wall clock came to be the number on the page.
func TestAGapNobodyObservedIsNotCredited(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(24*time.Hour, true)

		if credited := h.accrue(); credited != 0 {
			t.Errorf("credited %d transfers for a day nothing was watching", credited)
		}
		if got := h.active(id); got != 0 {
			t.Errorf("credited %.1fs for an unobserved day, want 0 - under-counting a "+
				"period nobody measured is the only honest option", got)
		}
		// Re-anchored rather than left stale, so the NEXT pass measures a real
		// interval instead of refusing the same day forever.
		if !h.anchored(id) {
			t.Error("the stale anchor was not reset, so this transfer can never accrue again")
		}
	})
}

// A pass credits the time since the LAST pass, not its nominal interval.
func TestAPassCreditsTheTimeSinceTheLastPass(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(sweepEvery, true)

		h.accrue()
		h.accrue() // microseconds later, so it is owed almost nothing

		if got := h.active(id); got < 29 || got > 33 {
			t.Errorf("two immediate passes credited %.1fs, want about %.0fs",
				got, sweepEvery.Seconds())
		}
	})
}

// A transfer that finished between two passes was never seen with a job in
// flight. Without the close-out it reports having taken no time at all - which
// is the answer a fully deduplicated download, finishing in under a second,
// would get every single time.
func TestATransferThatSettledBetweenPassesKeepsItsLastFragment(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(10*time.Second, true)
		// It succeeded four seconds after the anchor - well inside this pass.
		h.settle(id, "succeeded", 6*time.Second)

		h.accrue()
		if got := h.active(id); got < 3 || got > 5 {
			t.Errorf("credited %.1fs, want about 4 - measured to the moment it "+
				"completed, not to the moment somebody swept", got)
		}
		// The anchor is released, which is what makes the close-out run once.
		if h.anchored(id) {
			t.Error("a settled transfer kept its anchor and will be credited again")
		}
	})
}

// A settled transfer must never be credited twice, however often the sweep runs.
func TestASettledTransferIsAccountedForOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(10*time.Second, true)
		h.settle(id, "succeeded", 6*time.Second)

		for range 3 {
			h.accrue()
		}
		if got := h.active(id); got > 5 {
			t.Errorf("credited %.1fs over three passes, want it counted once", got)
		}
	})
}

// A settled transfer the close-out cannot account for - no completion time -
// must still lose its anchor, or every future pass re-examines it forever.
func TestASettledTransferWithNoCompletionTimeStillLosesItsAnchor(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(10*time.Second, true)
		h.exec(`UPDATE transfers SET state='failed', completed_at=NULL WHERE id = ?`, id)

		h.accrue()
		if h.anchored(id) {
			t.Error("a settled transfer with no completion time kept its anchor")
		}
	})
}

// A transfer nothing has ever leased has no anchor. The pass must give it one
// rather than skipping it forever, and must credit it nothing on the way.
func TestATransferNobodyHasLeasedIsAnchoredAndCreditedNothing(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(sweepEvery, true)
		h.exec(`UPDATE transfers SET last_active_at = NULL WHERE id = ?`, id)

		h.accrue()
		if got := h.active(id); got != 0 {
			t.Errorf("credited %.1fs to a transfer with no anchor to measure from", got)
		}
		if !h.anchored(id) {
			t.Error("no anchor was set, so this transfer can never accrue")
		}
	})
}

// The whole point, end to end: a transfer that ran for a minute, sat through a
// day of outage, and ran for another minute reports two minutes and not a day.
func TestAnOutageAddsNothingToTheTimeSpentDownloading(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(sweepEvery, true)

		// Two ordinary passes while a worker holds a job.
		h.accrue()
		h.exec(`UPDATE transfers SET last_active_at = ? WHERE id = ?`,
			time.Now().UTC().Add(-sweepEvery).Format("2006-01-02T15:04:05.000Z"), id)
		h.accrue()
		working := h.active(id)

		// Then the fleet disappears for a day. Nothing sweeps, so the anchor
		// goes stale by the whole of it - which is what a restart finds.
		h.exec(`UPDATE transfers SET last_active_at = ? WHERE id = ?`,
			time.Now().UTC().Add(-24*time.Hour).Format("2006-01-02T15:04:05.000Z"), id)
		h.accrue()

		if got := h.active(id); got != working {
			t.Errorf("a day of outage changed the figure from %.1fs to %.1fs", working, got)
		}
		if working < 55 || working > 66 {
			t.Errorf("two %s passes credited %.1fs, want about %.0fs",
				sweepEvery, working, 2*sweepEvery.Seconds())
		}
	})
}
