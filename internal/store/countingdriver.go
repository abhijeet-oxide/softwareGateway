package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/querycount"
)

// A driver wrapper that counts round trips, in front of whichever real driver
// this store uses.
//
// # Why the counting is here rather than in the store's methods
//
// Because a store method is not where a round trip happens. The two N+1s this
// repository shipped were both a store method called in a LOOP by a handler -
// each call perfectly reasonable, the loop the mistake - so counting inside
// the method would have counted one. Counting at the driver counts what the
// database actually saw, whatever route the caller took to it, including the
// queries a package other than this one issues.
//
// # What it costs
//
// One type assertion on a context value per statement, and an atomic add when
// somebody is counting. Nothing allocates. It is installed always rather than
// behind a flag, because a counter that is off in production is a counter that
// is off when the question is asked - and the question is always asked in
// production. See internal/platform/querycount.
//
// STATEMENTS ARE WRAPPED AS WELL AS CONNECTIONS, which is the part that is
// easy to get wrong: database/sql prepares and reuses statements on its own,
// so a wrapper that only counted Conn.QueryContext would miss most of the
// traffic on a busy pool and report an N+1 as a single query.

// countingDriverName registers `name` wrapping the driver already registered
// as `wraps`, and returns the name to open.
//
// Registration is once per process and idempotent, because database/sql panics
// on a duplicate and a test binary opens many stores.
func countingDriverName(name, wraps string) (string, error) {
	// PER NAME, not once for the process. A single sync.Once here registers
	// whichever driver is opened first and silently skips the other, so a
	// binary that opens SQLite and then Postgres asks database/sql for a
	// driver nobody registered. Both drivers are opened in one process by the
	// store's own test suite, which is where that was caught.
	registerCounting.Lock()
	defer registerCounting.Unlock()
	if registered[name] {
		return name, nil
	}

	// A throwaway connection to the wrapped driver is how its driver.Driver is
	// obtained: database/sql exposes the registry only through a DB, and the
	// alternative is importing the driver package here and hard-coding which
	// one this is.
	db, err := sql.Open(wraps, "")
	if err != nil {
		return "", fmt.Errorf("wrap the %s driver for query counting: %w", wraps, err)
	}
	inner := db.Driver()
	_ = db.Close()

	sql.Register(name, &countingDriver{inner: inner})
	registered[name] = true
	return name, nil
}

var (
	registerCounting sync.Mutex
	registered       = map[string]bool{}
)

type countingDriver struct{ inner driver.Driver }

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{inner: c}, nil
}

// OpenConnector is what database/sql prefers when the driver offers it, and
// both drivers this wraps do. Without it the DSN is re-parsed per connection.
func (d *countingDriver) OpenConnector(name string) (driver.Connector, error) {
	dc, ok := d.inner.(driver.DriverContext)
	if !ok {
		return &dsnConnector{dsn: name, driver: d}, nil
	}
	inner, err := dc.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return &countingConnector{inner: inner, driver: d}, nil
}

type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c *dsnConnector) Driver() driver.Driver                        { return c.driver }

type countingConnector struct {
	inner  driver.Connector
	driver driver.Driver
}

func (c *countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{inner: conn}, nil
}

func (c *countingConnector) Driver() driver.Driver { return c.driver }

type countingConn struct{ inner driver.Conn }

func (c *countingConn) Prepare(q string) (driver.Stmt, error) {
	s, err := c.inner.Prepare(q)
	if err != nil {
		return nil, err
	}
	return &countingStmt{inner: s}, nil
}

func (c *countingConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	p, ok := c.inner.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(q)
	}
	s, err := p.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	// NOT counted: preparing is not a round trip anybody is drawing a page
	// with, and database/sql reuses one prepared statement across many
	// executions. The executions below are the traffic.
	return &countingStmt{inner: s}, nil
}

func (c *countingConn) Close() error              { return c.inner.Close() }
func (c *countingConn) Begin() (driver.Tx, error) { return c.inner.Begin() } //nolint:staticcheck // driver.Conn requires it

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	b, ok := c.inner.(driver.ConnBeginTx)
	if !ok {
		return c.inner.Begin() //nolint:staticcheck // the pre-context fallback
	}
	return b.BeginTx(ctx, opts)
}

func (c *countingConn) QueryContext(
	ctx context.Context, q string, args []driver.NamedValue,
) (driver.Rows, error) {
	qc, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return querycount.Observe(ctx, func() (driver.Rows, error) {
		return qc.QueryContext(ctx, q, args)
	})
}

func (c *countingConn) ExecContext(
	ctx context.Context, q string, args []driver.NamedValue,
) (driver.Result, error) {
	ec, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return querycount.Observe(ctx, func() (driver.Result, error) {
		return ec.ExecContext(ctx, q, args)
	})
}

func (c *countingConn) Ping(ctx context.Context) error {
	p, ok := c.inner.(driver.Pinger)
	if !ok {
		return nil
	}
	// Deliberately not counted: a health check is not work a page did.
	return p.Ping(ctx)
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	s, ok := c.inner.(driver.SessionResetter)
	if !ok {
		return nil
	}
	return s.ResetSession(ctx)
}

func (c *countingConn) IsValid() bool {
	v, ok := c.inner.(driver.Validator)
	return !ok || v.IsValid()
}

type countingStmt struct{ inner driver.Stmt }

func (s *countingStmt) Close() error  { return s.inner.Close() }
func (s *countingStmt) NumInput() int { return s.inner.NumInput() }

func (s *countingStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.inner.Exec(args) //nolint:staticcheck // driver.Stmt requires it
}

func (s *countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.inner.Query(args) //nolint:staticcheck // driver.Stmt requires it
}

func (s *countingStmt) ExecContext(
	ctx context.Context, args []driver.NamedValue,
) (driver.Result, error) {
	e, ok := s.inner.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return querycount.Observe(ctx, func() (driver.Result, error) {
		return e.ExecContext(ctx, args)
	})
}

func (s *countingStmt) QueryContext(
	ctx context.Context, args []driver.NamedValue,
) (driver.Rows, error) {
	q, ok := s.inner.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return querycount.Observe(ctx, func() (driver.Rows, error) {
		return q.QueryContext(ctx, args)
	})
}
