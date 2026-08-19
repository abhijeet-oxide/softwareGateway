package store

import (
	"context"
	"testing"
)

func chainFixture(t *testing.T) (*Packages, *Replication, int64, context.Context) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()

	var productID int64
	if err := s.DB().QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a', 'h', '{}') RETURNING id`,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return NewPackages(s), NewReplication(s), productID, ctx
}

// seedChain records a request and two ordered steps: jfrog-store then ocp-prod.
func seedChain(t *testing.T, p *Packages, productID int64, ctx context.Context) (first, second string) {
	t.Helper()
	db := p.DB()

	var repoA, repoB, pkgID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO repositories (product_id, role, name, registry_host, repository_path)
		 VALUES (?, 'target', 'jfrog-store', 'artifactory.example.com', 'docker-local') RETURNING id`,
		productID).Scan(&repoA); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO repositories (product_id, role, name, registry_host, repository_path)
		 VALUES (?, 'target', 'ocp-prod', 'quay.example.com', 'platform-prod/x') RETURNING id`,
		productID).Scan(&repoB); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO packages (product_id, source_repo_id, tag, manifest_digest, media_type, state)
		 VALUES (?, ?, 'v3.2.1', 'sha256:aaa', 'application/vnd.oci.image.index.v1+json', 'discovered')
		 RETURNING id`, productID, repoA).Scan(&pkgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
		 VALUES ('req-1', ?, ?, 'replicate', ?, 'key-1')`, productID, pkgID, repoA); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	first, second = "t-first", "t-second"
	if _, err := p.CreateTransfer(ctx, tx, TransferRow{
		ID: first, RequestID: "req-1", PackageID: pkgID,
		SourceRepoID: repoA, TargetRepoID: repoA, Priority: 50, StepIndex: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateTransfer(ctx, tx, TransferRow{
		ID: second, RequestID: "req-1", PackageID: pkgID,
		SourceRepoID: repoA, TargetRepoID: repoB, Priority: 50,
		StepIndex: 1, DependsOn: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return first, second
}

func stateOf(t *testing.T, p *Packages, id string) string {
	t.Helper()
	var state string
	if err := p.DB().QueryRowContext(context.Background(),
		`SELECT state FROM transfers WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func setState(t *testing.T, p *Packages, id, state string) {
	t.Helper()
	if _, err := p.DB().ExecContext(context.Background(),
		`UPDATE transfers SET state = ? WHERE id = ?`, state, id); err != nil {
		t.Fatal(err)
	}
}

// A step with a predecessor must not be planned before that predecessor
// succeeds - planning it early would read from a target that does not hold the
// content yet.
func TestDependentStepOpensWaiting(t *testing.T) {
	p, _, productID, ctx := chainFixture(t)
	first, second := seedChain(t, p, productID, ctx)

	if got := stateOf(t, p, first); got != "planning" {
		t.Fatalf("the head of a chain is planned immediately, got %q", got)
	}
	if got := stateOf(t, p, second); got != "waiting" {
		t.Fatalf("a dependent step waits, got %q", got)
	}
}

func TestStepIsReleasedWhenItsPredecessorSucceeds(t *testing.T) {
	p, r, productID, ctx := chainFixture(t)
	first, second := seedChain(t, p, productID, ctx)

	setState(t, p, first, "succeeded")
	released, skipped, err := r.AdvanceSteps(ctx)
	if err != nil {
		t.Fatalf("AdvanceSteps: %v", err)
	}
	if released != 1 || skipped != 0 {
		t.Fatalf("released %d, skipped %d", released, skipped)
	}
	if got := stateOf(t, p, second); got != "planning" {
		t.Fatalf("state = %q", got)
	}
}

// THE security property: a failed step means the one after it never runs.
// Under verify.policy enforce, a destination that fails verification reaches
// `failed`, so the Quay step is skipped and Quay is never configured.
func TestStepIsSkippedWhenItsPredecessorFails(t *testing.T) {
	p, r, productID, ctx := chainFixture(t)
	first, second := seedChain(t, p, productID, ctx)

	setState(t, p, first, "failed")
	released, skipped, err := r.AdvanceSteps(ctx)
	if err != nil {
		t.Fatalf("AdvanceSteps: %v", err)
	}
	if released != 0 || skipped != 1 {
		t.Fatalf("released %d, skipped %d", released, skipped)
	}
	if got := stateOf(t, p, second); got != "skipped" {
		t.Fatalf("state = %q - and it must be `skipped`, not `failed`: nothing was attempted against that target", got)
	}
}

// `diverged` counts as unsuccessful FOR A CHAIN, because the destination holds
// content we did not ask for and copying from it would propagate the wrong
// digest onward.
func TestDivergedPredecessorSkipsTheNextStep(t *testing.T) {
	p, r, productID, ctx := chainFixture(t)
	first, second := seedChain(t, p, productID, ctx)

	setState(t, p, first, "diverged")
	if _, skipped, err := r.AdvanceSteps(ctx); err != nil || skipped != 1 {
		t.Fatalf("skipped %d, err %v", skipped, err)
	}
	if got := stateOf(t, p, second); got != "skipped" {
		t.Fatalf("state = %q", got)
	}
}

// A skipped step's own successor is skipped in turn, so one failure settles a
// whole chain rather than leaving its tail hanging.
func TestSkipCascades(t *testing.T) {
	p, r, productID, ctx := chainFixture(t)
	first, second := seedChain(t, p, productID, ctx)

	setState(t, p, first, "failed")
	if _, _, err := r.AdvanceSteps(ctx); err != nil {
		t.Fatalf("AdvanceSteps: %v", err)
	}
	// A third step depending on the second would be skipped on the next sweep.
	if _, err := p.DB().ExecContext(ctx,
		`UPDATE transfers SET state = 'waiting', depends_on_transfer_id = ? WHERE id = ?`,
		second, first); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := r.AdvanceSteps(ctx); err != nil || skipped != 1 {
		t.Fatalf("skipped %d, err %v", skipped, err)
	}
}

// A step whose predecessor is still running stays put - the sweep is idempotent
// and safe to run on every tick.
func TestUnsettledPredecessorChangesNothing(t *testing.T) {
	p, r, productID, ctx := chainFixture(t)
	_, second := seedChain(t, p, productID, ctx)

	released, skipped, err := r.AdvanceSteps(ctx)
	if err != nil {
		t.Fatalf("AdvanceSteps: %v", err)
	}
	if released != 0 || skipped != 0 {
		t.Fatalf("released %d, skipped %d", released, skipped)
	}
	if got := stateOf(t, p, second); got != "waiting" {
		t.Fatalf("state = %q", got)
	}
}
