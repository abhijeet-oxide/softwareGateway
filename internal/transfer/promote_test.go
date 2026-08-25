package transfer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// Vendor -> lab -> production, through the expander rather than around it.
//
// Promotion is the same engine with a target as its origin, and the one thing
// it must get right that replication does not is the PATH. At lab the bundle
// sits beneath lab's prefix; production has a prefix of its own; and what has
// to arrive there is the vendor's structure under production's prefix - not
// lab's prefix nested inside production's, which is what a naive copy of the
// origin path produces.

// testResolver is the expander's two answers, without a config loader.
type testResolver struct {
	packages *store.Packages
}

func (r testResolver) Reader(
	ctx context.Context, repoID int64, repositoryPath string,
) (registry.ManifestReader, error) {
	endpoints, err := r.packages.HydrateEndpoints(ctx, []int64{repoID})
	if err != nil {
		return nil, err
	}
	e := endpoints[repoID]
	if repositoryPath == "" {
		repositoryPath = e.Repository
	}
	return generic.New(generic.Config{
		Registry: e.Registry, Repository: repositoryPath, PlainHTTP: true,
	})
}

func (testResolver) Related(
	context.Context, string, store.PackageRow,
) ([]vendors.Related, error) {
	return nil, nil
}

func TestPromotionReproducesTheStructureUnderTheNewPrefix(t *testing.T) {
	prod := fakeregistry.New()
	t.Cleanup(prod.Close)

	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")

	// Hop one: the vendor to lab, the path already proven elsewhere.
	s.plan(pkg, "to-lab")
	if got := s.drain("worker-1", 4); got.Failed != 0 {
		t.Fatalf("replication to lab failed: %v", got.LastError)
	}

	// Hop two: lab to production, driven through the expander so the read
	// path, the relative path and the re-basing are all under test rather
	// than supplied by the test.
	const prodBase = "nokia-prod"
	prodID := s.repo("target", prod.Host(), prodBase)
	s.openTransfer("to-prod", pkg, "promote", s.targetID, prodID)

	expander := transfer.NewExpander(
		s.packages,
		transfer.NewPlanner(s.packages, 4, testLogger(t)),
		testResolver{packages: s.packages},
		0, testLogger(t),
	)
	if _, err := expander.Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}

	if got := s.drainInto("worker-2", 4, prod); got.Failed != 0 {
		t.Fatalf("promotion failed: %v", got.LastError)
	}
	if state := s.transferState("to-prod"); state != "succeeded" {
		t.Fatalf("promotion is %q, want succeeded", state)
	}

	// The bundle, under PRODUCTION's prefix and the vendor's own path. If the
	// origin's path had been copied rather than re-based this would be
	// nokia-prod/nokia-lab/orbs/CFX-5000-k8s.
	wantRoot := prodBase + "/" + sourcePath
	if got := prod.TagDigest(wantRoot, "orb_23.8.1076"); got != pkg.ManifestDigest {
		t.Fatalf("%s:orb_23.8.1076 resolves to %q, want %s", wantRoot, got, pkg.ManifestDigest)
	}

	// And every component, still under the names the vendor gave it - those
	// came from annotations inside manifests copied verbatim on the first hop,
	// so the second hop reproduces them without ever having seen the vendor.
	for _, c := range components {
		want := prodBase + "/" + sourcePath
		if c.destination != targetPath {
			// A component the bundle named in its own repository.
			want = prodBase + trimPrefix(c.destination, targetBase)
		}
		if got := prod.TagDigest(want, c.tag); got != c.digest {
			t.Errorf("%s:%s resolves to %q, want %s at %s", want, c.tag, got, c.digest, want)
		}
	}
}

// Promotion within one registry should relocate rather than move bytes. This
// is why a 45 GB promotion is normally seconds.
func TestPromotionWithinOneRegistryMovesNoBytes(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "to-lab")
	if got := s.drain("worker-1", 4); got.Failed != 0 {
		t.Fatalf("replication to lab failed: %v", got.LastError)
	}

	// Production on the SAME registry as lab, which is the common deployment.
	prodID := s.repo("target", s.dst.Host(), "nokia-prod")
	s.openTransfer("to-prod", pkg, "promote", s.targetID, prodID)

	expander := transfer.NewExpander(
		s.packages,
		transfer.NewPlanner(s.packages, 4, testLogger(t)),
		testResolver{packages: s.packages},
		0, testLogger(t),
	)
	if _, err := expander.Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}

	before := s.dst.UploadedBytes.Load()
	got := s.drainInto("worker-2", 4, s.dst)
	if got.Failed != 0 {
		t.Fatalf("promotion failed: %v", got.LastError)
	}

	if moved := s.dst.UploadedBytes.Load() - before; moved != 0 {
		t.Errorf("%d bytes were uploaded; a same-registry promotion should relocate them", moved)
	}
	if got.SkipReason[transfer.SkipMounted] == 0 {
		t.Error("no blob was mounted: within one registry every blob should relocate server-side")
	}
}

func trimPrefix(path, prefix string) string {
	if len(path) > len(prefix) && path[:len(prefix)] == prefix {
		return path[len(prefix):]
	}
	return path
}

// ---------------------------------------------------------------------------
// Native promotion: the hop a registry carries out itself.
// ---------------------------------------------------------------------------

