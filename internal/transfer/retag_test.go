package transfer_test

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// Tagging was the last step that wrote unconditionally.
//
// Every other step in a transfer asks first. A blob present at the destination
// is skipped; a manifest already stored under its digest is skipped; a blob
// held elsewhere on the destination registry is relocated rather than sent.
// Tagging did none of that — it PUT every tag on every attempt, including the
// tags it had applied itself on the previous run.
//
// That is invisible while it works, because rewriting a tag to the digest it
// already holds leaves the registry in the state it was already in. It stops
// being invisible when the destination declines: `PUT /v2/<repo>/manifests/
// <tag>` over an existing tag is an OVERWRITE, and permission to create is not
// permission to overwrite. Artifactory wants Delete on the permission target
// for it and answers 401 without one, so a credential that had published a
// release perfectly could not re-run the transfer over it — the run that
// should have been the cheapest was the only one that failed.

// The behaviour, stated without reference to any registry's permission model:
// a tag that already names this content is not written again.
func TestATagThatAlreadyNamesTheContentIsNotWrittenAgain(t *testing.T) {
	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "first")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first transfer failed: %v", got.LastError)
	}

	// The first run must have written them, or the second run's zero proves
	// nothing.
	if n := s.dst.TagWrites(targetPath, "orb_23.8.1076"); n != 1 {
		t.Fatalf("the bundle tag was written %d times on the first transfer, want 1", n)
	}

	s.plan(pkg, "second")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the second transfer failed: %v", got.LastError)
	}

	if n := s.dst.TagWrites(targetPath, "orb_23.8.1076"); n != 1 {
		t.Errorf("the bundle tag was written %d times across two transfers, want 1: "+
			"the second run rewrote a tag that already pointed at the same digest", n)
	}
	for _, c := range components {
		if n := s.dst.TagWrites(c.destination, c.tag); n != 1 {
			t.Errorf("%s:%s was written %d times across two transfers, want 1",
				c.destination, c.tag, n)
		}
	}

	// And the release is still whole. A shortcut that skips the write must not
	// skip the outcome.
	assertTagged(t, s, targetPath, "orb_23.8.1076", pkg.ManifestDigest)
	for _, c := range components {
		assertTagged(t, s, c.destination, c.tag, c.digest)
	}
}

// The consequence, against a destination that actually refuses.
//
// This is the deployment the field hit: a credential that may deploy but not
// overwrite. The first transfer creates every tag and succeeds. Without the
// check, the second fails on every tag it had already applied — and fails as
// `auth`, which is terminal, so it does not even retry.
func TestARerunSurvivesADestinationThatRefusesTagOverwrites(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New(fakeregistry.WithTagOverwriteDenied())
	t.Cleanup(dst.Close)

	s := newSliceOn(t, src, dst)
	pkg, components := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "first")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first transfer failed: %v", got.LastError)
	}

	s.plan(pkg, "second")
	second := s.drain("worker-1", 8)
	if second.Failed != 0 {
		t.Fatalf("re-running the transfer failed: %v", second.LastError)
	}
	if s.transferState("second") != "succeeded" {
		t.Errorf("re-run transfer state = %q, want succeeded", s.transferState("second"))
	}

	assertTagged(t, s, targetPath, "orb_23.8.1076", pkg.ManifestDigest)
	for _, c := range components {
		assertTagged(t, s, c.destination, c.tag, c.digest)
	}
}

// The refusal is real, and this is what proves it: with the tag already
// present, writing it again IS rejected. Without this the test above would
// pass against a destination that never objected to anything.
func TestTheRefusedOverwriteIsWhatTheRerunAvoids(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New(fakeregistry.WithTagOverwriteDenied())
	t.Cleanup(dst.Close)

	s := newSliceOn(t, src, dst)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "first")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first transfer failed: %v", got.LastError)
	}

	// Straight at the registry, bypassing the engine's check: the same request
	// the engine used to make.
	target := s.client(store.Endpoint{Registry: s.dst.Host(), Repository: targetPath})
	err := target.Tag(t.Context(), registry.Digest(pkg.ManifestDigest), "orb_23.8.1076")
	if err == nil {
		t.Fatal("the destination accepted a tag overwrite; the fixture is not " +
			"reproducing the refusal and the re-run test proves nothing")
	}
}
