package generic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

func writeRepo(t *testing.T, reg *fakeregistry.Registry, path string) *Repository {
	t.Helper()
	r, err := New(Config{Registry: reg.Host(), Repository: path, PlainHTTP: true})
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	return r
}

func TestPushAndFetchBlobRoundTrips(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	dst := writeRepo(t, reg, "mirror/suite")
	content := []byte("a helm chart tarball, or a config file, or an image layer - " +
		"the engine cannot tell and does not look")
	dgst := registry.NewDigestFromBytes(content)

	if err := dst.PushBlob(t.Context(), dgst, int64(len(content)), bytes.NewReader(content)); err != nil {
		t.Fatalf("push: %v", err)
	}

	rc, err := dst.FetchBlob(t.Context(), dgst)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("round trip changed the bytes:\n got %q\nwant %q", got, content)
	}
}

// The registry verifies on commit, and that is half the end-to-end integrity
// story: our reader-side verifier catches a source serving wrong bytes, and
// this catches corruption anywhere in our own path.
func TestRegistryRejectsAMismatchedDigest(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	dst := writeRepo(t, reg, "mirror/suite")
	claimed := registry.NewDigestFromBytes([]byte("what we said we would send"))
	actual := []byte("what we actually sent")

	err := dst.PushBlob(t.Context(), claimed, int64(len(actual)), bytes.NewReader(actual))
	if err == nil {
		t.Fatal("pushing content that does not match its digest must fail")
	}
	if reg.BlobExists("mirror/suite", claimed.String()) {
		t.Error("the mismatched blob was stored")
	}
}

// Pushing the same blob twice is a no-op, not an error. Content addressing is
// what makes a job safe to re-run after a lease expires, and a second push
// erroring would turn ordinary recovery into a failure.
func TestPushingAnExistingBlobSucceeds(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	dst := writeRepo(t, reg, "mirror/suite")
	content := []byte("idempotent")
	dgst := registry.NewDigestFromBytes(content)

	for i := range 2 {
		if err := dst.PushBlob(t.Context(), dgst, int64(len(content)), bytes.NewReader(content)); err != nil {
			t.Fatalf("push %d: %v", i+1, err)
		}
	}
}

// INVARIANT I9, and the one the whole deployment story rests on: the bytes
// pushed are the bytes fetched. Same bytes means the same digest, so a chart
// referencing an image by digest still resolves at the destination and only the
// registry host changes.
func TestManifestBytesAreNotMutated(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	src := writeRepo(t, reg, "vendor/suite")
	srcDigest := reg.AddImage("vendor/suite", "v1.0.0",
		fakeregistry.NewLayer("chart"), fakeregistry.NewLayer("config-file"))

	desc, raw, err := src.FetchManifest(t.Context(), srcDigest)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}

	// The blobs first, or the registry rejects the manifest - which is itself
	// the ordering invariant, enforced by the registry rather than by us.
	dst := writeRepo(t, reg, "mirror/suite")
	copyBlobsOf(t, reg, src, dst, raw)

	pushed, err := dst.PushManifest(t.Context(), "v1.0.0", desc.MediaType, raw)
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}

	if pushed.Digest != desc.Digest {
		t.Fatalf("digest changed in transit: %s -> %s", desc.Digest, pushed.Digest)
	}

	stored, ok := reg.ManifestBytes("mirror/suite", "v1.0.0")
	if !ok {
		t.Fatal("the manifest was not stored under its tag")
	}
	if !bytes.Equal(stored, raw) {
		t.Errorf("stored bytes differ from source bytes - any signature over this digest is now invalid\n"+
			" got %s\nwant %s", stored, raw)
	}
}

