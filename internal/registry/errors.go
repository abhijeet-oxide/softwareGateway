package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Registry errors are classified ONCE, here at the boundary.
//
// Retry policy, discovery backoff and the adaptive controller all key off the
// class - never off an HTTP status re-inspected deep in the call stack. That
// is what stops "is 429 retryable?" being answered differently in three
// places. See docs/design/06 §7 and docs/design/10 §6.
var (
	// ErrNotFound covers 404, BLOB_UNKNOWN and MANIFEST_UNKNOWN. Whether this
	// is an error at all is context-dependent: a 404 from StatBlob is a
	// perfectly normal negative answer.
	ErrNotFound = errors.New("not found")

	// ErrUnauthorized is a 401. Not retryable: credentials do not fix
	// themselves, and hammering an auth endpoint gets us rate-limited.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrForbidden is a 403. Not retryable.
	ErrForbidden = errors.New("forbidden")

	// ErrRateLimited is a 429. Retryable, honouring Retry-After over our own
	// backoff - the registry telling us how long to wait is better information
	// than a formula.
	ErrRateLimited = errors.New("rate limited")

	// ErrUnavailable is a 5xx. Retryable with full backoff.
	ErrUnavailable = errors.New("registry unavailable")

	// ErrTimeout is a connect, header or stall timeout. Retryable.
	ErrTimeout = errors.New("timeout")

	// ErrDigestMismatch means content did not hash as claimed. Retryable but
	// capped low: retrying rarely helps and it may indicate real corruption
	// worth surfacing rather than absorbing.
	ErrDigestMismatch = errors.New("digest mismatch")

	// ErrUnsupported means the registry lacks a capability. Not retryable.
	ErrUnsupported = errors.New("unsupported by registry")

	// ErrMountUnsupported means a cross-repository mount was declined.
	//
	// NOT a failure, and separate from ErrUnsupported so a caller cannot
	// accidentally treat it as one. Mount is an optimisation: the specification
	// permits a registry to answer 202 with an ordinary upload session instead,
	// and support is uneven in practice. Streaming the blob is always correct,
	// so this means "take the next rung of the ladder", not "this went wrong".
	ErrMountUnsupported = errors.New("cross-repository mount declined")

	// ErrMalformedResponse means the registry answered with something we
	// cannot parse - a proxy error page in place of JSON, say.
	ErrMalformedResponse = errors.New("malformed registry response")

	// ErrConnectionLost means the connection died mid-transfer: the peer, or
	// something between us and it, stopped talking before the body it promised
	// had arrived. Retryable.
	//
	// # Why this is not folded into ErrTimeout
	//
	// A timeout is OUR deadline expiring, and the fix is usually to wait
	// longer or to ask for less at once. A dropped connection is somebody
	// ELSE hanging up - a load balancer's idle or request-duration cap, a
	// proxy that will not carry a body this large, a registry that reset the
	// stream - and the fix is on the path, not in our patience. Reporting the
	// two as one class sends an operator to the wrong knob.
	//
	// It also has to exist so these stop arriving as ClassUnclassified: an
	// uncategorised error is retried with the full transient budget, which is
	// right for a blip and is eight full re-uploads of a 478 MB layer when a
	// proxy is capping every request at the same offset.
	ErrConnectionLost = errors.New("connection lost")
)

// Class is the coarse category used for retry decisions and metrics.
//
// A bounded set, so it is safe as a metric label - unlike an error message.
type Class string

const (
	ClassNotFound     Class = "not_found"
	ClassAuth         Class = "auth"
	ClassRateLimited  Class = "rate_limited"
	ClassUnavailable  Class = "unavailable"
	ClassTimeout      Class = "timeout"
	ClassIntegrity    Class = "integrity"
	ClassUnsupported  Class = "unsupported"
	ClassMalformed    Class = "malformed"
	ClassNetwork      Class = "network"
	ClassUnclassified Class = "unclassified"
)

// Error carries a classified registry failure with the context needed to act
// on it without re-parsing anything.
type Error struct {
	Op         string        // "list tags", "resolve tag"
	Repository string        // "registry.example.com/vendor-a/platform"
	Reference  string        // tag or digest, when applicable
	StatusCode int           // 0 when the failure was not an HTTP response
	RetryAfter time.Duration // from the Retry-After header, when present
	Detail     string        // the registry's own error message, when parseable
	Err        error         // one of the sentinels above
}

