package store

import (
	"fmt"
	"testing"
)

// THE BATCHED WEIGHING IS THE SAME ARITHMETIC, for every transfer on the page.
//
// TransferContentBytesFor replaces a call PER ROW of the transfer listing - a
// page of twenty-five was twenty-five round trips, each re-deriving the
// release's digests and the transfer's destinations from scratch. A profile of
// a slow Downloads page put TransferContentBytes under handleListTransfers and
// nothing else close.
//
// Because the batched version is the one the listing now uses and the single
// one still answers the detail page, the two must not drift. This holds them
// together the only way that means anything: the SAME estate, weighed both
// ways, digest for digest.
//
// The estate is built to exercise every branch of the CASE arithmetic at once,
// because a batched query that correlates on the wrong transfer would still
// agree on an estate where every transfer looks alike:
//
//   - content this transfer finished          -> weighs whole, as moved
//   - content in flight                       -> weighs what arrived
//   - content skipped                         -> present, not moved
//   - content with NO job that the target has -> present, not moved
//   - content shared between two transfers    -> counted once for each
func TestBatchedContentBytesMatchesOneAtATime(t *testing.T) {
	h := newPresentHarness(t)

	// A release of five blobs of different sizes, plus the manifest.
	shared := h.blob("shared", 1000)
	finished := h.blob("finished", 2000)
	inflight := h.blob("inflight", 4000)
	skipped := h.blob("skipped", 8000)
	untouched := h.blob("untouched", 16000)
	h.artifact("app", shared, finished, inflight, skipped, untouched)

	// Three transfers of the same release, each having got a different
	// distance - which is what makes a correlation on the wrong transfer
	// visible rather than a coincidence.
	ids := []string{
		h.transferN(1),
		h.transferN(2),
		h.transferN(3),
	}

	// The first has finished some and is part-way through another.
	h.job(ids[0], finished, "succeeded")
	h.jobIn(ids[0], inflight, "leased", "orbs/cfx-5000-k8s", 1500)
	h.job(ids[0], skipped, "skipped")

	// The second has done nothing but shares the release, and the destination
	// already holds one blob that therefore never got a job.
	h.placed(untouched)

	// The third has finished everything it was given.
	h.job(ids[2], shared, "succeeded")
	h.job(ids[2], finished, "succeeded")

	batched, err := h.packages.TransferContentBytesFor(t.Context(), ids)
	if err != nil {
		t.Fatalf("batched weighing: %v", err)
	}

	for _, id := range ids {
		one, err := h.packages.TransferContentBytes(t.Context(), id)
		if err != nil {
			t.Fatalf("weigh %s alone: %v", id, err)
		}
		got := batched[id]
		if got != one {
			t.Errorf("transfer %s:\n  one at a time: %+v\n  batched:       %+v", id, one, got)
		}
		if one.Total == 0 {
			t.Errorf("transfer %s weighs nothing, so this comparison proves nothing", id)
		}
	}
}

// An empty page asks nothing of the database and answers nothing.
func TestWeighingNoTransfersAsksNothing(t *testing.T) {
	h := newPresentHarness(t)
	got, err := h.packages.TransferContentBytesFor(t.Context(), nil)
	if err != nil {
		t.Fatalf("weighing an empty page: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty page weighed %d transfers", len(got))
	}
}

// A transfer with nothing to weigh is absent rather than an error, which is
// what lets the caller treat a missing entry as zero.
func TestATransferWithNoContentIsSimplyAbsent(t *testing.T) {
	h := newPresentHarness(t)
	id := h.transferN(9) // a transfer over a package with no artifacts

	got, err := h.packages.TransferContentBytesFor(t.Context(), []string{id})
	if err != nil {
		t.Fatalf("weighing a transfer with no content: %v", err)
	}
	if c, ok := got[id]; ok && c.Total != 0 {
		t.Errorf("a transfer with no artifacts weighed %+v", c)
	}
}

// transferN is transfer() for more than one, which a batch needs.
func (h *presentHarness) transferN(n int) string {
	h.t.Helper()
	id := fmt.Sprintf("aaaaaaaa-1111-2222-3333-%012d", n)

	h.exec(`INSERT INTO transfer_requests (id, product_id, package_id, operation,
	                                       source_repo_id, idempotency_key)
	         VALUES (?, ?, ?, 'replicate', ?, ?)`,
		"req-"+id, h.productID, h.packageID, h.repoID, "key-"+id)

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := h.packages.CreateTransfer(h.t.Context(), tx, TransferRow{
		ID: id, RequestID: "req-" + id, PackageID: h.packageID,
		SourceRepoID: h.repoID, TargetRepoID: h.repoID, Priority: 50,
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// What a page of content weighing costs, one at a time against batched.
//
// This is the measurement the Downloads page was missing: the listing's own
// query plan says nothing about it, because this runs outside the listing's
// query, and internal/store's benchmarks stopped at ListTransfers. Run it:
//
//	go test ./internal/store -run '^$' -bench BenchmarkPageContentBytes
func BenchmarkPageContentBytes(b *testing.B) {
	// One page, over an estate with real job counts behind it - the per-digest
	// arithmetic is correlated over `jobs`, so a transfer with four jobs
	// measures nothing anybody would notice.
	const (
		page     = 25
		jobsEach = 200
	)

	st := openBenchStore(b)
	p := NewPackages(st)
	seedBenchEstate(b, st, page, jobsEach)

	ids := make([]string, 0, page)
	rows, err := st.DB().QueryContext(b.Context(), `SELECT id FROM transfers`)
	if err != nil {
		b.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			b.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) != page {
		b.Fatalf("seeded %d transfers, want %d", len(ids), page)
	}

	b.Run("one-at-a-time", func(b *testing.B) {
		for b.Loop() {
			for _, id := range ids {
				if _, err := p.TransferContentBytes(b.Context(), id); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("batched", func(b *testing.B) {
		for b.Loop() {
			if _, err := p.TransferContentBytesFor(b.Context(), ids); err != nil {
				b.Fatal(err)
			}
		}
	})
}