// The backstop that makes the optimistic placement cache safe: when our record
// of "already there" is wrong, the registry tells us.
func TestManifestPushFailsWhenABlobIsMissing(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	src := writeRepo(t, reg, "vendor/suite")
	srcDigest := reg.AddImage("vendor/suite", "v1.0.0", fakeregistry.NewLayer("layer"))
	desc, raw, err := src.FetchManifest(t.Context(), srcDigest)
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately no blobs at the destination.
	dst := writeRepo(t, reg, "mirror/suite")
	_, err = dst.PushManifest(t.Context(), "v1.0.0", desc.MediaType, raw)
	if err == nil {
		t.Fatal("a manifest whose blobs are absent must be refused")
	}
	if !strings.Contains(err.Error(), "BLOB_UNKNOWN") && !strings.Contains(err.Error(), "not present") {
		t.Errorf("the error should name the missing blob, got %v", err)
	}
	if reg.TagDigest("mirror/suite", "v1.0.0") != "" {
		t.Error("the tag was applied despite the push failing - a consumer would see a broken release")
	}
}

// Zero bytes over the wire, whatever the blob's size. This is the dominant
// optimisation for promotion within one registry.
func TestMountMovesNoBytes(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	layer := fakeregistry.NewLayer("a large layer")
	reg.AddImage("vendor/suite", "v1.0.0", layer)

	dst := writeRepo(t, reg, "mirror/suite")
	if err := dst.MountBlob(t.Context(), registry.Digest(layer.Digest), "vendor/suite"); err != nil {
		t.Fatalf("mount: %v", err)
	}

	if !reg.BlobExists("mirror/suite", layer.Digest) {
		t.Error("the blob is not at the destination")
	}
	if got := reg.UploadedBytes.Load(); got != 0 {
		t.Errorf("%d bytes were uploaded; a mount must move none", got)
	}
	if got := reg.MountedBlobs.Load(); got != 1 {
		t.Errorf("mounted %d blobs, want 1", got)
	}
}

// A registry declining the shortcut answers 202 with an ordinary upload
// session. That is a NORMAL outcome under the specification, support is uneven
// in the wild, and treating it as a failure would break replication against
// every registry that answers this way.
func TestDeclinedMountIsNotAFailure(t *testing.T) {
	reg := fakeregistry.New(fakeregistry.WithDeclinedMounts())
	t.Cleanup(reg.Close)

	layer := fakeregistry.NewLayer("a large layer")
	reg.AddImage("vendor/suite", "v1.0.0", layer)

	dst := writeRepo(t, reg, "mirror/suite")
	err := dst.MountBlob(t.Context(), registry.Digest(layer.Digest), "vendor/suite")

	if !errors.Is(err, registry.ErrMountUnsupported) {
		t.Fatalf("a declined mount must report ErrMountUnsupported so the caller "+
			"falls through to streaming, got %v", err)
	}
	// And crucially it is NOT ErrUnsupported, which callers treat as fatal.
	if errors.Is(err, registry.ErrNotFound) {
		t.Error("a declined mount must not look like a missing blob")
	}
}