func (e *Error) Error() string {
	msg := e.Op
	if e.Repository != "" {
		msg += " " + e.Repository
	}
	if e.Reference != "" {
		msg += ":" + e.Reference
	}
	if e.StatusCode != 0 {
		msg = fmt.Sprintf("%s: HTTP %d", msg, e.StatusCode)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	// The sentinel, unless the registry's own words already said it. A 401
	// arrives as `unauthorized: authentication required`, and appending our
	// sentinel to that produced `unauthorized: authentication required:
	// unauthorized` - one fact stated twice, on the line an operator reads
	// first. Suppressed only on an exact word match, so a sentinel the detail
	// does NOT cover is still reported.
	if e.Err != nil && !detailStates(e.Detail, e.Err) {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// detailStates reports whether the registry's own message already carries the
// sentinel's word.
func detailStates(detail string, err error) bool {
	if detail == "" {
		return false
	}
	return strings.Contains(strings.ToLower(detail), strings.ToLower(err.Error()))
}

func (e *Error) Unwrap() error { return e.Err }

// Class returns the failure category.
func (e *Error) Class() Class { return ClassOf(e.Err) }

// Retryable reports whether retrying could plausibly succeed.
func (e *Error) Retryable() bool { return Retryable(e.Err) }

// ClassOf categorises any error, including a wrapped one.
func ClassOf(err error) Class {
	switch {
	case err == nil:
		return ClassUnclassified
	case errors.Is(err, ErrNotFound):
		return ClassNotFound
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrForbidden):
		return ClassAuth
	case errors.Is(err, ErrRateLimited):
		return ClassRateLimited
	case errors.Is(err, ErrUnavailable):
		return ClassUnavailable
	case errors.Is(err, ErrTimeout):
		return ClassTimeout
	case errors.Is(err, ErrDigestMismatch):
		return ClassIntegrity
	case errors.Is(err, ErrUnsupported):
		return ClassUnsupported
	case errors.Is(err, ErrMalformedResponse):
		return ClassMalformed
	case errors.Is(err, ErrConnectionLost):
		return ClassNetwork
	default:
		return ClassUnclassified
	}
}

// Retryable reports whether an error is worth retrying.
//
// An unclassified error is treated as retryable: a transient network fault we
// failed to categorise is far more likely than a permanent one, and the
// attempt cap bounds the cost of being wrong.
func Retryable(err error) bool {
	switch ClassOf(err) {
	case ClassRateLimited, ClassUnavailable, ClassTimeout, ClassNetwork, ClassUnclassified:
		return true
	case ClassIntegrity:
		return true // capped low by the caller's attempt policy
	default:
		return false
	}
}

// RetryAfterOf returns the server-directed delay, or zero.
func RetryAfterOf(err error) time.Duration {
	var re *Error
	if errors.As(err, &re) {
		return re.RetryAfter
	}
	return 0
}

// ClassifyStatus maps an HTTP status to a sentinel.
//
// Exported because every registry backend needs it and each one writing its own
// switch is how classifications drift apart - one backend deciding 403 is
// retryable while another does not is a bug that only shows up under load.
func ClassifyStatus(status int) error {
	switch {
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return ErrForbidden
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 500:
		return ErrUnavailable
	case status >= 400:
		// 400, 405, 406 and friends. Not retryable, and not auth - usually a
		// capability the registry does not have.
		return ErrUnsupported
	default:
		return nil
	}
}

// ClassifyTransport names a failure that never reached the point of being an
// HTTP response - the connection itself went wrong.
//
// Returns nil when the error is not one of those, so a caller can use it as
// the last rung of its own ladder without it swallowing anything.
//
// # Why this exists, and what it was costing not to have it
//
// A blob copy streams a source response body straight into a destination
// request body. When the SOURCE hangs up mid-body, Go's HTTP client surfaces
// `io.ErrUnexpectedEOF` from the read - and because that read happens inside
// the destination's `Do`, the error comes back wrapped as
// `Put "https://destination/...": unexpected EOF`. It names the destination
// URL, the destination host and the destination's upload session, and every
// word of that is about the end of the path that was working.
//
// Nothing matched it, so it landed in ClassUnclassified, which is retryable
// with the full transient budget. The observed result is a table of eight-of-
// eight attempts against the destination for a source that was dropping the
// connection at the same offset every time.
//
// # Why the string match
//
// The errors that matter are wrapped by `*url.Error`, by ORAS, and by the
// standard library's own transfer code, and several of them are constructed
// with `errors.New` at the point of failure rather than being sentinels
// anybody can match. `errors.Is` catches what it can; the prose catches the
// rest. Being wrong costs a retry classification, not a wrong result.
func ClassifyTransport(err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return ErrConnectionLost
	case errors.Is(err, net.ErrClosed):
		return ErrConnectionLost
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNABORTED):
		return ErrConnectionLost
	}

	// A deadline is a timeout however it was wrapped, and it is asked first
	// because a timed-out read often reports itself as a closed connection too.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}

	text := strings.ToLower(err.Error())
	for _, phrase := range droppedConnectionPhrases {
		if strings.Contains(text, phrase) {
			return ErrConnectionLost
		}
	}
	return nil
}

// droppedConnectionPhrases are the ways this failure spells itself once it has
// been through `*url.Error` and whatever wrapped that.
//
// "unexpected eof" is the one that matters most: it is what Go reports when a
// response body ends before the Content-Length the server declared, which is
// exactly a connection cut mid-blob.
var droppedConnectionPhrases = []string{
	"unexpected eof",
	"connection reset by peer",
	"broken pipe",
	"connection forcibly closed",
	"use of closed network connection",
	"http2: stream closed",
	"server closed idle connection",
	"transport connection broken",
	"unexpected end of stream",
	// net/http's own words when a request body delivered fewer bytes than the
	// Content-Length it declared - the short-read half of the same fault.
	"with body length",
}
