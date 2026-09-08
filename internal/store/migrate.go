package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/abhijeet-oxide/softwareGateway/db/migrations"
)

// migrationLockID is a distinct advisory-lock key from the leader-election
// lock. Sharing one would mean a replica running migrations could not become
// leader, and vice versa.
const migrationLockID int64 = 424242

// Migrate applies all pending migrations.
//
// On Postgres it runs under an advisory lock so that two replicas starting
// simultaneously - which is the normal case during a rolling deployment -
// cannot race. The second waits, then finds nothing to do.
func Migrate(ctx context.Context, s Store, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	fsys, err := migrations.FS(string(s.Driver()))
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	goose.SetBaseFS(fsys)
	goose.SetLogger(gooseLogger{logger})
	if err := goose.SetDialect(string(s.Driver())); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	db := s.DB()

	if s.SupportsAdvisoryLocks() {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("open migration connection: %w", err)
		}
		defer func() { _ = conn.Close() }()

		// Blocking lock, not try-lock: a replica that loses the race should
		// wait for the winner to finish rather than starting up against a
		// half-migrated schema.
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(releaseCtx, "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
				logger.Warn("migrate: releasing lock failed; connection close will release", "error", err)
			}
		}()
	}

	before, _ := goose.GetDBVersionContext(ctx, db)

	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if after == before {
		logger.Info("migrate: schema up to date", "version", after, "driver", string(s.Driver()))
	} else {
		logger.Info("migrate: applied migrations",
			"from", before, "to", after, "driver", string(s.Driver()))
	}
	return nil
}

// MigrateDown rolls back one migration. Development only - production is
// forward-only, per docs/design/03 section 9.
func MigrateDown(ctx context.Context, s Store) error {
	fsys, err := migrations.FS(string(s.Driver()))
	if err != nil {
		return err
	}
	goose.SetBaseFS(fsys)
	if err := goose.SetDialect(string(s.Driver())); err != nil {
		return err
	}
	return goose.DownContext(ctx, s.DB(), ".")
}

// Status prints applied and pending migrations.
func Status(ctx context.Context, s Store) error {
	fsys, err := migrations.FS(string(s.Driver()))
	if err != nil {
		return err
	}
	goose.SetBaseFS(fsys)
	if err := goose.SetDialect(string(s.Driver())); err != nil {
		return err
	}
	return goose.StatusContext(ctx, s.DB(), ".")
}

// gooseLogger routes goose output through slog rather than stdout, so
// migration output carries the same correlation keys as everything else.
type gooseLogger struct{ log *slog.Logger }

func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info(fmt.Sprintf(format, v...), "source", "goose")
}

func (g gooseLogger) Fatalf(format string, v ...any) {
	g.log.Error(fmt.Sprintf(format, v...), "source", "goose")
}

// ExpectedSchemaVersion is the highest migration EMBEDDED IN THIS BINARY.
//
// Compared against SchemaVersion, it answers the question a readiness probe
// actually has to ask: not "is the database reachable" but "is it the shape
// this code was written against". A replica whose schema is behind reaches a
// working database and then fails on the first query that names a column
// nobody has added yet, which surfaces as a scattering of 500s rather than as
// a replica that never went ready.
//
// It reads the embedded filenames rather than asking goose, deliberately.
// goose's dialect and base filesystem are PROCESS-GLOBAL, so calling into it
// from a probe that runs on every readiness interval would race whatever else
// is migrating. Filenames are pure.
func ExpectedSchemaVersion(driver Driver) (int64, error) {
	fsys, err := migrations.FS(string(driver))
	if err != nil {
		return 0, fmt.Errorf("load embedded migrations: %w", err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var highest int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// goose names a migration "<version>_<description>.sql".
		digits := e.Name()
		if i := strings.IndexByte(digits, '_'); i > 0 {
			digits = digits[:i]
		}
		v, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			// Not a versioned migration. Skipped rather than fatal: goose
			// itself ignores anything it cannot parse a version out of.
			continue
		}
		if v > highest {
			highest = v
		}
	}
	if highest == 0 {
		return 0, fmt.Errorf("no versioned migrations embedded for %s", driver)
	}
	return highest, nil
}
