package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

// dialects must stay in step with the embed directive.
var dialects = []string{"postgres", "sqlite"}

// validAnnotations is the closed set of goose directives we use.
var validAnnotations = map[string]bool{
	"+goose Up":             true,
	"+goose Down":           true,
	"+goose NO TRANSACTION": true,
	"+goose StatementBegin": true,
	"+goose StatementEnd":   true,
	"+goose ENVSUB ON":      true,
	"+goose ENVSUB OFF":     true,
}

func eachMigration(t *testing.T, fn func(dialect, name, body string)) {
	t.Helper()
	for _, d := range dialects {
		fsys, err := FS(d)
		if err != nil {
			t.Fatalf("FS(%s): %v", d, err)
		}
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		found := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			found = true
			b, err := fs.ReadFile(fsys, e.Name())
			if err != nil {
				t.Fatalf("read %s/%s: %v", d, e.Name(), err)
			}
			fn(d, e.Name(), string(b))
		}
		if !found {
			t.Fatalf("no .sql migrations embedded for %s - the embed directive is wrong", d)
		}
	}
}

// TestNoStrayGooseAnnotations guards a failure mode that cost a real debugging
// session and that no dialect-specific test would catch.
//
// goose inspects EVERY SQL-comment line for its "+goose" marker. A comment
// that merely mentions a directive - for example, explaining when
// NO TRANSACTION should be used - is parsed as a real directive and fails the
// migration at Coordinator startup.
//
// The original bug lived only in the Postgres file, so the SQLite-backed store
// tests passed and the breakage would first have appeared on deployment.
func TestNoStrayGooseAnnotations(t *testing.T) {
	eachMigration(t, func(dialect, name, body string) {
		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "--") {
				continue
			}
			comment := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
			if !strings.Contains(comment, "+goose") {
				continue
			}
			if !validAnnotations[comment] {
				t.Errorf(
					"%s/%s line %d: a comment containing the goose marker will be parsed as a\n"+
						"directive and fail the migration at startup. Rephrase it without the\n"+
						"literal marker.\n  %s",
					dialect, name, i+1, trimmed)
			}
		}
	})
}

// TestMigrationsHaveUpAndDown checks the section markers goose requires.
func TestMigrationsHaveUpAndDown(t *testing.T) {
	eachMigration(t, func(dialect, name, body string) {
		if !strings.Contains(body, "-- +goose Up") {
			t.Errorf("%s/%s: missing the Up marker", dialect, name)
		}
		if !strings.Contains(body, "-- +goose Down") {
			t.Errorf("%s/%s: missing the Down marker - needed for local rollback", dialect, name)
		}
		if strings.Index(body, "-- +goose Up") > strings.Index(body, "-- +goose Down") {
			t.Errorf("%s/%s: Up must precede Down", dialect, name)
		}
	})
}

// TestBothDialectsHaveMatchingFilenames keeps the two dialects in step. A
// migration added to one and forgotten in the other would mean development and
// production diverge silently.
func TestBothDialectsHaveMatchingFilenames(t *testing.T) {
	names := map[string][]string{}
	for _, d := range dialects {
		fsys, err := FS(d)
		if err != nil {
			t.Fatalf("FS(%s): %v", d, err)
		}
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				names[d] = append(names[d], e.Name())
			}
		}
	}
	pg := strings.Join(names["postgres"], ",")
	lite := strings.Join(names["sqlite"], ",")
	if pg != lite {
		t.Fatalf("migration sets differ:\n  postgres: %s\n  sqlite:   %s", pg, lite)
	}
}

func TestMigrationVersionNumbersAreUnique(t *testing.T) {
	for _, d := range dialects {
		seen := map[string]string{}
		fsys, err := FS(d)
		if err != nil {
			t.Fatalf("FS(%s): %v", d, err)
		}
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			prefix := strings.SplitN(e.Name(), "_", 2)[0]
			if prev, ok := seen[prefix]; ok {
				t.Fatalf("duplicate migration version %s in %s: %s and %s", prefix, d, prev, e.Name())
			}
			seen[prefix] = e.Name()
		}
	}
}

// TestFSRejectsUnknownDialect guards the embed sub-path lookup.
func TestFSRejectsUnknownDialect(t *testing.T) {
	fsys, err := FS("mysql")
	if err != nil {
		return // acceptable: rejected outright
	}
	if _, err := fs.ReadDir(fsys, "."); err == nil {
		t.Fatal("expected an unknown dialect to yield no migrations")
	}
}
