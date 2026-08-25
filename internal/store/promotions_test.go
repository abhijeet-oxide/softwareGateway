package store

import (
	"context"
	"errors"
	"testing"
)

// What this store has to get right is RESUMABILITY. A Coordinator killed
// half way through a 260-name promotion must come back, find what is left,
// and finish - not restart, and not report success on a release that is half
// there.

func promotionFixture(t *testing.T) (*Promotions, Store, context.Context) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()

	mustExec(t, s.DB(), `INSERT INTO products (id,name,config_hash,config) VALUES (1,'nokia','h','{}')`)
	mustExec(t, s.DB(), `INSERT INTO repositories (id,product_id,role,name,registry_host,repository_path)
	                     VALUES (1,1,'target','lab','acme.jfrog.io','docker-lab'),
	                            (2,1,'target','production','acme.jfrog.io','docker-prod')`)
	mustExec(t, s.DB(), `INSERT INTO packages (id,product_id,source_repo_id,tag,manifest_digest,media_type)
	                     VALUES (1,1,1,'v1','sha256:aa','application/json')`)
	mustExec(t, s.DB(), `INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
	                     VALUES ('r1',1,1,'promote',1,'k1')`)
	mustExec(t, s.DB(), `INSERT INTO transfers (id,request_id,package_id,source_repo_id,target_repo_id,state)
	                     VALUES ('t1','r1',1,1,2,'planning')`)

	return NewPromotions(s), s, ctx
}

func threeNames() []PromotionName {
	return []PromotionName{
		{Repository: "orbs/cfx", Tag: "v1", Digest: "sha256:aa"},
		{Repository: "orbs/cfx/nginx", Tag: "1.2.3", Digest: "sha256:bb"},
		{Repository: "orbs/cfx/redis", Tag: "7.0", Digest: "sha256:cc"},
	}
}

func transferState(t *testing.T, s Store, id string) string {
	t.Helper()
	var state string
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT state FROM transfers WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatalf("read transfer state: %v", err)
	}
	return state
}

