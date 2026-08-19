package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/backoff"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// retryTransport retries transient failures.
//
// Sits inside the rate limiter (so retries are themselves rate-limited) and
// outside auth (so a 401 is handled by re-authenticating rather than by
// retrying blindly).
type retryTransport struct {
	next     http.RoundTripper
	policy   backoff.Policy
	attempts int
	// maxElapsed bounds the TOTAL time one request may spend across all of its
	// attempts. See DefaultRetryMaxElapsed.
	maxElapsed time.Duration
}

// maxAttempts matches the transient class in docs/design/10 §6.
const maxAttempts = 8

func newRetryTransport(next http.RoundTripper, cfg Config) http.RoundTripper {
	attempts := cfg.MaxRetryAttempts
	if attempts <= 0 {
		attempts = maxAttempts
	}
	elapsed := cfg.RetryMaxElapsed
	if elapsed <= 0 {
		elapsed = DefaultRetryMaxElapsed
	}
	return &retryTransport{
		next:       next,
		policy:     backoff.Policy{Base: cfg.RetryBaseDelay, Cap: cfg.RetryMaxDelay},
		attempts:   attempts,
		maxElapsed: elapsed,
	}
}

func (t *retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// A request with a body can only be retried if it can be rewound.
	// GetBody is set by the stdlib for in-memory bodies; without it, replaying
	// would send a truncated request, which is worse than not retrying.
	rewindable := r.Body == nil || r.Body == http.NoBody || r.GetBody != nil

	started := time.Now()

	var lastErr error
	for attempt := 0; attempt < t.attempts; attempt++ {
		if attempt > 0 && t.exhausted(started) {
			// The attempt COUNT is the wrong bound on its own. Against a server
			// that accepts the connection and never answers, every attempt costs
			// the full ResponseHeaderTimeout, so eight attempts is four minutes
			// for ONE request - and discovery makes two of those per tag.
			// Measured in the field: a scan sat on a single tag for over two
			// minutes and had 47 more repositories to go.
			//
			// A time budget bounds the pathological case without shortening the
			// schedule for the transient one it exists for, where attempts
			// return quickly and the budget is never reached.
			break
		}
		if attempt > 0 {
			req, err := rewind(r, attempt)
			if err != nil {
				return nil, err
			}
			r = req
		}

		resp, err := t.next.RoundTrip(r)

		switch {
		case err != nil:
			lastErr = err
			// A cancelled context is a decision, not a fault: stop immediately.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// Some failures cannot be fixed by trying again. Missing or wrong
			// credentials are the important case: retrying them burns the full
			// backoff schedule - minutes - before reporting a problem the
			// operator could have been told about immediately, and hammering an
			// auth endpoint is a good way to get throttled.
			if !registry.Retryable(err) {
				return nil, err
			}
			if !rewindable {
				return nil, err
			}

		case retryableStatus(resp.StatusCode):
			if !rewindable || attempt == t.attempts-1 || t.exhausted(started) {
				return resp, nil
			}
			delay := t.delayFor(resp, attempt)
			// The body must be drained and closed before reusing the
			// connection; leaking it here would exhaust the pool under load.
			drain(resp)
			if err := sleep(r.Context(), delay); err != nil {
				return nil, err
			}
			continue

		default:
			return resp, nil
		}

		if err := sleep(r.Context(), t.policy.Delay(attempt)); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

// delayFor prefers the registry's own Retry-After over our backoff.
//
// A registry telling us how long to wait is better information than a formula,
// and ignoring it is a reliable way to get an IP blocked.
func (t *retryTransport) delayFor(resp *http.Response, attempt int) time.Duration {
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		// Cap it: a hostile or misconfigured header must not park a worker for
		// an hour.
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		return d
	}
	return t.policy.Delay(attempt)
}

// parseRetryAfter handles both forms the header permits: delta-seconds and an
// HTTP date.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusRequestTimeout ||
		status >= 500
}

func rewind(r *http.Request, attempt int) (*http.Request, error) {
	req := r.Clone(r.Context())
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	_ = attempt
	return req, nil
}

// drain reads and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	_ = resp.Body.Close()
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// exhausted reports whether the request has spent its whole retry budget.
func (t *retryTransport) exhausted(started time.Time) bool {
	return t.maxElapsed > 0 && time.Since(started) >= t.maxElapsed
}
