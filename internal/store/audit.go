package store

import (
	"context"
	"fmt"
)

// Reading the audit trail.
//
// The writing half is InsertAudit in packages.go, which has existed since the
// first migration; nothing has ever read these rows back. A year of history
// that no interface can show is a year of history nobody can use, so this is
// the read side of a table that was already being filled.

// AuditEvent is one recorded event, as a reader sees it.
//
// Timestamps are strings for the reason every other row in this package uses
// strings: the two dialects render them differently and the API serializes
// RFC 3339 either way, so parsing here would buy nothing and lose the
// dialect's own formatting.
type AuditEvent struct {
	ID          int64
	OccurredAt  string
	EventType   string
	Actor       string
	ActorKind   string
	ProductName string
	SubjectKind string
	SubjectID   string
	RequestID   string
	TraceID     string
	Outcome     string
	// Detail is the event's JSON payload, verbatim. Passed through rather than
	// decoded: every producer writes a different shape, and a reader that
	// insisted on knowing them all would have to be edited for each new event
	// type.
	Detail string
}

// AuditFilter narrows the trail.
//
// Every field is optional and they compose with AND. This struct is also the
// single place a future authorization scope is applied — a caller who may see
// two products has those two products set here, server-side, and every query
// below is scoped without touching the SQL. See docs/design/19 and the UI
// plan's forward-design section: authorization filtering happens here, never
// in a client.
type AuditFilter struct {
	// Products limits the trail to these product names. Empty means every
	// product, which is what an unauthenticated deployment gets today.
	Products []string
	// EventType, Actor and Outcome are exact matches. Empty means any.
	EventType string
	Actor     string
	Outcome   string
	// SubjectKind and SubjectID pin the trail to one thing — a package, a
	// transfer — which is what "history of this release" is.
	SubjectKind string
	SubjectID   string
	// Since and Until are RFC 3339 bounds, inclusive of Since and exclusive of
	// Until. Empty means unbounded.
	Since string
	Until string

	Limit  int
	Offset int
}

// ListAudit returns matching events, newest first.
func (p *Packages) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error) {
	query := `
		SELECT a.id, a.occurred_at, a.event_type, a.actor, a.actor_kind,
		       COALESCE(a.product_name, ''), COALESCE(a.subject_kind, ''),
		       COALESCE(a.subject_id, ''), COALESCE(a.request_id, ''),
		       COALESCE(a.trace_id, ''), a.outcome, COALESCE(a.detail, '')
		  FROM audit_events a
		 WHERE 1 = 1`
	var args []any

	if len(f.Products) > 0 {
		query += " AND a.product_name IN (" + placeholders(len(f.Products)) + ")"
		for _, name := range f.Products {
			args = append(args, name)
		}
	}
	for _, c := range []struct {
		column string
		value  string
	}{
		{"a.event_type", f.EventType},
		{"a.actor", f.Actor},
		{"a.outcome", f.Outcome},
		{"a.subject_kind", f.SubjectKind},
		{"a.subject_id", f.SubjectID},
	} {
		if c.value != "" {
			query += " AND " + c.column + " = ?"
			args = append(args, c.value)
		}
	}
	if f.Since != "" {
		query += " AND a.occurred_at >= ?"
		args = append(args, f.Since)
	}
	if f.Until != "" {
		query += " AND a.occurred_at < ?"
		args = append(args, f.Until)
	}

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	// id breaks ties: audit_events is partitioned by occurred_at and several
	// events written in one transaction share a timestamp to the microsecond,
	// so occurred_at alone is not a total order and a page boundary could
	// repeat or drop a row.
	query += " ORDER BY a.occurred_at DESC, a.id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, max(f.Offset, 0))

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.EventType, &e.Actor, &e.ActorKind,
			&e.ProductName, &e.SubjectKind, &e.SubjectID, &e.RequestID,
			&e.TraceID, &e.Outcome, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// placeholders renders `?, ?, ?` for an IN clause, before dialect rewriting.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	s := make([]byte, 0, n*3)
	for i := range n {
		if i > 0 {
			s = append(s, ',', ' ')
		}
		s = append(s, '?')
	}
	return string(s)
}
