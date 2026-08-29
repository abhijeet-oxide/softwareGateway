package registry

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

// Every one of these arrived as ClassUnclassified before, which is retryable
// with the full transient budget: eight complete re-uploads of a half-gigabyte
// layer for a path that cuts every one of them at the same offset.
func TestADroppedConnectionIsNamedRatherThanLeftUnclassified(t *testing.T) {
	for name, err := range map[string]error{
		"the bare sentinel": io.ErrUnexpectedEOF,
		"wrapped by url.Error": fmt.Errorf(
			`Put "https://registry.example.com/v2/x/blobs/uploads/abc?digest=sha256%%3Aff": ` +
				`unexpected EOF`),
		"a reset by the peer":     syscall.ECONNRESET,
		"a broken pipe":           syscall.EPIPE,
		"a closed net connection": net.ErrClosed,
		"the standard library's short-body report": errors.New(
			"http: ContentLength=478200000 with Body length 40700000"),
		"a proxy that reset mid-body": errors.New(
			"write tcp 10.0.0.4:52344->10.0.0.9:443: connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			sentinel := ClassifyTransport(err)
			if sentinel == nil {
				t.Fatalf("not recognised as a transport failure: %v", err)
			}
			if !errors.Is(sentinel, ErrConnectionLost) {
				t.Fatalf("classified as %v, want %v", sentinel, ErrConnectionLost)
			}
			if ClassOf(sentinel) != ClassNetwork {
				t.Errorf("class is %q, want %q", ClassOf(sentinel), ClassNetwork)
			}
			if !Retryable(sentinel) {
				t.Error("a dropped connection is worth retrying and was reported as not")
			}
		})
	}
}

// A deadline is our patience running out, not somebody else hanging up, and the
// two send an operator to different knobs.
func TestADeadlineIsATimeoutAndNotADroppedConnection(t *testing.T) {
	if got := ClassifyTransport(timeoutError{}); !errors.Is(got, ErrTimeout) {
		t.Errorf("classified as %v, want %v", got, ErrTimeout)
	}
}

// The classifier is the last rung of somebody else's ladder, so it has to
// answer "not mine" rather than claiming everything.
func TestAnErrorThatIsNotATransportFailureIsLeftAlone(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("manifest invalid"),
		ErrUnauthorized,
	} {
		if got := ClassifyTransport(err); got != nil {
			t.Errorf("ClassifyTransport(%v) = %v, want nil", err, got)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
