// Package middleware provides the Coordinator's HTTP middleware chain.
//
// Order is fixed and load-bearing (docs/design/09-api.md section 10.1):
//
//	RequestID -> Logging -> Tracing -> Metrics -> Recovery -> Auth -> Compress -> handler
//
// RequestID is outermost so every log line, span and error carries it.
// Recovery sits inside Metrics so a panic is still counted as a 500 rather
// than vanishing from the metrics entirely. Compress is innermost so the
// bytes the other five observe are the bytes that went on the wire.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/trace"

	plog "github.com/abhijeet-oxide/softwareGateway/internal/platform/log"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
)

type ctxKeyRequestID struct{}

// maxInboundRequestIDLen bounds an adopted request ID.
//
// The value is echoed in a response header and written to the audit trail, so
// an unbounded caller-supplied string would be a cheap way to bloat both.
const maxInboundRequestIDLen = 128

// RequestID assigns or adopts a request ID and echoes it in the response.
//
// An inbound X-Request-Id is honoured so a caller can correlate across
// services, but only if it is sane: bounded in length and printable ASCII.
// A header value containing CR/LF would otherwise be reflected into the
// response headers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, id)))
	})
}

func validRequestID(s string) bool {
	if s == "" || len(s) > maxInboundRequestIDLen {
		return false
	}
	for _, c := range s {
		// Printable ASCII only; excludes CR, LF and control characters.
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// newRequestID mints an opaque 128-bit identifier.
//
// crypto/rand rather than a counter: a counter leaks request volume and
// collides across replicas, and these IDs appear in a durable audit trail.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// timestamp-derived ID is still better than an empty one, because an
		// error with no correlation ID is untraceable.
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// RequestIDFrom returns the request ID, or "" when absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return id
}

// Logging attaches a request-scoped logger and logs completion.
func Logging(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			l := base.With(slog.String(plog.KeyRequestID, RequestIDFrom(r.Context())))
			if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
				l = l.With(slog.String(plog.KeyTraceID, sc.TraceID().String()))
			}
			ctx := plog.Into(r.Context(), l)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r.WithContext(ctx))

			// Probes and the metrics scrape run every few seconds; logging
			// them at info would bury everything else.
			level := slog.LevelInfo
			if isNoisyPath(r.URL.Path) {
				level = slog.LevelDebug
			} else if ww.Status() >= 500 {
				level = slog.LevelError
			}

			l.Log(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

func isNoisyPath(p string) bool {
	return p == "/healthz" || p == "/readyz" || p == "/metrics"
}

// Metrics records request counts and latency.
//
// The route TEMPLATE is used as the label, never the populated path -
// otherwise every product name would mint a new time series.
func Metrics(reg *metrics.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				// Unmatched request. Bucketing these as a literal keeps a
				// scanner probing random URLs from exploding cardinality.
				route = "unmatched"
			}

			reg.APIRequests.WithLabelValues(route, r.Method, metrics.StatusClass(ww.Status())).Inc()
			reg.APILatency.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		})
	}
}

// Recovery turns a panic into a 500 with a problem detail.
//
// The response deliberately carries no panic text: the specifics go to the
// logs keyed by request ID, so an attacker learns nothing from a crash.
func Recovery(writeInternal func(http.ResponseWriter, *http.Request, any)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is the documented way to abandon a
					// response; re-panic so the server handles it normally
					// rather than reporting it as an internal error.
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}
					writeInternal(w, r, rec)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
