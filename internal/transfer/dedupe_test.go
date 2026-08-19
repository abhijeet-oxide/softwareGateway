package transfer_test

import (
	"encoding/json"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// What is NOT sent, proven rather than asserted.
//
// Every claim in this file was made in prose first - "the second transfer is
// nearly free", "a component appearing three times was sent once" - and prose
// is exactly what content addressing makes impossible to check by eye. A blob
// uploaded twice to the same place is byte-identical to one uploaded once, so
// every total, every size and every state agrees with both stories. Only a
// LEDGER of arrivals tells them apart, which is why the fake registry keeps
// one.

// The plainest case, and the one an operator hits first: run the same transfer
// again.
func TestTransferringTheSameBundleTwiceMovesNothingTheSecondTime(t *testing.T) {
	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "first")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first transfer failed: %v", got.LastError)
	}

	before := s.dst.UploadedBytes.Load()
	vendorBefore := s.src.ServedBlobs.Load()

	s.plan(pkg, "second")
	second := s.drain("worker-1", 8)
	if second.Failed != 0 {
		t.Fatalf("the second transfer failed: %v", second.LastError)
	}

	if moved := s.dst.UploadedBytes.Load() - before; moved != 0 {
		t.Errorf("the second transfer uploaded %d bytes of content already there", moved)
	}
	if fetched := s.src.ServedBlobs.Load() - vendorBefore; fetched != 0 {
		t.Errorf("the second transfer fetched %d blob(s) from the vendor again", fetched)
	}
	if second.BytesMoved != 0 {
		t.Errorf("the engine reported moving %d bytes on a repeat transfer", second.BytesMoved)
	}

	// And it is a real transfer, not a no-op that skipped the point: the tags
	// still resolve.
	assertTagged(t, s, targetPath, "orb_23.8.1076", pkg.ManifestDigest)
	for _, c := range components {
		assertTagged(t, s, c.destination, c.tag, c.digest)
	}
}

// Two requests for the same bundle, planned before either has run. The
// placement cache cannot help here - nothing is at the destination yet when the
// second is planned - so this is the concurrent-duplicate path, not the dedupe
// path, and it is the one that would quietly send everything twice.
func TestTwoRequestsForOneBundleDoNotSendItTwice(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "request-a")
	s.plan(pkg, "request-b")

	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("draining two requests failed: %v", got.LastError)
	}

	if over := s.dst.Overwrites(); len(over) != 0 {
		t.Errorf("content arrived more than once at some destination:\n  %v", over)
	}
	// The vendor is the honest witness. Uploads alone cannot show a double
	// fetch: two fetches and two uploads of identical bytes look, at the
	// destination, exactly like one of each.
	distinct := s.distinctSourceBlobs()
	if fetched := s.src.ServedBlobs.Load(); fetched > int64(distinct) {
		t.Errorf("the vendor served %d blob GETs for %d distinct blobs; the second "+
			"request re-fetched what the first was already moving", fetched, distinct)
	}
}

// THE CLAIM THAT NEEDED PROVING. A component of a bundle appears three times in
// the destination browser - under its tag, under its digest, and inside the
// bundle - and the answer to "was it sent three times?" is no, but no total
// shows that. The first two are one OCI repository and cost nothing; the third
// is a second repository, which the specification requires to hold its own blob
// links, and that arrival is a MOUNT.
func TestAComponentAppearingInThreePlacesIsSentOnce(t *testing.T) {
	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "t1")

	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("transfer failed: %v", got.LastError)
	}

	nginx := components[0]
	container := targetPath // the bundle's own repository
	published := nginx.destination

	if container == published {
		t.Fatal("the fixture puts both copies in one repository; the test would prove nothing")
	}

	for _, digest := range s.blobsOfManifest(s.src, sourcePath, nginx.digest) {
		uploads := s.dst.BlobUploads(container, digest) + s.dst.BlobUploads(published, digest)
		mounts := s.dst.BlobMounts(container, digest) + s.dst.BlobMounts(published, digest)

		// Exactly one upload: the bytes crossed the network once.
		if uploads != 1 {
			t.Errorf("blob %s was uploaded %d times, want once", digest[:19], uploads)
		}
		// And exactly one mount: the second repository got it without bytes.
		if mounts != 1 {
			t.Errorf("blob %s was mounted %d times, want once - the second repository "+
				"should be a relocation, not a transfer", digest[:19], mounts)
		}
	}

	// Nothing landed twice in the same place. An overwrite is invisible to
	// every other measure, because the second write is byte-identical.
	if over := s.dst.Overwrites(); len(over) != 0 {
		t.Errorf("content arrived more than once at some destination:\n  %v", over)
	}

	// And the destination really does resolve all three references.
	assertTagged(t, s, published, nginx.tag, nginx.digest)
	if !s.dst.BlobExists(container, s.blobsOfManifest(s.src, sourcePath, nginx.digest)[0]) {
		t.Error("the bundle repository does not hold the component's layers, so the " +
			"index cannot be pulled from it")
	}
}

