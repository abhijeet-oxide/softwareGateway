// Package storetest opens migrated SQLite stores for tests, cheaply.
//
// The schema is built ONCE per test binary and copied per store, rather than
// replayed. Fifty-one migrations cost several seconds each time under the race
// detector, and the packages that use this open a store per test case: the
// store package's own suite spent most of nine minutes on nothing but goose and
// went past Go's ten-minute test timeout without reaching its assertions, and
// the transfer package took seven.
//
// The migrations still run - once here, and again from scratch in the Postgres
// job and the coordinator boot check, which is where a broken migration is
// actually caught. What a test that is not about migrations needs is the
// schema, and a copied file is the same schema by construction.
//
// The store package cannot import this (that would be a cycle), so it carries
// its own copy of the same trick.
package storetest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Open returns a migrated SQLite store on a temp file, closed when the test
// ends. A file rather than :memory: because that is what the deployments and
// the rest of the suite use.
func Open(t *testing.T) store.Store {
	t.Helper()
	return OpenAt(t, filepath.Join(t.TempDir(), "test.db"))
}

// OpenAt is Open at a path the caller chooses, for a test that needs to reopen
// the same database or look at the file.
func OpenAt(t *testing.T, dsn string) store.Store {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dsn), 0o700); err != nil {
		t.Fatalf("create database directory: %v", err)
	}
	if err := os.WriteFile(dsn, schema(t), 0o600); err != nil {
		t.Fatalf("seed schema: %v", err)
	}

	st, err := store.Open(context.Background(), store.Config{
		Driver: store.DriverSQLite, DSN: dsn,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

var (
	buildOnce   sync.Once
	schemaBytes []byte
	buildErr    error
)

// schema returns the bytes of a migrated, empty database.
//
// It is held in MEMORY rather than as a file on the side, so there is nothing
// to clean up: the file it is built from lives in the temp directory of
// whichever test happened to be first, and is read back before that test ends.
// It is also CLOSED before being read - SQLite runs in WAL mode here, and the
// checkpoint on the last connection closing is what folds the write-ahead log
// back into the single file these bytes are.
func schema(t *testing.T) []byte {
	t.Helper()
	buildOnce.Do(func() {
		path := filepath.Join(t.TempDir(), "template.db")

		ctx := context.Background()
		var st store.Store
		if st, buildErr = store.Open(ctx, store.Config{
			Driver: store.DriverSQLite, DSN: path,
		}); buildErr != nil {
			return
		}
		if buildErr = store.Migrate(ctx, st, nil); buildErr != nil {
			_ = st.Close()
			return
		}
		if buildErr = st.Close(); buildErr != nil {
			return
		}
		// #nosec G304 -- path is this test's own t.TempDir(), not input.
		schemaBytes, buildErr = os.ReadFile(path)
	})
	if buildErr != nil {
		t.Fatalf("build schema template: %v", buildErr)
	}
	return schemaBytes
}
