package registry

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Registry errors are classified ONCE, here at the boundary.
//
// Retry policy, discovery backoff and the adaptive controller all key off the
// class — never off an HTTP status re-inspected deep in the call stack. That
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
	// backoff — the registry telling us how long to wait is better information
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

	// ErrMalformedResponse means the registry answered with something we
	// cannot parse — a proxy error page in place of JSON, say.
	ErrMalformedResponse = errors.New("malformed registry response")
)

// Class is the coarse category used for retry decisions and metrics.
//
// A bounded set, so it is safe as a metric label — unlike an error message.
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
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
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
	case ClassRateLimited, ClassUnavailable, ClassTimeout, ClassUnclassified:
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
// switch is how classifications drift apart — one backend deciding 403 is
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
		// 400, 405, 406 and friends. Not retryable, and not auth — usually a
		// capability the registry does not have.
		return ErrUnsupported
	default:
		return nil
	}
}
