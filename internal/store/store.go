// Package store owns persistence: SQL, both dialects, and transaction
// boundaries.
//
// See docs/design/03-persistence.md.
//
// The package owns no business logic. A query returns rows; it does not decide
// what they mean. That rule is what stops the same invariant being implemented
// three times, slightly differently, in three places.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Driver names the supported dialects.
type Driver string

const (
	DriverPostgres Driver = "postgres"
	DriverSQLite   Driver = "sqlite"
)

// Store is the persistence interface.
//
// M1 needs open, ping, migrate and the advisory lock. Domain queries land in
// M2/M3 alongside the features that need them - generating code for four
// statements would be ceremony.
type Store interface {
	// DB exposes the database/sql handle. Used by the migration runner and by
	// leader election, both of which need a raw connection.
	DB() *sql.DB

	// Driver reports the dialect, so callers can select dialect-specific SQL.
	Driver() Driver

	// Ping verifies connectivity. Backs the readiness probe.
	Ping(context.Context) error

	// Stats reports pool statistics for the deep health check.
	Stats() sql.DBStats

	// SchemaVersion returns the highest applied migration.
	SchemaVersion(context.Context) (int64, error)

	// SupportsAdvisoryLocks reports whether leader election is meaningful.
	// False for SQLite, where a single process has nothing to contend with.
	SupportsAdvisoryLocks() bool

	Close() error
}

// Config opens a store.
type Config struct {
	Driver          Driver
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Open returns a store for the configured driver.
func Open(ctx context.Context, cfg Config) (Store, error) {
	switch cfg.Driver {
	case DriverPostgres:
		return openPostgres(ctx, cfg)
	case DriverSQLite:
		return openSQLite(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Driver)
	}
}

// applyPoolSettings is shared by both drivers.
func applyPoolSettings(db *sql.DB, cfg Config) {
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
}
