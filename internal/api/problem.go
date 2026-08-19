package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	plog "github.com/abhijeet-oxide/softwareGateway/internal/platform/log"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// WriteProblem emits an RFC 9457 problem detail.
//
// The request ID is attached here rather than at each call site, so it is
// impossible to return an error a user cannot correlate with a log line.
func WriteProblem(w http.ResponseWriter, r *http.Request, p *v1.Problem) {
	if p.RequestID == "" {
		p.RequestID = middleware.RequestIDFrom(r.Context())
	}
	if p.Status == 0 {
		p.Status = v1.StatusFor(p.Code)
	}

	w.Header().Set("Content-Type", v1.ProblemContentType)
	w.WriteHeader(p.Status)

	if err := json.NewEncoder(w).Encode(p); err != nil {
		plog.From(r.Context()).Error("failed to encode problem detail", "error", err)
	}
}

// Error is the common path: build a problem from a code and detail, and write it.
func Error(w http.ResponseWriter, r *http.Request, code v1.Code, detail string) {
	WriteProblem(w, r, v1.NewProblem(code, detail))
}

// NotFound reports a missing resource, naming what was looked for.
func NotFound(w http.ResponseWriter, r *http.Request, kind, id string) {
	Error(w, r, v1.CodeNotFound, kind+" "+id+" was not found")
}

// WriteJSON emits a successful JSON response.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so the response cannot be
		// converted into an error. Log it - this is a server bug.
		plog.From(r.Context()).Error("failed to encode response body", "error", err)
	}
}

// internalErrorWriter returns the Recovery middleware's handler.
//
// The panic value and stack go to the log; the client gets a generic message
// plus the request ID. Leaking a stack trace to a caller tells an attacker
// about the internals and tells a legitimate user nothing they can act on.
func internalErrorWriter(logger *slog.Logger) func(http.ResponseWriter, *http.Request, any) {
	return func(w http.ResponseWriter, r *http.Request, rec any) {
		reqID := middleware.RequestIDFrom(r.Context())

		logger.Error("panic recovered",
			slog.Any("panic", rec),
			slog.String(plog.KeyRequestID, reqID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("stack", string(debug.Stack())),
		)

		WriteProblem(w, r, &v1.Problem{
			Type:      v1.TypeURI(v1.CodeInternal),
			Title:     v1.TitleFor(v1.CodeInternal),
			Status:    http.StatusInternalServerError,
			Code:      v1.CodeInternal,
			Detail:    "An internal error occurred. Quote the request ID when reporting it.",
			RequestID: reqID,
		})
	}
}

// unauthenticatedWriter is installed in the Auth middleware. Unreachable while
// AnonymousAuthenticator is in place, but wired so that switching on real
// authentication needs no change here.
func unauthenticatedWriter(w http.ResponseWriter, r *http.Request, err error) {
	detail := "Authentication is required."
	if err != nil {
		detail = err.Error()
	}
	Error(w, r, v1.CodeUnauthenticated, detail)
}