// Opening a promotion and moving its transfer are ONE fact. A transfer left in
// `planning` with a promotion row would be run twice; a transfer in
// `promoting` with no row would be invisible to the runner forever.
func TestOpenMovesTheTransferAndRecordsTheNames(t *testing.T) {
	p, s, ctx := promotionFixture(t)

	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := transferState(t, s, "t1"); got != "promoting" {
		t.Errorf("transfer is %q, want promoting", got)
	}

	pm, err := p.ForTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("ForTransfer: %v", err)
	}
	if pm.Promoter != "jfrog" || pm.State != "requested" || pm.NamesTotal != 3 {
		t.Errorf("promotion is %+v", pm)
	}

	var strategy string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT strategy FROM transfers WHERE id = 't1'`).Scan(&strategy); err != nil {
		t.Fatal(err)
	}
	// A settled transfer with no jobs and no bytes is otherwise
	// indistinguishable from one that failed to plan.
	if strategy != "relocate" {
		t.Errorf("strategy is %q, want relocate", strategy)
	}
}

// A re-expansion - which is what a Coordinator killed between opening and
// settling produces - must find the existing promotion rather than append a
// second set of names.
func TestOpenIsIdempotent(t *testing.T) {
	p, _, ctx := promotionFixture(t)

	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatalf("Open again: %v", err)
	}

	pm, err := p.ForTransfer(ctx, "t1")
	if err != nil {
		t.Fatalf("ForTransfer: %v", err)
	}
	if pm.NamesTotal != 3 {
		t.Errorf("namesTotal is %d, want 3 - the names were recorded twice", pm.NamesTotal)
	}
	names, err := p.AllNames(ctx, pm.ID)
	if err != nil {
		t.Fatalf("AllNames: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("%d name rows, want 3", len(names))
	}
}

func TestOpenRefusesAPromotionWithNoNames(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", nil); err == nil {
		t.Fatal("a promotion that publishes nothing must be refused")
	}
}

// The whole point of the child table: a resumed promotion re-issues only what
// is left, in the order the tree gave, so an interrupted one has published a
// consistent prefix rather than an arbitrary subset.
func TestAResumedPromotionOnlyHasWhatIsLeft(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	pm, err := p.ClaimPromotion(ctx, "coordinator-1")
	if err != nil {
		t.Fatalf("ClaimPromotion: %v", err)
	}

	if err := p.NamePromoted(ctx, pm.ID, 0); err != nil {
		t.Fatalf("NamePromoted: %v", err)
	}

	pending, err := p.PendingNames(ctx, pm.ID)
	if err != nil {
		t.Fatalf("PendingNames: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("%d names left, want 2", len(pending))
	}
	if pending[0].Position != 1 || pending[0].Tag != "1.2.3" {
		t.Errorf("resumed at %+v, want position 1", pending[0])
	}

	after, err := p.ForTransfer(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if after.NamesDone != 1 {
		t.Errorf("namesDone is %d, want 1", after.NamesDone)
	}
}

// Counted from the rows the statement actually changed, so a name recorded
// twice cannot push the numerator past the denominator.
func TestRecordingANameTwiceDoesNotDoubleCount(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}
	pm, err := p.ClaimPromotion(ctx, "coordinator-1")
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := p.NamePromoted(ctx, pm.ID, 0); err != nil {
			t.Fatalf("NamePromoted: %v", err)
		}
	}
	after, err := p.ForTransfer(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if after.NamesDone != 1 {
		t.Errorf("namesDone is %d, want 1", after.NamesDone)
	}
}

// Two Coordinators racing must produce ONE claim, or one release is promoted
// twice by two processes reporting different outcomes.
func TestOnlyOneCoordinatorClaimsAPromotion(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}

	if _, err := p.ClaimPromotion(ctx, "coordinator-1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The second finds a live claim - the heartbeat was just written - and is
	// told there is nothing to do rather than taking it.
	if _, err := p.ClaimPromotion(ctx, "coordinator-2"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("a live claim must not be taken again; got %v", err)
	}
}

// Nothing to do is the ordinary answer on almost every tick, and it must not
// read as a fault.
func TestNothingToRunIsNotAnError(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if _, err := p.ClaimPromotion(ctx, "coordinator-1"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("want ErrNoRecord, got %v", err)
	}
}

func TestSettleClosesThePromotionAndItsTransfer(t *testing.T) {
	p, s, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}
	pm, err := p.ClaimPromotion(ctx, "coordinator-1")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Settle(ctx, pm.ID, "succeeded", ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := transferState(t, s, "t1"); got != "succeeded" {
		t.Errorf("transfer is %q, want succeeded", got)
	}
}

// `diverged` belongs to a mirror following an upstream tag that moved. A
// promotion is a copy WE asked for between two repositories of one registry,
// so there is nothing for it to diverge from.
func TestThereIsNoDivergedOutcome(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}
	pm, err := p.ClaimPromotion(ctx, "coordinator-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Settle(ctx, pm.ID, "diverged", ""); err == nil {
		t.Fatal("`diverged` is not a promotion outcome and must be refused")
	}
}

// A failed promotion can be asked for again - which is what makes an
// interrupted Coordinator recoverable rather than a request somebody has to
// make twice.
func TestAFailedPromotionCanBeReopened(t *testing.T) {
	p, s, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}
	pm, err := p.ClaimPromotion(ctx, "coordinator-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Settle(ctx, pm.ID, "failed", "Artifactory said no"); err != nil {
		t.Fatal(err)
	}
	if got := transferState(t, s, "t1"); got != "failed" {
		t.Fatalf("transfer is %q, want failed", got)
	}

	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := transferState(t, s, "t1"); got != "promoting" {
		t.Errorf("transfer is %q, want promoting", got)
	}
	if _, err := p.ClaimPromotion(ctx, "coordinator-1"); err != nil {
		t.Fatalf("a reopened promotion must be claimable: %v", err)
	}
}

// A promotion whose transfer somebody stopped must not be picked up.
func TestAStoppedTransferIsNotPromoted(t *testing.T) {
	p, s, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.DB(), `UPDATE transfers SET state = 'cancelled' WHERE id = 't1'`)

	if _, err := p.ClaimPromotion(ctx, "coordinator-1"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("a cancelled transfer must not be promoted; got %v", err)
	}
}

// The claim carries the hop, so the runner needs no second query - and it
// carries the transfer's OWN repository rows rather than current
// configuration, because a request's intent is durable.
func TestTheClaimCarriesTheHop(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if err := p.Open(ctx, "t1", "jfrog", threeNames()); err != nil {
		t.Fatal(err)
	}
	pm, err := p.ClaimPromotion(ctx, "coordinator-1")
	if err != nil {
		t.Fatal(err)
	}

	if pm.ProductName != "nokia" {
		t.Errorf("product is %q, want nokia", pm.ProductName)
	}
	if pm.OriginRepoID != 1 || pm.DestinationRepoID != 2 {
		t.Errorf("hop is %d -> %d, want 1 -> 2", pm.OriginRepoID, pm.DestinationRepoID)
	}
	if pm.PackageTag != "v1" || pm.PackageDigest != "sha256:aa" {
		t.Errorf("package is %s@%s", pm.PackageTag, pm.PackageDigest)
	}
	if pm.ClaimedBy != "coordinator-1" || pm.Attempts != 1 {
		t.Errorf("claim is %q attempt %d", pm.ClaimedBy, pm.Attempts)
	}
}

// A promotion nobody ever opened is not an error at any caller - it is every
// ordinary copy.
func TestATransferWithNoPromotionAnswersNoRecord(t *testing.T) {
	p, _, ctx := promotionFixture(t)
	if _, err := p.ForTransfer(ctx, "t1"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("want ErrNoRecord, got %v", err)
	}
}
