package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// The failure from the screenshot, verbatim enough to be recognisable: a PUT
// to the destination's upload session that ends in `unexpected EOF`.
//
// It is what an operator saw eight times per blob, and it is a description of
// the destination for a failure the SOURCE caused - the read happens inside the
// destination's Do, so the destination's URL is the one in the message.
//
// Capitalised because that is what Go produces - a `*url.Error` renders as
// `Put "<url>": <cause>` - and the point of the fixture is to be the string an
// operator actually saw. Held as a constant so the linter's "error strings
// should not be capitalised" rule, which is about errors we write, does not
// fire on one we are quoting.
const observedPushMessage = `Put "https://artifact.example.com/v2/oci-external/orbs/` +
	`cfx-5000-k8s/blobs/uploads/89ffb45b-5f38-4927-82f3-a08d6e7d9750.patch` +
	`?digest=sha256%3A0035772b529a": unexpected EOF`

// verbatim quotation of one Go produces - a *url.Error renders as
// `Put "<url>": <cause>` - and lower-casing it would make the fixture stop
// being the string an operator actually saw.
//
//nolint:staticcheck // ST1005 is about error strings WE write. This is a
var observedPushError = errors.New(observedPushMessage)

const (
	blobSize  = 478_200_000
	readSoFar = 40_700_000
)

var someDigest = registry.Digest("sha256:0035772b529af91eea5e12f24dff05b19a0fb9114ccf9d3b1ade6bedef6e7bd7")

func TestASourceThatHangsUpIsNotReportedAsADestinationFailure(t *testing.T) {
	src := &watchedReader{n: readSoFar, err: io.ErrUnexpectedEOF}

	got := sideOf(observedPushError, src, blobSize, someDigest)
	msg := got.Error()

	if !strings.Contains(msg, "read blob from source") {
		t.Errorf("the message does not say which end failed:\n%s", msg)
	}
	if strings.HasPrefix(msg, "push blob to destination") {
		t.Errorf("a source-side interruption is still being reported as a "+
			"destination push:\n%s", msg)
	}
	// The two numbers are the whole diagnosis: a body that stopped at a
	// fortieth of its declared size is a cut connection, not a slow one.
	for _, want := range []string{"38.8 MiB", "456.0 MiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not say how far it got (%q missing):\n%s", want, msg)
		}
	}
	// The destination's own words survive, because on an ambiguous case an
	// operator wants both halves.
	if !strings.Contains(msg, "artifact.example.com") {
		t.Errorf("the destination's report was dropped:\n%s", msg)
	}

	if !errors.Is(got, registry.ErrConnectionLost) {
		t.Errorf("class is %q, want %q - an unclassified failure is retried "+
			"with the full transient budget, which is eight complete re-uploads",
			registry.ClassOf(got), registry.ClassNetwork)
	}
}

// The politer half of the same fault: the source closes the body cleanly, just
// early. Every layer between the socket and here reads that as success.
func TestASourceThatClosesEarlyIsCaughtByTheSizeItPromised(t *testing.T) {
	src := &watchedReader{n: readSoFar, err: io.EOF}

	got := sideOf(observedPushError, src, blobSize, someDigest)
	if !strings.Contains(got.Error(), "read blob from source") {
		t.Errorf("a short read that ended in a clean EOF was not caught:\n%s", got)
	}
	if !errors.Is(got, registry.ErrConnectionLost) {
		t.Errorf("class is %q, want %q", registry.ClassOf(got), registry.ClassNetwork)
	}
}

// The case the original wording was always right about. A source that
// delivered every byte it owed leaves the destination as the only candidate,
// and the message must not start hedging about it.
func TestADestinationFailureIsStillReportedAsOne(t *testing.T) {
	src := &watchedReader{n: blobSize}

	got := sideOf(observedPushError, src, blobSize, someDigest)
	if !strings.HasPrefix(got.Error(), "push blob to destination") {
		t.Errorf("a genuine destination failure changed wording:\n%s", got)
	}
	if strings.Contains(got.Error(), "read blob from source") {
		t.Errorf("a complete source read is being blamed:\n%s", got)
	}
}

