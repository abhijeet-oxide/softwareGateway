package store

import (
	"testing"
	"time"
)

// THE MEMO MUST NEVER CHANGE AN ANSWER, only where it came from.
//
// A remembered rollup is served instead of a query, so the only thing that
// makes it safe is that the two are the same numbers. This reads one page
// twice - cold, then warm - and requires every one of the twelve columns to
// match. A regression here is a progress bar that disagrees with itself
// between two refreshes of the same page, which is the failure this whole
// design is chosen to avoid.
func TestARememberedRollupIsTheSameAnswer(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.rollupEstate()
		h.settle(id, "succeeded", time.Minute)

		cold := h.listOne(t, id)
		if h.packages.rollups.len() == 0 {
			t.Fatal("nothing was remembered for a settled transfer, so the " +
				"warm read below proves nothing")
		}
		warm := h.listOne(t, id)

		if cold != warm {
			t.Errorf("the remembered rollup differs from the computed one:\n"+
				" computed: %+v\n remembered: %+v", cold, warm)
		}
	})
}

// A TRANSFER STILL RUNNING IS NEVER REMEMBERED.
//
// This is the invariant the design rests on: a live transfer's rollup changes
// under it, so it is computed on every read and can never be served stale.
// Without this, a download in flight would freeze at whatever it read first -
// a progress bar stuck at 94% forever, which is exactly the failure mode that
// ruled out maintained counters.
func TestARunningTransferIsNeverRemembered(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.rollupEstate() // running() leaves it `running`

		before := h.listOne(t, id)
		if n := h.packages.rollups.len(); n != 0 {
			t.Fatalf("%d rollups remembered for a running transfer, want 0", n)
		}

		// Another job finishes, as one does while somebody is watching.
		h.exec(`UPDATE jobs SET state = 'succeeded'
		         WHERE transfer_id = ? AND state = 'pending'`, id)

		after := h.listOne(t, id)
		if after.JobsDone <= before.JobsDone {
			t.Errorf("jobs done went from %d to %d after a job succeeded - the "+
				"listing is serving a frozen rollup for a running transfer",
				before.JobsDone, after.JobsDone)
		}
	})
}

// A TRANSFER THAT IS REOPENED AND SETTLES AGAIN REPORTS ITS NEW NUMBERS.
//
// The one way a terminal transfer changes is by ceasing to be one: a retry
// reopens a failed transfer, it runs again, and it settles again with
// different counts. A memo keyed on the id alone would be served afterwards
// and be wrong, and nothing about the second settle would look unusual.
//
// The entry carries the `updated_at` it was computed at, and every statement
// that changes a transfer's state sets that - so this passes without any
// reopen path knowing the memo exists.
func TestAReopenedTransferIsNotServedItsOldRollup(t *testing.T) {
	eachDialect(t, func(t *testing.T, h *activeHarness) {
		id := h.rollupEstate()
		h.settleVersioned(id, "failed")

		first := h.listOne(t, id)
		if h.packages.rollups.len() == 0 {
			t.Fatal("nothing was remembered, so this test proves nothing")
		}

		// The retry: reopened, more work done, settled again.
		//
		// `updated_at` is moved on each transition because that is what every
		// statement in this package that changes a transfer's state does - see
		// recovery.go and control.go. The harness's own settle() is a test
		// shortcut that skips it, which would make this test pass for the
		// wrong reason.
		h.settleVersioned(id, "running")
		h.exec(`UPDATE jobs SET state = 'succeeded'
		         WHERE transfer_id = ? AND state IN ('failed','pending')`, id)
		h.settleVersioned(id, "succeeded")

		second := h.listOne(t, id)
		if second.JobsDone == first.JobsDone {
			t.Errorf("jobs done is still %d after the transfer was retried and "+
				"more work succeeded - the old rollup is being served for a row "+
				"that has moved on", second.JobsDone)
		}
		if second.JobsFailed == first.JobsFailed && first.JobsFailed != 0 {
			t.Errorf("jobs failed is still %d after a retry cleared them",
				second.JobsFailed)
		}
	})
}

// The memo is bounded, so a long-running Coordinator cannot grow one entry per
// transfer it has ever listed.
func TestTheRollupMemoIsBounded(t *testing.T) {
	c := newRollupCache(4)
	for i := range 20 {
		id := string(rune('a' + i))
		c.put(id, "succeeded", "v1", transferRollup{JobsDone: i})
	}
	if got := c.len(); got > 4 {
		t.Errorf("the memo holds %d entries with a limit of 4", got)
	}
	// The most recent survive; the oldest are the ones dropped.
	if _, ok := c.get(string(rune('a'+19)), "succeeded", "v1"); !ok {
		t.Error("the most recently remembered rollup was evicted")
	}
}

// Nothing non-terminal is stored, whatever a caller asks for.
//
// put refuses it rather than trusting the call site, because that refusal is
// the invariant and a caller is exactly what gets it wrong.
func TestTheMemoRefusesAnythingStillMoving(t *testing.T) {
	c := newRollupCache(16)
	for _, state := range []string{
		"running", "pending", "ready", "planning", "paused", "syncing",
		"promoting", "verifying", "waiting", "cancelling",
		// `diverged` is settled by hand and `paused` resumes: neither is
		// finished, so neither may be remembered.
		"diverged",
	} {
		c.put("t-"+state, state, "v1", transferRollup{JobsDone: 1})
		if _, ok := c.get("t-"+state, state, "v1"); ok {
			t.Errorf("a transfer in state %q was remembered", state)
		}
	}
	if n := c.len(); n != 0 {
		t.Errorf("%d entries remembered for transfers that are still moving", n)
	}
}

// A row version that has moved on is a miss, even in a terminal state.
func TestAMemoIsNotServedAcrossARowVersion(t *testing.T) {
	c := newRollupCache(16)
	c.put("t1", "succeeded", "2026-01-01T00:00:00.000Z", transferRollup{JobsDone: 7})

	if _, ok := c.get("t1", "succeeded", "2026-01-01T00:00:00.000Z"); !ok {
		t.Fatal("the entry just written was not found at its own version")
	}
	if r, ok := c.get("t1", "succeeded", "2026-01-02T00:00:00.000Z"); ok {
		t.Errorf("a rollup from an older version of the row was served: %+v", r)
	}
}

// settleVersioned moves a transfer to a state the way production does: with
// `updated_at` moved too.
//
// The harness's settle() writes state and completed_at only. That is fine for
// the tests it was written for and wrong for this one, where `updated_at` IS
// the mechanism: a memo is kept across a read only while the row still carries
// the version it was computed from.
func (h *activeHarness) settleVersioned(id, state string) {
	h.t.Helper()
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	completed := "NULL"
	if terminalTransferStates[state] {
		completed = "?"
	}
	args := []any{state}
	if completed == "?" {
		args = append(args, at)
	}
	args = append(args, id)
	h.exec(`UPDATE transfers SET state = ?, completed_at = `+completed+`,
	               updated_at = `+h.packages.Dialect().Now()+`
	         WHERE id = ?`, args...)
	// SQLite stores these as text and compares them as text, so two writes
	// inside the same millisecond produce the same version. A real transition
	// is never that fast; this makes the test never that fast either.
	time.Sleep(2 * time.Millisecond)
}