// claimingPromotion is a plugin that says yes, without being one.
//
// The seam is what keeps internal/transfer free of JFrog, and this is the
// mechanical proof: the expander's whole promotion branch is exercised here
// with a two-line fake, because everything it knows about a promoter is the
// interface in promotion.go.
type claimingPromotion struct {
	claims bool
	// seen is the hop it was asked about, so the test can assert what the
	// expander derived rather than what it was told.
	seen transfer.PromotionHop
}

func (c *claimingPromotion) Claim(
	_ context.Context, hop transfer.PromotionHop,
) (transfer.PromotionClaim, error) {
	c.seen = hop
	if !c.claims {
		return transfer.PromotionClaim{Reason: "not mine"}, nil
	}
	return transfer.PromotionClaim{Promoter: "fake", Claimed: true, Reason: "mine"}, nil
}

// promotionSlice replicates a bundle to lab, then opens a promotion out of it.
func promotionSlice(t *testing.T) (*slice, store.PackageRow, int64) {
	t.Helper()

	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "to-lab")
	if got := s.drain("worker-1", 4); got.Failed != 0 {
		t.Fatalf("replication to lab failed: %v", got.LastError)
	}

	prodID := s.repo("target", s.dst.Host(), "nokia-prod")
	s.openTransfer("to-prod", pkg, "promote", s.targetID, prodID)
	return s, pkg, prodID
}

func expanderWith(t *testing.T, s *slice, p transfer.Promotion) *transfer.Expander {
	t.Helper()
	return transfer.NewExpander(
		s.packages,
		transfer.NewPlanner(s.packages, 4, testLogger(t)),
		testResolver{packages: s.packages},
		0, testLogger(t),
	).WithPromotion(p, store.NewPromotions(s.st))
}

func jobCount(t *testing.T, s *slice, transferID string) int {
	t.Helper()
	var n int
	if err := s.st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM jobs WHERE transfer_id = ?`, transferID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A claimed promotion creates NO JOBS. That is the entire point: our engine
// would relocate every blob by mount, which is already cheap and is still tens
// of thousands of round trips to move content the registry can move in one
// call per name.
func TestAClaimedPromotionPlansNoJobs(t *testing.T) {
	s, _, _ := promotionSlice(t)

	seam := &claimingPromotion{claims: true}
	if _, err := expanderWith(t, s, seam).Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}

	if state := s.transferState("to-prod"); state != "promoting" {
		t.Errorf("transfer is %q, want promoting", state)
	}
	if n := jobCount(t, s, "to-prod"); n != 0 {
		t.Errorf("%d jobs were planned; a claimed promotion moves nothing itself", n)
	}

	pm, err := store.NewPromotions(s.st).ForTransfer(t.Context(), "to-prod")
	if err != nil {
		t.Fatalf("the promotion was not recorded: %v", err)
	}
	if pm.Promoter != "fake" {
		t.Errorf("promoter is %q, want fake", pm.Promoter)
	}
	if pm.NamesTotal == 0 {
		t.Error("a promotion must record the names it publishes")
	}
}

// The names are RELATIVE to each end's base, so one value re-bases under
// either. Carrying lab's prefix would nest it inside production's - the same
// mistake TestPromotionReproducesTheStructureUnderTheNewPrefix guards on the
// copy path.
func TestTheNamesAPromotionPublishesCarryNoPrefix(t *testing.T) {
	s, pkg, _ := promotionSlice(t)

	seam := &claimingPromotion{claims: true}
	if _, err := expanderWith(t, s, seam).Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}

	if len(seam.seen.Names) == 0 {
		t.Fatal("the seam was asked about a hop with no names")
	}
	root := seam.seen.Names[0]
	if root.Repository != sourcePath {
		t.Errorf("the root is at %q, want the vendor's own path %q", root.Repository, sourcePath)
	}
	if root.Tag != pkg.Tag {
		t.Errorf("the root is tagged %q, want %q", root.Tag, pkg.Tag)
	}
	if root.Digest != pkg.ManifestDigest {
		t.Errorf("the root resolves to %q, want %q", root.Digest, pkg.ManifestDigest)
	}

	for _, n := range seam.seen.Names {
		if strings.HasPrefix(n.Repository, targetBase) {
			t.Errorf("%q carries the origin's prefix; it must be relative", n.Repository)
		}
		if n.Tag == "" {
			t.Errorf("%q has no tag; a promotion publishes NAMES", n.Repository)
		}
	}
}

// Nothing claiming is the ordinary answer, and it must leave the copy path
// exactly as it was.
func TestAnUnclaimedPromotionIsPlannedAsACopy(t *testing.T) {
	s, _, _ := promotionSlice(t)

	if _, err := expanderWith(t, s, &claimingPromotion{claims: false}).Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}

	if state := s.transferState("to-prod"); state == "promoting" {
		t.Fatal("nothing claimed, so the transfer must be planned as a copy")
	}
	if n := jobCount(t, s, "to-prod"); n == 0 {
		t.Error("a copied promotion must have jobs")
	}
}

// A REPLICATION is never offered to the promoters, whatever they would say. A
// vendor's registry is not somewhere a target can relocate from.
func TestAReplicationIsNeverOfferedToThePromoters(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "to-lab")

	seam := &claimingPromotion{claims: true}
	if _, err := expanderWith(t, s, seam).Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}

	if seam.seen.TransferID != "" {
		t.Fatalf("a replication was offered to the promoters as %q", seam.seen.TransferID)
	}
	if state := s.transferState("to-lab"); state == "promoting" {
		t.Fatal("a replication must never enter `promoting`")
	}
}
