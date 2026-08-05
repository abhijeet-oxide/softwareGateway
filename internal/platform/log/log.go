// Package log configures structured logging.
//
// See docs/design/12-observability-and-audit.md section 6. Every line carries
// the correlation keys that make metrics, traces, logs and audit one story.
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Correlation keys. Used as attribute names everywhere so that a query on
// request_id joins logs to traces to audit records.
const (
	KeyRequestID  = "request_id"
	KeyTraceID    = "trace_id"
	KeyTransferID = "transfer_id"
	KeyJobID      = "job_id"
	KeyWorkerID   = "worker_id"
	KeyComponent  = "component"
	KeyProduct    = "product"
	KeyRepository = "repository"
	KeyDigest     = "digest"
)

// Config selects level and format.
type Config struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

// New builds a logger. JSON is the default because these logs are shipped to a
// cluster aggregator; text exists for local development readability.
func New(cfg Config, w io.Writer, component string) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}

	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}

	var h slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	return slog.New(h).With(slog.String(KeyComponent, component))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type ctxKey struct{}

// Into returns a context carrying the logger.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the context's logger, or the default if none is set.
// Never returns nil, so callers need no guard.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
