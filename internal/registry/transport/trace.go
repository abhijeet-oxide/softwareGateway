package transport

import (
	"log/slog"
	"net/http"
	"time"
)

// DefaultSlowRequest is how long one registry request may take before it is
// worth complaining about at WARN.
//
// Ten seconds: an order of magnitude above a healthy manifest HEAD, and well
// below the thirty-second header timeout, so a request that is heading for a
// timeout is reported BEFORE it fails rather than only after.
const DefaultSlowRequest = 10 * time.Second

// traceTransport logs every request.
//
// It exists because "discovery is slow" was, from the outside, indistinguishable
// from "discovery is broken". The counters said two tags in fifty-four seconds
// and there was no way to see whether that was one slow request or two hundred,
// which host they were going to, or whether they were even leaving the process.
//
// Placed OUTERMOST of the functional layers - outside the rate limiter - so the
// duration it reports is the wall-clock cost the scan actually paid, including
// time spent waiting for a rate-limit token or sleeping between retries. A
// timer inside those layers would report a healthy 200ms for a request that
// took the scanner thirty seconds, which is precisely the lie that makes this
// kind of problem hard to find.
type traceTransport struct {
	next http.RoundTripper
	log  *slog.Logger
	slow time.Duration
}

func newTraceTransport(next http.RoundTripper, log *slog.Logger, slow time.Duration) http.RoundTripper {
	if log == nil {
		return next
	}
	if slow <= 0 {
		slow = DefaultSlowRequest
	}
	return &traceTransport{next: next, log: log, slow: slow}
}

func (t *traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.next.RoundTrip(r)
	elapsed := time.Since(started)

	attrs := []any{
		"method", r.Method,
		"host", r.URL.Host,
		"path", r.URL.Path,
		"duration", elapsed.Round(time.Millisecond),
	}
	if resp != nil {
		attrs = append(attrs, "status", resp.StatusCode)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}

	switch {
	case err != nil:
		// Every failed request, at WARN. The scan collects these and carries on,
		// so without this the only evidence of forty failures is a counter.
		t.log.WarnContext(r.Context(), "registry request failed", attrs...)

	case elapsed >= t.slow:
		t.log.WarnContext(r.Context(), "registry request was slow", append(attrs,
			"hint", "raise network.timeouts.responseHeader if the registry is simply "+
				"slow; check network.proxy if it is not")...)

	default:
		t.log.DebugContext(r.Context(), "registry request", attrs...)
	}

	return resp, err
}
