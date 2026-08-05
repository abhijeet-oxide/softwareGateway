package v1

import (
	"fmt"
	"net/http"
)

// Code is the stable error enum. Clients switch on this, never on the HTTP
// status or the message text.
type Code string

const (
	CodeInvalidArgument    Code = "INVALID_ARGUMENT"
	CodeNotFound           Code = "NOT_FOUND"
	CodeAlreadyExists      Code = "ALREADY_EXISTS"
	CodeFailedPrecondition Code = "FAILED_PRECONDITION"
	CodeAborted            Code = "ABORTED"
	CodeResourceExhausted  Code = "RESOURCE_EXHAUSTED"
	CodeUnavailable        Code = "UNAVAILABLE"
	CodeInternal           Code = "INTERNAL"
	CodeUnauthenticated    Code = "UNAUTHENTICATED"
	CodePermissionDenied   Code = "PERMISSION_DENIED"
)

// ProblemContentType is the media type for error responses (RFC 9457).
const ProblemContentType = "application/problem+json"

// Problem is an RFC 9457 problem detail.
//
// docs/design/09-api.md section 8: `detail` names the offending field and why.
// "invalid request" forces the user into the logs, which they usually cannot
// read.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   Code   `json:"code"`

	// RequestID is on EVERY error and every response. It is the thread from a
	// user's failure to the server logs, the trace and the audit record.
	RequestID string `json:"requestId,omitempty"`

	// Errors carries per-field detail for validation failures.
	Errors []FieldError `json:"errors,omitempty"`

	// CurrentState accompanies FAILED_PRECONDITION so a caller can see why the
	// transition was rejected without a second round trip.
	CurrentState string `json:"currentState,omitempty"`
}

// FieldError locates one validation failure.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Error implements error so a client can return a Problem directly.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%s: %s", p.Code, p.Detail)
	}
	return fmt.Sprintf("%s: %s", p.Code, p.Title)
}

// StatusFor maps a code to its HTTP status. One place, so a handler cannot
// pair INVALID_ARGUMENT with 500 by accident.
func StatusFor(c Code) int {
	switch c {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists, CodeFailedPrecondition:
		return http.StatusConflict
	case CodeAborted:
		return http.StatusPreconditionFailed
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// TitleFor is the human-readable summary for a code.
func TitleFor(c Code) string {
	switch c {
	case CodeInvalidArgument:
		return "Invalid argument"
	case CodeNotFound:
		return "Not found"
	case CodeAlreadyExists:
		return "Already exists"
	case CodeFailedPrecondition:
		return "Failed precondition"
	case CodeAborted:
		return "Aborted"
	case CodeResourceExhausted:
		return "Resource exhausted"
	case CodeUnavailable:
		return "Service unavailable"
	case CodeUnauthenticated:
		return "Unauthenticated"
	case CodePermissionDenied:
		return "Permission denied"
	default:
		return "Internal error"
	}
}

// TypeURI is the documentation URI for a code, per RFC 9457's `type` field.
func TypeURI(c Code) string {
	switch c {
	case CodeInvalidArgument:
		return "https://softwaregateway.io/errors/invalid-argument"
	case CodeNotFound:
		return "https://softwaregateway.io/errors/not-found"
	case CodeAlreadyExists:
		return "https://softwaregateway.io/errors/already-exists"
	case CodeFailedPrecondition:
		return "https://softwaregateway.io/errors/failed-precondition"
	case CodeAborted:
		return "https://softwaregateway.io/errors/aborted"
	case CodeResourceExhausted:
		return "https://softwaregateway.io/errors/resource-exhausted"
	case CodeUnavailable:
		return "https://softwaregateway.io/errors/unavailable"
	case CodeUnauthenticated:
		return "https://softwaregateway.io/errors/unauthenticated"
	case CodePermissionDenied:
		return "https://softwaregateway.io/errors/permission-denied"
	default:
		return "https://softwaregateway.io/errors/internal"
	}
}

// NewProblem builds a problem detail with its status, title and type filled in
// from the code, so those three can never disagree.
func NewProblem(code Code, detail string) *Problem {
	return &Problem{
		Type:   TypeURI(code),
		Title:  TitleFor(code),
		Status: StatusFor(code),
		Detail: detail,
		Code:   code,
	}
}