// Two releases of one product, sharing a base layer and one whole component.
// The second must move only what is genuinely new.
func TestASecondReleaseTransfersOnlyTheDelta(t *testing.T) {
	s := newSlice(t)

	first := seedRelease(t, s, "orb_23.8.1076", "a")
	s.plan(first.pkg, "release-a")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first release failed: %v", got.LastError)
	}

	uploadedForA := s.dst.UploadedBytes.Load()
	fetchedForA := s.src.ServedBytes.Load()

	second := seedRelease(t, s, "orb_23.9.1100", "b")
	s.plan(second.pkg, "release-b")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the second release failed: %v", got.LastError)
	}

	uploadedForB := s.dst.UploadedBytes.Load() - uploadedForA
	fetchedForB := s.src.ServedBytes.Load() - fetchedForA

	// The shared base layer and the entire unchanged component are common to
	// both releases, so the second must be materially cheaper. Asserted as a
	// fraction rather than an absolute, because the fixture's sizes are
	// arbitrary and the RELATIONSHIP is the thing under test.
	if uploadedForB >= uploadedForA {
		t.Errorf("the second release uploaded %d bytes against the first's %d; "+
			"nothing was reused", uploadedForB, uploadedForA)
	}
	if fetchedForB >= fetchedForA {
		t.Errorf("the second release fetched %d bytes from the vendor against the "+
			"first's %d; the delta was not recognised", fetchedForB, fetchedForA)
	}

	// The specific claim: content shared between the releases was not sent
	// again, at all.
	for _, digest := range second.shared {
		if n := s.dst.BlobUploads(targetPath, digest); n != 1 {
			t.Errorf("shared blob %s was uploaded %d times across two releases, want once",
				digest[:19], n)
		}
	}

	// And the second release is complete despite moving almost nothing.
	assertTagged(t, s, targetPath, "orb_23.9.1100", second.pkg.ManifestDigest)
	for _, c := range second.components {
		assertTagged(t, s, c.destination, c.tag, c.digest)
	}
}

// Content removed at the destination - a garbage collector, an operator with a
// delete button - must come back, and nothing else with it.
func TestOnlyWhatWasRemovedFromTheTargetIsSentAgain(t *testing.T) {
	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")

	s.plan(pkg, "first")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first transfer failed: %v", got.LastError)
	}

	// Remove one component's layers from the destination, and the placement
	// records that say they are there - which is what a garbage collector
	// leaves behind, and the case the whole optimistic cache rests on.
	victim := components[1]
	gone := s.blobsOfManifest(s.src, sourcePath, victim.digest)
	for _, digest := range gone {
		s.dst.RemoveBlob(targetPath, digest)
		s.dst.RemoveBlob(victim.destination, digest)
	}
	s.forgetPlacements(gone)

	before := s.dst.UploadedBlobs.Load()

	s.plan(pkg, "second")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the repair transfer failed: %v", got.LastError)
	}

	// Only the missing blobs moved. Every other blob in the bundle is still at
	// the destination and must not have been touched.
	uploaded := s.dst.UploadedBlobs.Load() - before
	if uploaded == 0 {
		t.Fatal("nothing was re-sent for content that had been deleted")
	}
	if uploaded > int64(len(gone)) {
		t.Errorf("re-sent %d blob(s) to replace %d that were deleted; the rest of the "+
			"bundle was re-sent as well", uploaded, len(gone))
	}
	for _, digest := range gone {
		if !s.dst.BlobExists(targetPath, digest) {
			t.Errorf("blob %s was not restored", digest[:19])
		}
	}
}

