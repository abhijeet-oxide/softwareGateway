package transport

import (
	"net/http"

	"golang.org/x/time/rate"
)

// rateLimitTransport applies a token bucket to every request.
//
// This is the OUTERMOST layer, and that is the whole design decision: retries
// happen inside it, so a burst of failures cannot bypass the limit meant to
// protect a struggling registry. Retry-outside-limiter is how a transient
// error becomes an outage.
//
// The limit here is the configured ceiling from the product's rateLimits
// (docs/design/02 §5.3). The adaptive controller in M3 moves the effective
// concurrency WITHIN this ceiling; configuration says what is permitted, the
// controller decides what is safe right now.
type rateLimitTransport struct {
	next    http.RoundTripper
	limiter *rate.Limiter
}

// newLimiter builds the token bucket, or nil for unlimited.
//
// Built once per SOURCE, not per repository: a per-repository limiter means the
// configured requests-per-second is multiplied by the number of repositories
// scanned in parallel, which is the opposite of what a ceiling is for.
func newLimiter(rps, burst int) *rate.Limiter {
	if rps <= 0 {
		return nil // unlimited
	}
	if burst <= 0 {
		burst = rps * 2
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}

// newRateLimitTransportWith wraps next with an already-built limiter.
func newRateLimitTransportWith(next http.RoundTripper, limiter *rate.Limiter) http.RoundTripper {
	if limiter == nil {
		return next
	}
	return &rateLimitTransport{next: next, limiter: limiter}
}

func (t *rateLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Wait honours context cancellation, so a shutdown or a cancelled scan is
	// not delayed by a full bucket.
	if err := t.limiter.Wait(r.Context()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}