// A tag is applied only once its content exists, which is what makes an
// interrupted transfer safe: a consumer sees the old tag or the complete new
// one, never a half-written one.
func TestTagPointsAtExistingContent(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	src := writeRepo(t, reg, "vendor/suite")
	srcDigest := reg.AddImage("vendor/suite", "v1.0.0", fakeregistry.NewLayer("l"))
	desc, raw, err := src.FetchManifest(t.Context(), srcDigest)
	if err != nil {
		t.Fatal(err)
	}

	dst := writeRepo(t, reg, "mirror/suite")
	copyBlobsOf(t, reg, src, dst, raw)

	// Pushed BY DIGEST: staged, but not yet visible under any tag.
	if _, err := dst.PushManifest(t.Context(), desc.Digest.String(), desc.MediaType, raw); err != nil {
		t.Fatalf("push by digest: %v", err)
	}
	if reg.TagDigest("mirror/suite", "v1.0.0") != "" {
		t.Fatal("pushing by digest must not create a tag")
	}

	if err := dst.Tag(t.Context(), desc.Digest, "v1.0.0"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if got := reg.TagDigest("mirror/suite", "v1.0.0"); got != desc.Digest.String() {
		t.Errorf("tag resolves to %q, want %q", got, desc.Digest)
	}
}

// Resumption is deliberately unimplemented, and reports itself as such rather
// than pretending. The caller's fallback - restart the blob - is always correct
// because blobs are content-addressed.
func TestResumeUploadReportsUnsupported(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	dst := writeRepo(t, reg, "mirror/suite")
	err := dst.ResumeUpload(t.Context(),
		registry.UploadState{Location: "/v2/mirror/suite/blobs/uploads/1", Offset: 4096},
		strings.NewReader("rest"))

	if !errors.Is(err, registry.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported so the caller restarts the blob, got %v", err)
	}
}

// copyBlobsOf moves every blob a manifest references, so a manifest push has
// something to reference.
func copyBlobsOf(t *testing.T, reg *fakeregistry.Registry, src, dst *Repository, raw []byte) {
	t.Helper()
	for _, dgst := range referencedDigests(t, raw) {
		rc, err := src.FetchBlob(t.Context(), dgst)
		if err != nil {
			t.Fatalf("fetch blob %s: %v", dgst.Short(), err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := dst.PushBlob(t.Context(), dgst, int64(len(body)), bytes.NewReader(body)); err != nil {
			t.Fatalf("push blob %s: %v", dgst.Short(), err)
		}
	}
	_ = reg
}

// referencedDigests lists the config and layer digests a manifest names.
func referencedDigests(t *testing.T, raw []byte) []registry.Digest {
	t.Helper()
	var body struct {
		Config *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	var out []registry.Digest
	if body.Config != nil && body.Config.Digest != "" {
		out = append(out, registry.Digest(body.Config.Digest))
	}
	for _, l := range body.Layers {
		out = append(out, registry.Digest(l.Digest))
	}
	return out
}

// A registry that will not accept these bytes will not accept them the eighth
// time either.
//
// This used to come back `unclassified`, and unclassified is treated as
// retryable on the reasonable theory that an uncategorised failure is more
// likely transient than permanent. For a manifest the destination rejects, that
// theory costs eight full round trips per manifest and ends exactly where it
// started - which on a bundle of five hundred manifests over a slow link is the
// difference between a failure and an afternoon.
func TestARejectedManifestIsNotTreatedAsATransientFault(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	src := writeRepo(t, reg, "vendor/suite")
	srcDigest := reg.AddImage("vendor/suite", "v1.0.0", fakeregistry.NewLayer("layer"))
	desc, raw, err := src.FetchManifest(t.Context(), srcDigest)
	if err != nil {
		t.Fatal(err)
	}

	dst := writeRepo(t, reg, "mirror/suite")
	reg.RejectNext("/manifests/", 400, "MANIFEST_INVALID", "schema version is not supported", 1)

	_, err = dst.PushManifest(t.Context(), desc.Digest.String(), desc.MediaType, raw)
	if err == nil {
		t.Fatal("a rejected manifest push reported success")
	}

	if got := registry.ClassOf(err); got != registry.ClassUnsupported {
		t.Errorf("class = %q, want unsupported", got)
	}
	if registry.Retryable(err) {
		t.Error("a manifest the registry refuses was marked retryable")
	}

	// And the operator has to be able to see WHY without opening a debugger.
	for _, want := range []string{"400", "manifest invalid", "schema version"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error %q does not carry %q - what the registry actually said", err, want)
		}
	}
}

// The same status, a different meaning. Some registries report a missing blob
// on a manifest push as 400 rather than 404, and reading the status alone gets
// that right half the time. The self-healing path in the engine keys off this
// classification, so getting it wrong turns a repairable placement-cache miss
// into a permanent failure.
func TestAMissingBlobReportedAsA400IsStillAMissingBlob(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	src := writeRepo(t, reg, "vendor/suite")
	srcDigest := reg.AddImage("vendor/suite", "v1.0.0", fakeregistry.NewLayer("layer"))
	desc, raw, err := src.FetchManifest(t.Context(), srcDigest)
	if err != nil {
		t.Fatal(err)
	}

	// No blobs at the destination: the fake answers 400 MANIFEST_BLOB_UNKNOWN,
	// exactly as a real registry does.
	dst := writeRepo(t, reg, "mirror/suite")
	_, err = dst.PushManifest(t.Context(), desc.Digest.String(), desc.MediaType, raw)
	if err == nil {
		t.Fatal("a manifest whose blobs are absent was accepted")
	}
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("error %q is not a not-found; the engine will not repair the placement", err)
	}
}
