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

func newRateLimitTransport(next http.RoundTripper, rps, burst int) http.RoundTripper {
	if rps <= 0 {
		return next // unlimited
	}
	if burst <= 0 {
		burst = rps * 2
	}
	return &rateLimitTransport{
		next:    next,
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
	}
}

func (t *rateLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Wait honours context cancellation, so a shutdown or a cancelled scan is
	// not delayed by a full bucket.
	if err := t.limiter.Wait(r.Context()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}
