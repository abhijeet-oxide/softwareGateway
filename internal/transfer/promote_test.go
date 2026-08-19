package transfer_test

import (
	"context"
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
