package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// THE INCIDENT THIS PREVENTS.
//
// A query with a bug in it - `COALESCE(<jsonb>, ”)`, which Postgres answers
// 22P02 to - was reported as UNAVAILABLE. 503 is the status this API reserves
// for a service that is standing down, so the interface drew an outage over
// the whole application for a fault on one tab, and its recovery loop then
// refetched into the same failure several times a minute.
//
// A statement the database refused must be 500. Only the database being
// UNREACHABLE is 503, because only that is a claim a client may act on by
// waiting.
func TestAFailedStatementIsNotReportedAsAnOutage(t *testing.T) {
	s := &Server{deps: Deps{Logger: slog.New(slog.DiscardHandler)}}

	// Exactly what the incident produced.
	refused := &pgconn.PgError{
		Code:    "22P02",
		Message: `invalid input syntax for type json`,
	}

	rec := httptest.NewRecorder()
	s.fault(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil),
		"could not list the files", refused)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a refused statement answered %d, want 500", rec.Code)
	}
	var problem v1.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != v1.CodeInternal {
		t.Fatalf("code %q, want %q", problem.Code, v1.CodeInternal)
	}
	// The cause is what made this diagnosable from a screenshot. Keep it.
	if !strings.Contains(problem.Detail, "could not list the files") ||
		!strings.Contains(problem.Detail, "invalid input syntax") {
		t.Fatalf("detail %q lost either the operation or the cause", problem.Detail)
	}
}

// The other half: a database that is genuinely not there IS an outage, and
// saying 500 for it would send an operator hunting for a bug in a query that
// never ran.
func TestAnUnreachableDatabaseIsReportedAsAnOutage(t *testing.T) {
	s := &Server{deps: Deps{Logger: slog.New(slog.DiscardHandler)}}

	rec := httptest.NewRecorder()
	s.fault(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil),
		"could not list transfers", driver.ErrBadConn)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable database answered %d, want 503", rec.Code)
	}
}

// What `store.Unreachable` must and must not call an outage, listed here
// rather than in the store because this is where the consequence lands.
func TestOnlyConnectionFailuresAreOutages(t *testing.T) {
	outage := []struct {
		name string
		err  error
	}{
		{"bad connection", driver.ErrBadConn},
		{"connection already closed", sql.ErrConnDone},
		{"socket refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
		{"class 08", &pgconn.PgError{Code: "08006", Message: "connection failure"}},
		{"admin shutdown", &pgconn.PgError{Code: "57P01", Message: "terminating connection"}},
		{"too many connections", &pgconn.PgError{Code: "53300"}},
	}
	for _, tc := range outage {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s := &Server{deps: Deps{Logger: slog.New(slog.DiscardHandler)}}
			s.fault(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "read", tc.err)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s answered %d, want 503", tc.name, rec.Code)
			}
		})
	}

	ours := []struct {
		name string
		err  error
	}{
		{"bad json literal", &pgconn.PgError{Code: "22P02"}},
		{"syntax error", &pgconn.PgError{Code: "42601"}},
		{"undefined column", &pgconn.PgError{Code: "42703"}},
		{"unique violation", &pgconn.PgError{Code: "23505"}},
		{"a plain error", errors.New("something went wrong")},
		// A slow query is not an absent server, and calling it one would hand
		// the same lie back under a different name.
		{"a deadline", context.DeadlineExceeded},
	}
	for _, tc := range ours {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s := &Server{deps: Deps{Logger: slog.New(slog.DiscardHandler)}}
			s.fault(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "read", tc.err)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("%s answered %d, want 500", tc.name, rec.Code)
			}
		})
	}
}
