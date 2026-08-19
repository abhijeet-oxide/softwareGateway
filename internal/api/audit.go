package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// GET /api/v1/auditEvents - the audit trail (docs/design/09 §2, 12 §4).
//
// Specified since the first design pass and never registered, which meant the
// table was accumulating a year of history that no interface could show.
//
// Read-only and served by any replica: an audit event is a record, and asking
// "what happened" should not require finding the leader.

func (s *Server) handleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "audit storage is not configured")
		return
	}

	q := r.URL.Query()
	pageSize, err := parsePageSize(q.Get("pageSize"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	offset, err := parseOffset(q.Get("pageToken"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	since, err := parseTimeBound(q.Get("since"), "since")
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	until, err := parseTimeBound(q.Get("until"), "until")
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	products, visible := auditProducts(r, q.Get("product"))
	if !visible {
		// Asking for a product this caller cannot see returns an EMPTY trail
		// rather than a permission error: whether that product exists at all
		// is itself something they have not been told.
		WriteJSON(w, r, http.StatusOK, v1.ListAuditEventsResponse{AuditEvents: []v1.AuditEvent{}})
		return
	}

	filter := store.AuditFilter{
		Products:    products,
		EventType:   q.Get("eventType"),
		Actor:       q.Get("actor"),
		Outcome:     q.Get("outcome"),
		SubjectKind: q.Get("subjectKind"),
		SubjectID:   q.Get("subjectId"),
		Since:       since,
		Until:       until,
		// One row over the page, so "is there another page" is answered
		// without a second COUNT - the same trick the packages listing uses.
		Limit:  pageSize + 1,
		Offset: offset,
	}

	rows, err := s.deps.Packages.ListAudit(r.Context(), filter)
	if err != nil {
		s.internal(w, r, "list audit events", err)
		return
	}

	resp := v1.ListAuditEventsResponse{AuditEvents: []v1.AuditEvent{}}
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		resp.NextPageToken = strconv.Itoa(offset + pageSize)
	}
	for _, e := range rows {
		resp.AuditEvents = append(resp.AuditEvents, auditEventDTO(e))
	}
	WriteJSON(w, r, http.StatusOK, resp)
}

// auditProducts intersects what the caller ASKED for with what they may SEE.
//
// The second return is false when the intersection is empty because the
// request named something outside the caller's visibility - which is a
// different outcome from "no filter", and the caller must not conflate them by
// passing an empty slice to the store, where empty means UNRESTRICTED.
//
// Visibility is applied server-side and cannot be widened by a query
// parameter. That asymmetry is the whole reason authorization filtering lives
// here and never in a client.
//
// Today VisibleProducts is nil for every caller, so this reduces to the query
// filter and nothing changes.
func auditProducts(r *http.Request, requested string) ([]string, bool) {
	visible := middleware.IdentityFrom(r.Context()).VisibleProducts()
	if requested == "" {
		return visible, true
	}
	if len(visible) == 0 {
		return []string{requested}, true
	}
	for _, p := range visible {
		if p == requested {
			return []string{requested}, true
		}
	}
	return nil, false
}

func auditEventDTO(e store.AuditEvent) v1.AuditEvent {
	id := strconv.FormatInt(e.ID, 10)
	out := v1.AuditEvent{
		Name:        "auditEvents/" + id,
		ID:          id,
		OccurredAt:  e.OccurredAt,
		EventType:   e.EventType,
		Actor:       e.Actor,
		ActorKind:   e.ActorKind,
		Product:     e.ProductName,
		SubjectKind: e.SubjectKind,
		SubjectID:   e.SubjectID,
		RequestID:   e.RequestID,
		TraceID:     e.TraceID,
		Outcome:     e.Outcome,
	}
	// Only pass through detail that is actually JSON. A producer that wrote a
	// bare string would otherwise make the whole response unparseable, and one
	// malformed row a year ago should not break the page.
	if e.Detail != "" && json.Valid([]byte(e.Detail)) {
		out.Detail = json.RawMessage(e.Detail)
	}
	return out
}

// parseTimeBound accepts RFC 3339 and normalises to UTC.
//
// Rejected rather than ignored when unparseable: a filter that silently does
// nothing shows the caller a wider trail than they asked for, and they have no
// way to tell.
func parseTimeBound(v, field string) (string, error) {
	if v == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", &badBound{field: field}
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

type badBound struct{ field string }

func (e *badBound) Error() string {
	return e.field + " must be an RFC 3339 timestamp, for example 2026-08-10T10:00:00Z"
}
