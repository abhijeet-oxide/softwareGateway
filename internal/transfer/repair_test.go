package transfer_test

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// A destination that lies about holding a blob, and the repair that gets past
// it.
//
// # What this reproduces
//
// Artifactory answers HEAD on a blob from a checksum index spanning the whole
// Artifactory repository rather than the image path the request named, so a
// blob present under any path reads as present under all of them. The blob fast
// path believes it, skips the upload and records a placement. The truth arrives
// at manifest push, which links per path and cannot:
//
//	HTTP 400: manifest invalid: map[description:Failed to copy blob sha256:… to
//	orbs/cfx-5000-…/sha256:…/sha256__dc82908b11cf…]
//
// A 63.7 GiB bundle moved every byte, reported every blob job successful, and
// delivered nothing: every manifest failed that way, eight times each, and then
// permanently.
//
// Two separate defects made that possible and both are covered here — the
// classification, which read MANIFEST_INVALID as "these bytes are wrong and
// always will be", and the missing repair, which docs/design/11 §2.5 specified
// and nothing implemented.

func TestABundleSurvivesADestinationThatLiesAboutItsBlobs(t *testing.T) {
	s := newSliceQuirky(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "t1")

	// Attempts DO fail here, and that is the mechanism rather than a defect:
	// the rejection is how the destination admits the blob is not where it
	// said, and the repair is what happens next. What must not survive is a
	// terminally failed job.
	got := s.drain("worker-1", 8)
	if got.Failed == 0 {
		t.Fatal("no manifest push was ever rejected; the fixture is not reproducing " +
			"the bug and this test would pass with the repair removed")
	}

	// The point of the whole exercise: the release is actually pullable.
	assertTagged(t, s, targetPath, "orb_23.8.1076", pkg.ManifestDigest)
	for _, c := range components {
		assertTagged(t, s, c.destination, c.tag, c.digest)
	}
	if s.transferState("t1") != "succeeded" {
		t.Errorf("transfer state = %q, want succeeded", s.transferState("t1"))
	}
}

// And it must have cost a repair rather than luck: the manifest push has to
// have been rejected at least once, or the fixture is not reproducing the bug
// and the test proves nothing.
func TestTheRepairIsWhatMakesTheBundleSurvive(t *testing.T) {
	s := newSliceQuirky(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "t1")

	s.drain("worker-1", 8)

	// A blob forced past the fast paths is the signature of the repair.
	forced := s.count(`SELECT COUNT(*) FROM jobs WHERE kind='blob' AND force_upload <> 0`)
	uploads := s.count(`SELECT COUNT(*) FROM jobs
	                     WHERE kind='blob' AND state='succeeded' AND bytes_transferred > 0`)
	if forced == 0 && uploads == 0 {
		t.Fatal("nothing was ever forced or uploaded; the destination never lied, " +
			"so this test would pass with the repair removed")
	}

	// The flag OUTLIVES the upload on purpose: it is the durable record that
	// this blob has already been through the repair. Clearing it would let a
	// destination that rejects the manifest for some other reason send the same
	// bytes up again on every attempt.
	if forced == 0 {
		t.Error("no blob kept its force_upload flag; the repair has no memory and " +
			"would re-upload on every rejection")
	}
}

// The classification half, stated on its own: MANIFEST_INVALID naming a blob is
// a missing blob, and a missing blob is repairable. Reading it as "these bytes
// are wrong" gives up on a transfer that is one upload away from finishing.
func TestARejectionNamingABlobIsTreatedAsRepairable(t *testing.T) {
	s := newSliceQuirky(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "t1")

	s.drain("worker-1", 8)

	// Nothing may be left terminally failed, and in particular nothing may be
	// left failed as `unsupported` — which is what the rejection was read as
	// before the detail was consulted.
	var class string
	err := s.st.DB().QueryRowContext(s.t.Context(),
		`SELECT COALESCE(last_error_class,'') FROM jobs
		  WHERE transfer_id='t1' AND state='failed' LIMIT 1`).Scan(&class)
	if err == nil {
		t.Fatalf("a job is terminally failed with class %q; the rejection was not "+
			"recognised as a missing blob", class)
	}
	if !strings.Contains(err.Error(), "no rows") {
		t.Fatal(err)
	}
}

// newSliceQuirky wires the slice against a destination with Artifactory's
// behaviour. The SOURCE stays well behaved: the vendor is not the problem.
func newSliceQuirky(t *testing.T) *slice {
	t.Helper()

	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New(fakeregistry.WithArtifactoryQuirks())
	t.Cleanup(dst.Close)

	return newSliceOn(t, src, dst)
}
