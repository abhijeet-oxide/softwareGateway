package discovery

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// TestInspectFillsInWhatDiscoveryListed is the round trip the two-stage design
// rests on: discovery records a package cheaply, inspect completes it, and the
// completed record is what a transfer would move.
func TestInspectFillsInWhatDiscoveryListed(t *testing.T) {
	h := newHarness(t, baseDoc)

	shared := fakeregistry.NewLayer("shared-base")
	amd := h.reg.AddImage(testRepoPath, "", shared, fakeregistry.NewLayer("amd"))
	arm := h.reg.AddImage(testRepoPath, "", shared, fakeregistry.NewLayer("arm"))
	h.reg.AddIndex(testRepoPath, "v1.0.0",
		[]string{amd, arm}, []string{"linux/amd64", "linux/arm64"})

	if res := h.scan(); res.New != 1 {
		t.Fatalf("expected 1 new package, got %d", res.New)
	}

	// After discovery: the index fetched, its two children listed, size unknown.
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE raw IS NULL`); got != 2 {
		t.Fatalf("expected 2 listed-but-unfetched artifacts, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM packages WHERE total_bytes IS NULL`); got != 1 {
		t.Fatalf("expected the size to be unknown after discovery, got %d rows", got)
	}

	pkg, err := h.packages.GetPackage(t.Context(), "vendor-a", "v1.0.0")
	if err != nil {
		t.Fatalf("get package: %v", err)
	}

	client, err := h.scanner.clientFor(testRepoPath)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	res, err := InspectPackage(t.Context(), h.packages, pkg, client, 4)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	// Two children fetched; the index itself was already held but is re-read as
	// part of the walk, so three requests in total.
	if res.Fetched != 3 {
		t.Errorf("fetched %d manifests, want 3 (the index and its two children)", res.Fetched)
	}
	if res.AlreadyExpanded {
		t.Error("the package was not already expanded")
	}

	// Now nothing is merely listed, and the size is real.
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts WHERE raw IS NULL`); got != 0 {
		t.Errorf("expected every artifact to carry bytes after inspect, %d still do not", got)
	}
	// Two configs and three distinct layers: the shared base counts once.
	if res.Blobs != 5 {
		t.Errorf("got %d blobs, want 5 — the shared base layer counted once", res.Blobs)
	}
	if res.Package.TotalBytes == nil || *res.Package.TotalBytes <= 0 {
		t.Errorf("expected a measured size, got %v", res.Package.TotalBytes)
	}
	if res.Package.BlobCount == nil || *res.Package.BlobCount != 5 {
		t.Errorf("expected blob_count 5 on the row, got %v", res.Package.BlobCount)
	}

	// Idempotent, and — the part that matters — FREE. The tree under a digest
	// cannot change, so a second inspection must read the record rather than
	// walk the registry again. A transfer that follows an inspect gets the same
	// guarantee: the walk happens once, whoever asks first.
	before := h.reg.ManifestCalls.Load()
	again, err := InspectPackage(t.Context(), h.packages, pkg, client, 4)
	if err != nil {
		t.Fatalf("re-inspect: %v", err)
	}
	if got := h.reg.ManifestCalls.Load() - before; got != 0 {
		t.Errorf("a second inspection made %d registry calls; the tree is already recorded", got)
	}
	if !again.AlreadyExpanded {
		t.Error("a second inspection must report that nothing needed fetching")
	}
	if again.Fetched != 0 {
		t.Errorf("a second inspection fetched %d manifests, want 0", again.Fetched)
	}
	if again.Blobs != res.Blobs || *again.Package.TotalBytes != *res.Package.TotalBytes {
		t.Errorf("a second inspection produced different numbers: %+v vs %+v", again, res)
	}
	if got := h.count(`SELECT COUNT(*) FROM package_artifacts`); got != 3 {
		t.Errorf("a second inspection duplicated artifacts: %d rows, want 3", got)
	}
}

// TestStandardCreatedAnnotationBecomesPublishedAt covers the one annotation
// promoted to a column.
//
// org.opencontainers.image.created is defined by the OCI image spec under the
// reserved org.opencontainers. namespace — it is not any one vendor's key,
// which is what makes it safe to build on. It is also OPTIONAL, so nothing may
// require it.
func TestStandardCreatedAnnotationBecomesPublishedAt(t *testing.T) {
	h := newHarness(t, baseDoc)

	child := h.reg.AddImage(testRepoPath, "", fakeregistry.NewLayer("a"))
	h.reg.AddAnnotatedIndex(testRepoPath, "v1.0.0", []string{child}, []string{"linux/amd64"},
		map[string]string{
			"org.opencontainers.image.created": "2024-06-12T17:56:19Z",
			"org.opencontainers.image.vendor":  "Nokia",
			// A vendor's own key must survive without this project knowing it.
			"com.nokia.ncd.orb.rb.version": "23.8.1076",
		})

	if res := h.scan(); res.New != 1 {
		t.Fatalf("expected 1 new package, got %d", res.New)
	}

	pkg, err := h.packages.GetPackage(t.Context(), "vendor-a", "v1.0.0")
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	if pkg.PublishedAt == nil {
		t.Fatal("published_at was not recorded from org.opencontainers.image.created")
	}
	if *pkg.PublishedAt != "2024-06-12T17:56:19Z" {
		t.Errorf("published_at = %q, want the vendor's declared build time", *pkg.PublishedAt)
	}
	// Not conflated with our own observation.
	if pkg.DiscoveredAt == *pkg.PublishedAt {
		t.Error("published_at and discovered_at must stay distinct: one is the " +
			"vendor's claim, the other is when we saw it")
	}

	arts, err := h.packages.ListArtifacts(t.Context(), pkg.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if got := arts[0].Annotations["com.nokia.ncd.orb.rb.version"]; got != "23.8.1076" {
		t.Errorf("a vendor's own annotation was lost: got %q", got)
	}
	if got := arts[0].Annotations["org.opencontainers.image.vendor"]; got != "Nokia" {
		t.Errorf("standard annotations must be kept too: got %q", got)
	}
}

// A package with no created annotation must record nothing rather than
// inventing a date. The spec makes it optional, so absence is normal.
func TestMissingCreatedAnnotationLeavesPublishedAtUnset(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("a"))

	if res := h.scan(); res.New != 1 {
		t.Fatalf("expected 1 new package, got %d", res.New)
	}
	pkg, err := h.packages.GetPackage(t.Context(), "vendor-a", "v1.0.0")
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	if pkg.PublishedAt != nil {
		t.Errorf("published_at = %q; a missing annotation must stay missing", *pkg.PublishedAt)
	}
}

// The annotation is free text written by whoever published the artifact, so a
// value that is not a timestamp must be dropped rather than stored for
// something downstream to trip over.
func TestUnparseableCreatedAnnotationIsIgnored(t *testing.T) {
	h := newHarness(t, baseDoc)

	child := h.reg.AddImage(testRepoPath, "", fakeregistry.NewLayer("a"))
	h.reg.AddAnnotatedIndex(testRepoPath, "v1.0.0", []string{child}, nil,
		map[string]string{"org.opencontainers.image.created": "last Tuesday"})

	if res := h.scan(); res.New != 1 {
		t.Fatalf("expected 1 new package, got %d", res.New)
	}
	pkg, err := h.packages.GetPackage(t.Context(), "vendor-a", "v1.0.0")
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	if pkg.PublishedAt != nil {
		t.Errorf("published_at = %q; an unparseable date must be dropped", *pkg.PublishedAt)
	}
}