// A blob whose size the descriptor did not carry cannot be checked for a short
// read, and must not be guessed at: zero bytes expected is not zero bytes owed.
func TestAnUnsizedBlobIsNotAccusedOfBeingShort(t *testing.T) {
	src := &watchedReader{n: 12}

	got := sideOf(observedPushError, src, 0, someDigest)
	if !strings.HasPrefix(got.Error(), "push blob to destination") {
		t.Errorf("an unsized blob was reported as a short source read:\n%s", got)
	}
}

func TestWatchedReaderRemembersTheLastFailure(t *testing.T) {
	w := &watchedReader{r: io.MultiReader(
		strings.NewReader("hello"), errReader{io.ErrUnexpectedEOF})}

	if _, err := io.Copy(io.Discard, w); err == nil {
		t.Fatal("the copy succeeded over a reader that fails")
	}
	if w.n != 5 {
		t.Errorf("counted %d bytes, want 5", w.n)
	}
	if !errors.Is(w.err, io.ErrUnexpectedEOF) {
		t.Errorf("remembered %v, want unexpected EOF", w.err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// ---------------------------------------------------------------------------
// Through the real stream(), not just the attribution helper
// ---------------------------------------------------------------------------

// truncatingSource serves a blob and then hangs up part way through it, which
// is what a vendor registry behind an idle-capping proxy does to a large layer.
//
// `io.ErrUnexpectedEOF` is the standard library's own word for it: Go's HTTP
// client returns exactly this from a response body that ends before the
// Content-Length the server declared.
type truncatingSource struct {
	registry.Repository
	served int
}

func (s *truncatingSource) FetchBlob(context.Context, registry.Digest) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(
		bytes.NewReader(bytes.Repeat([]byte{'x'}, s.served)),
		errReader{io.ErrUnexpectedEOF},
	)), nil
}

// consumingTarget drains whatever it is given and reports the read failure the
// way a real destination client does: wrapped in the PUT it was in the middle
// of, naming the destination's own URL.
type consumingTarget struct {
	registry.Repository
}

func (consumingTarget) Capabilities(context.Context) registry.Capabilities {
	return registry.Capabilities{}
}

func (consumingTarget) StatBlob(context.Context, registry.Digest) (registry.Descriptor, error) {
	return registry.Descriptor{}, registry.ErrNotFound
}

func (consumingTarget) PushBlob(
	_ context.Context, _ registry.Digest, _ int64, src io.Reader,
) error {
	if _, err := io.Copy(io.Discard, src); err != nil {
		return errors.New(observedPushMessage)
	}
	return nil
}

// The whole path, end to end: a source that stops sending must not produce a
// failure about the destination.
//
// This is the case in the screenshot - 40.7 MB of 478.2 MB, eight attempts of
// eight, every one of them reported against the destination's upload URL.
func TestAJobWhoseSourceHangsUpBlamesTheSource(t *testing.T) {
	e := NewEngine(0, slog.New(slog.DiscardHandler))

	res := e.Run(t.Context(), Job{
		ID:        1,
		Kind:      "blob",
		Digest:    someDigest.String(),
		SizeBytes: blobSize,
		Source:    &truncatingSource{served: readSoFar},
		Target:    consumingTarget{},
	}, nil)

	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want a failure", res.Outcome)
	}

	msg := res.Err.Error()
	if !strings.Contains(msg, "the source stopped sending") {
		t.Errorf("the failure does not name the end that broke:\n%s", msg)
	}
	if strings.Contains(msg, "push blob to destination: Put") {
		t.Errorf("still reported as a destination push:\n%s", msg)
	}

	// The class is what the retry policy keys off, and what the failure
	// listing groups by. `unclassified` is what this used to be, and it is
	// retried with the full transient budget - eight complete re-reads of a
	// blob the source will truncate at the same place every time.
	if res.ErrorClass != string(registry.ClassNetwork) {
		t.Errorf("class = %q, want %q", res.ErrorClass, registry.ClassNetwork)
	}

	// And the bytes that DID move are still counted, so the progress bar does
	// not rewind on a failure.
	if res.BytesMoved != readSoFar {
		t.Errorf("BytesMoved = %d, want %d", res.BytesMoved, readSoFar)
	}
}
