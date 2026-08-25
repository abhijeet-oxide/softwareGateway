package transfer

import (
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The same freeze as cmd/coordinator's EnsureTarget, one layer up.
//
// Create opens a transaction, writes the request and one transfer per
// destination, and commits. When the transfer already exists - a replay, which
// is the ORDINARY case for a retried click, a re-issued CLI command or a rule
// that fires twice - CreateTransfer's ON CONFLICT DO NOTHING reports "a row
// exists" without saying which, and the ID is read back.
//
// That read went through the pool while this goroutine held the pool's only
// connection in the open transaction, so it waited for a connection only it
// could release. Two consequences, in ascending order of seriousness: the read
// could not have seen an uncommitted row from its own transaction anyway, and
// the wait never ends - taking the Coordinator's database down with it until
// the process is restarted.
func TestReplayedRequestDoesNotDeadlockOnTheSingleConnection(t *testing.T) {
	h := newHarness(t)
	row := h.insertBarePackage("v1")

	requester := NewRequester(h.packages, singleTargetCatalog(h))
	req := CreateRequest{Product: "vendor-a", Package: "v1", Row: row}

	first, err := requester.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if !first.Created {
		t.Fatal("the first request reported itself as a replay")
	}
	if len(first.TransferIDs) != 1 {
		t.Fatalf("first request opened %d transfers, want 1", len(first.TransferIDs))
	}

	// The replay. Identical inputs produce the identical idempotency key, so
	// the request row is reused and the transfer insert conflicts - which is
	// the path that hung.
	type result struct {
		res CreateResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := requester.Create(t.Context(), req)
		done <- result{res, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("replayed request: %v", got.err)
		}
		if got.res.Created {
			t.Error("the replay reported itself as a new request; it would " +
				"have produced a second piece of work for one intent")
		}
		// The point of reading the ID back at all: the caller carries on
		// planning against the transfer that already exists rather than a
		// fresh UUID the UNIQUE constraint would reject.
		if len(got.res.TransferIDs) != 1 || got.res.TransferIDs[0] != first.TransferIDs[0] {
			t.Errorf("replay returned transfers %v, want the original %v",
				got.res.TransferIDs, first.TransferIDs)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Create deadlocked on the replay path: it is holding the only " +
			"database connection while waiting for another one, which wedges " +
			"every later request in the process until it is restarted")
	}
}

// singleTargetCatalog resolves against the harness's real catalog rows, so
// Create's inserts satisfy the foreign keys the migration declares.
func singleTargetCatalog(h *harness) *fakeCatalog {
	source := RepoView{
		RepositoryID: h.sourceID, Name: "source-vendor/suite", Role: "source",
		Registry: "registry.example.com", Repository: "vendor/suite",
	}
	target := RepoView{
		RepositoryID: h.targetID, Name: "target-mirror/vendor/suite", Role: "target",
		Registry: "registry.example.com", Repository: "mirror/vendor/suite", Default: true,
	}
	return &fakeCatalog{
		view: ProductView{
			Name:    "vendor-a",
			Sources: []RepoView{source},
			Targets: []RepoView{target},
		},
		rows: map[int64]RepoView{h.sourceID: source, h.targetID: target},
	}
}

// insertBarePackage records a package without going through the registry.
//
// seedPackage fetches a real manifest, which this test does not need: nothing
// here walks a tree, and the request path only reads the row's IDs and tag.
func (h *harness) insertBarePackage(tag string) store.PackageRow {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertPackage(h.t.Context(), tx, store.PackageRow{
		ProductID: h.productID, SourceRepoID: h.sourceID, Tag: tag,
		ManifestDigest: "sha256:" + repeatHex(64), MediaType: "application/json",
		ArtifactCount: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}

	row, err := h.packages.GetPackageByID(h.t.Context(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return row
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