// The claim that a delta transfer is FAST, rather than merely small.
//
// Bytes are the honest measure and duration is not - a test machine's clock
// says nothing about a 917 ms link - so what is asserted here is the quantity
// that duration is proportional to: how much crossed the network. On a path
// where every stream is bounded by window ÷ RTT, time is bytes ÷ a constant,
// and the ratio is the part that carries over.
func TestASecondReleaseIsProportionallyCheaper(t *testing.T) {
	s := newSlice(t)

	first := seedRelease(t, s, "orb_23.8.1076", "a")
	s.plan(first.pkg, "release-a")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the first release failed: %v", got.LastError)
	}
	firstMoved := s.dst.UploadedBytes.Load()

	second := seedRelease(t, s, "orb_23.9.1100", "b")
	s.plan(second.pkg, "release-b")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the second release failed: %v", got.LastError)
	}
	secondMoved := s.dst.UploadedBytes.Load() - firstMoved

	if firstMoved == 0 {
		t.Fatal("the first release moved nothing; the comparison is meaningless")
	}
	// Half is a deliberately loose bound. The fixture's proportions are
	// arbitrary and the point is the ORDER of the saving, not its exact size -
	// a change that quietly stopped reusing anything would put this ratio at
	// or above one and fail, which is what the assertion is for.
	if secondMoved*2 >= firstMoved {
		t.Errorf("the second release moved %d bytes against the first's %d; "+
			"a release sharing a base layer and an unchanged component should "+
			"cost a fraction of the first", secondMoved, firstMoved)
	}
	t.Logf("first release %d bytes, second %d bytes (%.0f%% of the first)",
		firstMoved, secondMoved, 100*float64(secondMoved)/float64(firstMoved))
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

type release struct {
	pkg        store.PackageRow
	components []orbComponent
	// shared are the blob digests this release has in common with every other,
	// and therefore the ones a second transfer must not send.
	shared []string
}

// seedRelease publishes one release of a product line.
//
// `variant` changes the content that genuinely differs between releases and
// nothing else, so what is shared is shared BY DIGEST rather than by the test
// asserting it is. The base layer and the chart are byte-identical across
// releases; the payload and the configuration are not.
func seedRelease(t *testing.T, s *slice, tag, variant string) release {
	t.Helper()

	// Identical in every release: same bytes, same digest, and therefore
	// already at the destination the second time.
	base := fakeregistry.NewLayer("a base layer every release shares")
	chartLayer := fakeregistry.NewLayer("a chart that did not change this release")
	chartLayer.MediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	nginx := s.src.AddImage(sourcePath, "", base, fakeregistry.NewLayer("nginx payload "+variant))
	chart := s.src.AddImage(sourcePath, "", chartLayer)
	custo := s.addGeneric(sourcePath, map[string]string{
		"CONFIGURATION/example_parameters.json": `{"replicas":3,"release":"` + variant + `"}`,
	})

	components := []orbComponent{
		{refName: sourcePath + "/nginx:" + tag,
			destination: targetBase + "/" + sourcePath + "/nginx", tag: tag, digest: nginx},
		{refName: sourcePath + "/charts/cfx:23.8.1076",
			destination: targetBase + "/" + sourcePath + "/charts/cfx",
			tag:         "23.8.1076", digest: chart},
		{refName: sourcePath + ":generic_custo_" + tag,
			destination: targetPath, tag: "generic_custo_" + tag, digest: custo},
	}

	children := make([]map[string]any, 0, len(components))
	for _, c := range components {
		children = append(children, map[string]any{
			"mediaType":   registry.MediaTypeOCIManifest,
			"digest":      c.digest,
			"size":        100,
			"annotations": map[string]string{registry.AnnotationRefName: c.refName},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex, "manifests": children,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := s.src.AddManifest(sourcePath, tag, raw, registry.MediaTypeOCIIndex)

	return release{
		pkg:        s.recordPackage(tag, root, len(components)+1),
		components: components,
		shared:     append([]string{base.Digest}, s.blobsOfManifest(s.src, sourcePath, chart)...),
	}
}

// blobsOfManifest reads the layer and config digests a manifest references,
// from the registry rather than from the fixture's own variables - so the test
// is checking what was actually published.
func (s *slice) blobsOfManifest(reg *fakeregistry.Registry, repoPath, digest string) []string {
	s.t.Helper()

	raw := reg.Manifest(repoPath, digest)
	if raw == nil {
		s.t.Fatalf("manifest %s is not in %s", digest, repoPath)
	}

	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		s.t.Fatal(err)
	}

	out := make([]string, 0, len(m.Layers)+1)
	if m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	return out
}

// distinctSourceBlobs counts the blobs the vendor holds for this package, which
// is the ceiling on how many GETs a correct transfer may make.
func (s *slice) distinctSourceBlobs() int {
	s.t.Helper()

	var n int
	if err := s.st.DB().QueryRowContext(s.t.Context(),
		`SELECT COUNT(DISTINCT digest) FROM blobs`).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// forgetPlacements withdraws our record that these blobs are at the
// destination, as the TTL would in time.
func (s *slice) forgetPlacements(digests []string) {
	s.t.Helper()

	for _, d := range digests {
		if _, err := s.st.DB().ExecContext(s.t.Context(),
			`DELETE FROM blob_placements WHERE digest = ?`, d); err != nil {
			s.t.Fatal(err)
		}
	}
}
