package api

import (
	"net/http"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// fault reports an operation that failed, and CHOOSES THE CODE FROM WHY.
//
// # The incident
//
// Every read in this API used to answer UNAVAILABLE when its query failed:
//
//	Error(w, r, v1.CodeUnavailable, "could not list the files: "+err.Error())
//
// One of those queries coalesced a JSONB column with an empty string, which
// Postgres refuses (SQLSTATE 22P02). So a bug affecting exactly one tab of one
// page was served as 503 - the status this API reserves for "the service is
// standing down, come back shortly" - and every client believed it. The
// interface's connection monitor put an outage banner over the whole
// application; its health probe found the service perfectly healthy a second
// later, announced the recovery, refetched everything on screen, hit the same
// broken query and began again. Hundreds of requests a minute, and a reader
// watching a backend apparently restarting in a loop when nothing had
// restarted at all.
//
// 503 IS A PROMISE ABOUT THE SERVICE, not a description of a request. A caller
// is entitled to read it as "wait and retry, nothing is wrong with what you
// asked for". An error that will fail identically on every retry must never
// wear it.
//
// # The rule
//
// The database being unreachable is an outage; a statement it refused is this
// service's own fault. store.Unreachable tells them apart, and is deliberately
// narrow: a connection, not a timeout and not a syntax error.
//
// `what` is the sentence fragment a reader sees in front of the cause - "could
// not list the files". The cause is kept rather than replaced with a generic
// line, because it is what made this diagnosable from a screenshot; the
// request ID that goes on automatically is what ties it to the log line below.
func (s *Server) fault(w http.ResponseWriter, r *http.Request, what string, err error) {
	code := v1.CodeInternal
	if store.Unreachable(err) {
		code = v1.CodeUnavailable
	}
	if s.deps.Logger != nil {
		s.deps.Logger.ErrorContext(r.Context(), "request failed",
			"op", what, "error", err, "code", string(code))
	}
	detail := err.Error()
	if what != "" {
		detail = what + ": " + detail
	}
	Error(w, r, code, detail)
}
