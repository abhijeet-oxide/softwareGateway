package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The gap these exist for.
//
// The metric catalog measured the API and nothing else. A deployment where
// every endpoint answered in four milliseconds and not one byte was being
// transferred looked perfectly healthy on every graph there was, because
// nothing in the process could see the queue. These cover the read that fixed
// that - and, more importantly, the two ways it could quietly stop being
// trustworthy: by counting the wrong rows, and by stopping using its index.

// seedJob inserts one job in a given state against the harness's transfer.
func (h *activeHarness) seedJob(transferID, state string, size, moved int64, paused bool, createdAgo string) {
	h.t.Helper()
	h.n++
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id,
	                          target_repo_id, state, paused, bytes_transferred,
	                          wave, attempts, max_attempts, created_at)
	         VALUES (?, 'blob', ?, ?, ?, ?, ?, ?, ?, 0, 0, 8, `+createdAgo+`)`,
		transferID, fmt.Sprintf("sha256:%064d", h.n), size,
		h.repoID, h.repoID, state, paused, moved)
}

// TestQueueSnapshotCountsOnlyOutstandingWork is the gauge/counter distinction,
// held.
//
// A settled job must not appear in a depth gauge. It is history, and history
// leaves when the archiver runs - so a gauge that counted it would FALL when
// old rows were deleted, which reads as work being undone. The counterpart is
// jobs_completed_total, which is a counter precisely because it may not.
func TestQueueSnapshotCountsOnlyOutstandingWork(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(0, false) // creates one pending job of its own
		h.seedJob(id, "pending", 1000, 0, false, h.packages.dialect.Now())
		h.seedJob(id, "blocked", 2000, 0, false, h.packages.dialect.Now())
		h.seedJob(id, "leased", 4000, 512, false, h.packages.dialect.Now())
		h.seedJob(id, "succeeded", 8000, 8000, false, h.packages.dialect.Now())
		h.seedJob(id, "failed", 16000, 0, false, h.packages.dialect.Now())
		h.seedJob(id, "cancelled", 32000, 0, false, h.packages.dialect.Now())

		s, err := h.packages.QueueSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		// running() leaves one pending job behind, hence two.
		if got := s.Jobs["pending"]; got != 2 {
			t.Errorf("pending = %d, want 2", got)
		}
		if got := s.Jobs["blocked"]; got != 1 {
			t.Errorf("blocked = %d, want 1", got)
		}
		if got := s.Jobs["leased"]; got != 1 {
			t.Errorf("leased = %d, want 1", got)
		}
		for _, settled := range []string{"succeeded", "failed", "cancelled"} {
			if n, ok := s.Jobs[settled]; ok {
				t.Errorf("the snapshot reported %d %s jobs.\n"+
					"Settled work must not reach a gauge: archiving those rows would\n"+
					"make the gauge fall, which reads as work being undone. Count them\n"+
					"with jobs_completed_total instead.", n, settled)
			}
		}

		// running()'s own job is 1024 bytes.
		if want := int64(1000 + 2000 + 1024); s.PendingBytes != want {
			t.Errorf("PendingBytes = %d, want %d (blocked and pending only)", s.PendingBytes, want)
		}
		if s.InFlightBytes != 512 {
			t.Errorf("InFlightBytes = %d, want 512 (what the leased job has moved)", s.InFlightBytes)
		}
	})
}

// TestQueueSnapshotSeparatesPausedWork keeps the two readings of a deep queue
// apart: paused is somebody's decision, not paused is an incident.
func TestQueueSnapshotSeparatesPausedWork(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(0, false)
		h.seedJob(id, "pending", 1, 0, true, h.packages.dialect.Now())
		h.seedJob(id, "pending", 1, 0, true, h.packages.dialect.Now())
		h.seedJob(id, "pending", 1, 0, false, h.packages.dialect.Now())

		s, err := h.packages.QueueSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Jobs["pending"]; got != 4 {
			t.Errorf("pending = %d, want 4 (paused work is still outstanding)", got)
		}
		if s.JobsPaused != 2 {
			t.Errorf("JobsPaused = %d, want 2", s.JobsPaused)
		}
	})
}

// TestQueueSnapshotAgesTheOldestPendingJob is the dialect-sensitive one.
//
// The age is built with Dialect.SecondsBetween, whose two forms have nothing in
// common - Postgres subtracts timestamptz values, SQLite does arithmetic on
// text through julianday - and whose failure mode is silent: an expression
// SQLite cannot parse answers NULL, which COALESCE turns into a confident zero.
// A zero here says "the queue is keeping up" about a queue that has not moved
// in a day, so it is worth a test that runs on both.
func TestQueueSnapshotAgesTheOldestPendingJob(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.running(0, false)
		h.seedJob(id, "pending", 1, 0, false, h.packages.dialect.TimeAgo("3600"))
		h.seedJob(id, "pending", 1, 0, false, h.packages.dialect.TimeAgo("60"))

		s, err := h.packages.QueueSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if s.OldestPendingSeconds < 3500 || s.OldestPendingSeconds > 3700 {
			t.Errorf("OldestPendingSeconds = %.0f, want about 3600.\n"+
				"Zero here usually means Dialect.SecondsBetween produced NULL on this\n"+
				"dialect, which COALESCE then turned into a confident 'the queue is\n"+
				"keeping up' about a queue that has not moved in an hour.",
				s.OldestPendingSeconds)
		}
	})
}

// TestQueueSnapshotCountsWorkerCapacity covers the three numbers that say
// whether the fleet or the queue is the constraint.
func TestQueueSnapshotCountsWorkerCapacity(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		h.exec(`INSERT INTO workers (id, version, max_concurrency, granted_concurrency,
		                             active_jobs, state)
		         VALUES ('w1', 'v1', 8, 4, 3, 'active'),
		                ('w2', 'v1', 8, 2, 2, 'active'),
		                ('w3', 'v1', 8, 8, 8, 'stale')`)

		s, err := h.packages.QueueSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if s.Workers["active"] != 2 || s.Workers["stale"] != 1 {
			t.Errorf("workers = %v, want 2 active and 1 stale", s.Workers)
		}
		// The stale worker's eight slots must not be counted. A fleet reported
		// as able to absorb work it cannot reach is worse than one reported
		// small: it is the number an autoscaler would act on.
		if s.SlotsActive != 5 || s.SlotsGranted != 6 || s.SlotsMax != 16 {
			t.Errorf("slots active/granted/max = %d/%d/%d, want 5/6/16 - "+
				"a stale worker contributes no capacity",
				s.SlotsActive, s.SlotsGranted, s.SlotsMax)
		}
	})
}

func TestQueueSnapshotReportsDatabaseSize(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		s, err := h.packages.QueueSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if s.DatabaseBytes <= 0 {
			t.Errorf("DatabaseBytes = %d; a migrated database is not empty, so this\n"+
				"dialect's size query is returning nothing useful", s.DatabaseBytes)
		}
	})
}

// TestQueueGaugeStatesMatchTheirIndexes is the one that fails for the mistake
// nobody would otherwise notice.
//
// The snapshot runs on a timer forever, and it is only affordable because both
// its queries are served by a PARTIAL index. Add a state to the Go list -
// which is the natural thing to do when the schema gains one - and the
// predicate no longer covers the query, so the planner falls back to a scan of
// the whole table. Nothing about the numbers looks wrong. The read just
// silently becomes proportional to everything ever transferred, several times
// a minute, forever.
func TestQueueGaugeStatesMatchTheirIndexes(t *testing.T) {
	for _, dialect := range []string{"postgres", "sqlite"} {
		t.Run(dialect, func(t *testing.T) {
			path := filepath.Join("..", "..", "db", "migrations", dialect,
				"00054_queue_state_index.sql")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)

			for _, c := range []struct {
				index string
				want  string
			}{
				{"jobs_live_state_idx", unsettledJobStates},
				{"transfers_live_state_idx", unsettledTransferStates},
			} {
				got := indexPredicateStates(t, body, c.index)
				want := statesIn(c.want)
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("%s covers %v but the snapshot asks about %v.\n\n"+
						"They have to be the same set. A state the query asks for and the\n"+
						"index does not carry turns an index-only read into a scan of the\n"+
						"whole table, taken every %s, forever - and the numbers still look\n"+
						"right, so nothing else will tell you.\n\n"+
						"Fix: add the state to the WHERE clause in %s.",
						c.index, got, want, "15s", path)
				}
			}
		})
	}
}

// indexPredicateStates pulls the quoted states out of one CREATE INDEX's
// WHERE clause.
func indexPredicateStates(t *testing.T, body, index string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)CREATE INDEX ` + index + `\b.*?WHERE state IN \((.*?)\)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CREATE INDEX %s with a state predicate in the migration", index)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		if s := strings.Trim(strings.TrimSpace(part), "'"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TestQueueSnapshotUsesItsIndex asks SQLite what it actually did.
//
// The test above holds the two lists together; this one holds the PLAN. They
// catch different mistakes: a predicate can match the query perfectly and
// still not be used, because the index was dropped, renamed, or never created
// on a database that had already been migrated past 00054.
func TestQueueSnapshotUsesItsIndex(t *testing.T) {
	h := newActiveHarness(t, openTestStore)

	for _, c := range []struct{ name, query, index string }{
		{
			"job depth",
			fmt.Sprintf(`SELECT state, COUNT(*) FROM jobs WHERE state IN (%s) GROUP BY state`,
				unsettledJobStates),
			"jobs_live_state_idx",
		},
		{
			"transfer depth",
			fmt.Sprintf(`SELECT state, COUNT(*) FROM transfers WHERE state IN (%s) GROUP BY state`,
				unsettledTransferStates),
			"transfers_live_state_idx",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, err := h.st.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+c.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()

			var plan strings.Builder
			for rows.Next() {
				var id, parent, notUsed int
				var detail string
				if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(detail + "\n")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan.String(), c.index) {
				t.Errorf("the %s query does not use %s:\n\n%s\n"+
					"This read runs on a timer forever. Without the index it is a scan of\n"+
					"the whole table, so the cost of watching the queue grows with\n"+
					"everything ever transferred.", c.name, c.index, plan.String())
			}
		})
	}
}
