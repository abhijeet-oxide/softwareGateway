package store

import (
	"context"
	"database/sql"
	"fmt"

	// Registers the pgx driver with database/sql. The Coordinator uses
	// database/sql here because goose and the advisory-lock elector both need
	// it; pgxpool arrives in M3 for the hot dequeue path, where the native
	// protocol earns its place.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

type postgresStore struct {
	db *sql.DB
}

func openPostgres(ctx context.Context, cfg Config) (Store, error) {
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	applyPoolSettings(db, cfg)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return &postgresStore{db: db}, nil
}

func (s *postgresStore) DB() *sql.DB                    { return s.db }
func (s *postgresStore) Driver() Driver                 { return DriverPostgres }
func (s *postgresStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *postgresStore) Stats() sql.DBStats             { return s.db.Stats() }
func (s *postgresStore) SupportsAdvisoryLocks() bool    { return true }
func (s *postgresStore) Close() error                   { return s.db.Close() }

func (s *postgresStore) SchemaVersion(ctx context.Context) (int64, error) {
	return schemaVersion(ctx, s.db, string(DriverPostgres))
}

func schemaVersion(ctx context.Context, db *sql.DB, dialect string) (int64, error) {
	if err := goose.SetDialect(dialect); err != nil {
		return 0, fmt.Errorf("set goose dialect: %w", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}
