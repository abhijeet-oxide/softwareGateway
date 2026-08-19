package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	// modernc.org/sqlite is PURE GO. mattn/go-sqlite3 requires cgo, which
	// would break the CGO_ENABLED=0 distroless build - a development
	// convenience must not compromise the production image.
	// See docs/design/16-technology-choices.md section 3.
	_ "modernc.org/sqlite"
)

type sqliteStore struct {
	db *sql.DB
}

func openSQLite(ctx context.Context, cfg Config) (Store, error) {
	dsn, err := sqliteDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite serializes writers. More than one open connection buys nothing
	// and turns lock contention into SQLITE_BUSY errors, so the pool is
	// deliberately a single connection. This is also why BEGIN IMMEDIATE is
	// already exclusive and FOR UPDATE SKIP LOCKED is unnecessary rather than
	// merely unavailable - see docs/design/03 section 2.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to sqlite: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

// sqliteDSN normalises a path or URI and applies the pragmas the schema needs.
//
// foreign_keys is OFF by default in SQLite, so without this the ON DELETE
// CASCADE and REFERENCES clauses in the migration would be silently inert -
// development would not exercise the constraints production enforces.
func sqliteDSN(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("database.dsn is required for sqlite")
	}

	// In-memory databases are used by tests and need no directory.
	if strings.HasPrefix(raw, ":memory:") || strings.Contains(raw, "mode=memory") {
		return withPragmas(raw), nil
	}

	path := strings.TrimPrefix(raw, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}
	return withPragmas(raw), nil
}

func withPragmas(dsn string) string {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "foreign_keys(1)")
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "busy_timeout(5000)")

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + pragmas.Encode()
}

func (s *sqliteStore) DB() *sql.DB                    { return s.db }
func (s *sqliteStore) Driver() Driver                 { return DriverSQLite }
func (s *sqliteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *sqliteStore) Stats() sql.DBStats             { return s.db.Stats() }
func (s *sqliteStore) Close() error                   { return s.db.Close() }

// SupportsAdvisoryLocks is false: a single-process development store has no
// other replica to contend with, so leader election is unconditional.
func (s *sqliteStore) SupportsAdvisoryLocks() bool { return false }

func (s *sqliteStore) SchemaVersion(ctx context.Context) (int64, error) {
	return schemaVersion(ctx, s.db, string(DriverSQLite))
}
