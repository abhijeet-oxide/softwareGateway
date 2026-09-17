package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Unreachable reports whether an error means THE DATABASE COULD NOT BE REACHED,
// as opposed to a statement it received and refused.
//
// # Why anything needs to tell those apart
//
// Because the two are opposite answers to "is this service serving?", and
// until this existed every handler gave the same one. A failed read was
// reported as UNAVAILABLE - 503, which this API's own contract reserves for a
// service that is standing down - so a query with a bug in it arrived at a
// browser wearing the clothes of a maintenance window.
//
// That is not a cosmetic difference. A 503 is the one status a client may
// treat as "nothing is wrong with the request, come back shortly": the
// interface's connection monitor reads it as an outage and the boot screen
// draws a maintenance page for it. A malformed query answered 503 on one
// endpoint therefore produced an application-wide outage that recovery could
// never resolve, because the next attempt failed identically. See
// docs/design/33-availability-and-failure-reporting.md.
//
// # What counts
//
// Connection-level failures only: a refused or dropped socket, a pool whose
// connections are all bad, a server shutting down or refusing new sessions.
// Everything else - a syntax error, a constraint, a type the column will not
// take - is this service's own fault and is reported as such, because it will
// fail the same way on every retry and a person needs to be told rather than
// asked to wait.
//
// A timeout is deliberately NOT here. A statement that ran out of time is
// usually a slow query rather than an absent server, and calling it an outage
// would hand the same lie back under a different name.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}

	// OUR OWN CLOCK, FIRST. A context that ran out is the caller giving up,
	// and it satisfies net.Error - so without this it would fall into the
	// socket branch below and a slow query would be filed as an absent server,
	// which is the exact lie this function exists to stop telling.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	// database/sql's own signals that the connection, rather than the
	// statement, is what went wrong.
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}

	// The socket. A closed connection surfaces as an EOF from the driver
	// rather than as anything more descriptive.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// The socket's own timeout is a different thing from the one above: a dial
	// that never completed is a server that is not there.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}

	// What Postgres itself said. The SQLSTATE classes below are the ones that
	// mean the server will not hold a session right now; everything else it
	// answers is a statement it understood and rejected.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		// Class 08 - connection exception.
		case strings.HasPrefix(pgErr.Code, "08"):
			return true
		// 57P01 admin shutdown, 57P02 crash shutdown, 57P03 cannot connect now.
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03":
			return true
		// 53300 too many connections, 53400 configuration limit exceeded. The
		// server is up and is turning this replica away, which is the same
		// fact from the caller's side.
		case pgErr.Code == "53300", pgErr.Code == "53400":
			return true
		}
		return false
	}

	return false
}
