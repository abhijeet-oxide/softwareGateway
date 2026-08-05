package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/db/migrations"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/statemachine"
)

// openTestStore returns a migrated SQLite store on a temp file.
//
// A file rather than :memory: because MaxOpenConns(1) plus in-memory would be
// fine, but a file also exercises the directory-creation path that the
// zero-setup development flow depends on.
func openTestStore(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()

	s, err := Open(ctx, Config{
		Driver: DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := Migrate(ctx, s, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrationsCreateEverySchemaTable(t *testing.T) {
	s := openTestStore(t)

	got := map[string]bool{}
	for _, n := range tableNames(t, s.DB()) {
		got[n] = true
	}

	// The 17 tables from docs/design/03 sections 4-7.
	want := []string{
		"products", "repositories",
		"packages", "package_artifacts", "blobs", "artifact_blobs", "blob_placements",
		"transfer_requests", "transfers", "jobs", "scheduled_requests",
		"verifications", "notifications", "audit_events",
		"workers", "repository_budgets", "worker_logs",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing table %q", w)
		}
	}
	if len(want) != 17 {
		t.Fatalf("the design specifies 17 tables, this list has %d", len(want))
	}
}

func TestMigrationsCreateThePartialIndexes(t *testing.T) {
	// The partial dequeue index is the difference between a queue that stays
	// fast and one that degrades: without WHERE state='pending' it carries
	// every job ever created.
	s := openTestStore(t)

	rows, err := s.DB().QueryContext(t.Context(),
		`SELECT name, COALESCE(sql,'') FROM sqlite_master WHERE type='index' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	idx := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		idx[name] = ddl
	}

	for _, name := range []string{
		"jobs_dequeue_idx", "jobs_lease_expiry_idx", "jobs_inflight_blob_idx",
		"blob_placements_verified_idx", "notifications_outbox_idx",
		"scheduled_requests_due_idx", "repositories_physical_idx",
	} {
		if _, ok := idx[name]; !ok {
			t.Errorf("missing index %q", name)
		}
	}

	if ddl := idx["jobs_dequeue_idx"]; !strings.Contains(strings.ToUpper(ddl), "WHERE") {
		t.Errorf("jobs_dequeue_idx must be PARTIAL, got: %s", ddl)
	}
	if ddl := idx["jobs_dequeue_idx"]; strings.Contains(ddl, "wave") {
		t.Errorf("jobs_dequeue_idx must not include wave — gating is resolved into state: %s", ddl)
	}
}

// TestCheckConstraintsMatchStateMachines is the guard that keeps the schema
// and the state machines from drifting apart. The database must accept exactly
// the states the machine defines, and reject everything else.
func TestCheckConstraintsMatchStateMachines(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed the minimum rows needed to satisfy foreign keys.
	mustExec(t, s.DB(), `INSERT INTO products (id,name,config_hash,config) VALUES (1,'p','h','{}')`)
	mustExec(t, s.DB(), `INSERT INTO repositories (id,product_id,role,name,registry_host,repository_path)
	                     VALUES (1,1,'source','src','reg.example.com','a/b'), (2,1,'target','dst','reg2.example.com','c/d')`)
	mustExec(t, s.DB(), `INSERT INTO packages (id,product_id,source_repo_id,tag,manifest_digest,media_type)
	                     VALUES (1,1,1,'v1','sha256:aa','application/json')`)
	mustExec(t, s.DB(), `INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
	                     VALUES ('r1',1,1,'replicate',1,'k1')`)
	mustExec(t, s.DB(), `INSERT INTO transfers (id,request_id,package_id,source_repo_id,target_repo_id)
	                     VALUES ('t1','r1',1,1,2)`)

	t.Run("jobs accepts every machine state", func(t *testing.T) {
		for _, st := range statemachine.Job.States() {
			if st == statemachine.JobStateZero {
				continue // pseudo-state for creation; never persisted
			}
			_, err := s.DB().ExecContext(ctx,
				`INSERT INTO jobs (transfer_id,kind,digest,source_repo_id,target_repo_id,state)
				 VALUES ('t1','blob',?,1,2,?)`,
				"sha256:"+string(st), string(st))
			if err != nil {
				t.Errorf("state %q is defined by the machine but rejected by the CHECK constraint: %v", st, err)
			}
		}
	})

	t.Run("jobs rejects an undefined state", func(t *testing.T) {
		_, err := s.DB().ExecContext(ctx,
			`INSERT INTO jobs (transfer_id,kind,digest,source_repo_id,target_repo_id,state)
			 VALUES ('t1','blob','sha256:bogus',1,2,'teleporting')`)
		if err == nil {
			t.Fatal("the database must reject a state the state machine does not define")
		}
		if !strings.Contains(strings.ToUpper(err.Error()), "CONSTRAINT") {
			t.Fatalf("expected a constraint violation, got: %v", err)
		}
	})

	t.Run("transfers accepts every machine state", func(t *testing.T) {
		for _, st := range statemachine.Transfer.States() {
			if st == statemachine.TransferStateZero {
				continue
			}
			_, err := s.DB().ExecContext(ctx,
				`UPDATE transfers SET state=? WHERE id='t1'`, string(st))
			if err != nil {
				t.Errorf("transfer state %q is rejected by the CHECK constraint: %v", st, err)
			}
		}
	})

	t.Run("packages accepts every machine state", func(t *testing.T) {
		for _, st := range statemachine.Package.States() {
			if st == statemachine.PackageStateZero {
				continue
			}
			if _, err := s.DB().ExecContext(ctx, `UPDATE packages SET state=? WHERE id=1`, string(st)); err != nil {
				t.Errorf("package state %q is rejected by the CHECK constraint: %v", st, err)
			}
		}
	})

	t.Run("verifications accepts every machine state", func(t *testing.T) {
		mustExec(t, s.DB(), `INSERT INTO verifications (id,package_id,repository_id,stage,policy,subject_digest)
		                     VALUES (1,1,2,'destination','enforce','sha256:aa')`)
		for _, st := range statemachine.Verification.States() {
			if st == statemachine.VerificationStateZero {
				continue
			}
			if _, err := s.DB().ExecContext(ctx, `UPDATE verifications SET state=? WHERE id=1`, string(st)); err != nil {
				t.Errorf("verification state %q is rejected by the CHECK constraint: %v", st, err)
			}
		}
	})
}

func TestIdempotencyConstraintsAreStructural(t *testing.T) {
	// Idempotency is enforced by unique indexes rather than by application
	// logic, because check-then-insert has a race and a unique index does not.
	s := openTestStore(t)

	mustExec(t, s.DB(), `INSERT INTO products (id,name,config_hash,config) VALUES (1,'p','h','{}')`)
	mustExec(t, s.DB(), `INSERT INTO repositories (id,product_id,role,name,registry_host,repository_path)
	                     VALUES (1,1,'source','src','reg.example.com','a/b')`)

	// Invariant I4: a re-scan can never create a duplicate package.
	mustExec(t, s.DB(), `INSERT INTO packages (product_id,source_repo_id,tag,manifest_digest,media_type)
	                     VALUES (1,1,'v1','sha256:aa','application/json')`)
	_, err := s.DB().ExecContext(t.Context(), `INSERT INTO packages (product_id,source_repo_id,tag,manifest_digest,media_type)
	                       VALUES (1,1,'v1','sha256:aa','application/json')`)
	if err == nil {
		t.Fatal("a duplicate (source_repo, tag, digest) must be rejected — this is invariant I4")
	}

	// A re-push of the same tag with DIFFERENT content is a new package, not a
	// duplicate: identity includes the digest.
	mustExec(t, s.DB(), `INSERT INTO packages (product_id,source_repo_id,tag,manifest_digest,media_type)
	                     VALUES (1,1,'v1','sha256:bb','application/json')`)

	// Invariant I3: a duplicate request creates no additional work.
	mustExec(t, s.DB(), `INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
	                     VALUES ('r1',1,1,'replicate',1,'same-key')`)
	_, err = s.DB().ExecContext(t.Context(), `INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
	                      VALUES ('r2',1,1,'replicate',1,'same-key')`)
	if err == nil {
		t.Fatal("a duplicate idempotency key must be rejected — this is invariant I3")
	}
}

func TestPhysicalRepositoryIsUnique(t *testing.T) {
	// One physical repository is one row, so blob placements are shared across
	// products. Keying by config entry would silently forfeit cross-product
	// deduplication.
	s := openTestStore(t)
	mustExec(t, s.DB(), `INSERT INTO products (id,name,config_hash,config) VALUES (1,'a','h1','{}'),(2,'b','h2','{}')`)
	mustExec(t, s.DB(), `INSERT INTO repositories (product_id,role,name,registry_host,repository_path)
	                     VALUES (1,'target','lab','internal.azurecr.io','shared/base')`)

	_, err := s.DB().ExecContext(t.Context(), `INSERT INTO repositories (product_id,role,name,registry_host,repository_path)
	                       VALUES (2,'target','lab','internal.azurecr.io','shared/base')`)
	if err == nil {
		t.Fatal("two products pointing at the same physical repository must share one row")
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	// SQLite has foreign keys OFF by default; without the pragma the
	// REFERENCES clauses would be silently inert and development would not
	// exercise the constraints production enforces.
	s := openTestStore(t)
	_, err := s.DB().ExecContext(t.Context(), `INSERT INTO packages (product_id,source_repo_id,tag,manifest_digest,media_type)
	                       VALUES (999,999,'v1','sha256:aa','application/json')`)
	if err == nil {
		t.Fatal("foreign keys must be enforced; check the _pragma foreign_keys(1) DSN option")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	v1, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == 0 {
		t.Fatal("expected a non-zero schema version after migration")
	}

	// Re-running must be a no-op, not an error: every Coordinator runs this at
	// startup, and a rolling deployment runs several at once.
	if err := Migrate(ctx, s, nil); err != nil {
		t.Fatalf("second migrate should be a no-op: %v", err)
	}
	v2, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("schema version changed on re-run: %d -> %d", v1, v2)
	}
}

func TestSQLiteReportsNoAdvisoryLocks(t *testing.T) {
	s := openTestStore(t)
	if s.SupportsAdvisoryLocks() {
		t.Fatal("SQLite has no advisory locks; leader election must take the unconditional path")
	}
	if s.Driver() != DriverSQLite {
		t.Fatalf("got driver %q", s.Driver())
	}
}

func TestOpenRejectsUnknownDriver(t *testing.T) {
	_, err := Open(context.Background(), Config{Driver: "mysql", DSN: "x"})
	if err == nil {
		t.Fatal("expected an error for an unsupported driver")
	}
}

func TestBothDialectsDefineTheSameTables(t *testing.T) {
	// The logical schema must be identical across dialects. A table present in
	// one and missing from the other would mean development exercises a
	// different system from production.
	pg := readMigration(t, "postgres")
	lite := readMigration(t, "sqlite")

	pgTables := extractTables(pg)
	liteTables := extractTables(lite)

	// Guard against the comparison passing trivially: two empty slices are
	// equal, and a broken regexp or embed path would produce exactly that.
	const wantTables = 17
	if len(pgTables) != wantTables {
		t.Fatalf("extracted %d postgres tables, expected %d: %v", len(pgTables), wantTables, pgTables)
	}

	sort.Strings(pgTables)
	sort.Strings(liteTables)

	if strings.Join(pgTables, ",") != strings.Join(liteTables, ",") {
		t.Fatalf("dialects define different tables:\n postgres: %v\n sqlite:   %v", pgTables, liteTables)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// readMigration returns the embedded migration SQL for a dialect, reading it
// through the same embed.FS the Coordinator uses — so this test cannot pass
// against files that would not actually ship in the binary.
func readMigration(t *testing.T, dialect string) string {
	t.Helper()

	fsys, err := migrations.FS(dialect)
	if err != nil {
		t.Fatalf("open embedded migrations for %s: %v", dialect, err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("read %s migrations: %v", dialect, err)
	}

	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			t.Fatalf("read %s/%s: %v", dialect, e.Name(), err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		t.Fatalf("no migrations embedded for %s — the embed directive is wrong", dialect)
	}
	return sb.String()
}

// createTableRE matches CREATE TABLE statements in the Up section. The
// optional IF NOT EXISTS and the quoting variants are permitted so a later
// migration written in a different style is still detected.
var createTableRE = regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["'` + "`" + `]?([a-z_][a-z0-9_]*)["'` + "`" + `]?`)

// extractTables returns the table names declared by a migration, excluding
// anything after the goose Down marker — otherwise the DROP section would be
// scanned too and the comparison would be meaningless.
func extractTables(sql string) []string {
	if i := strings.Index(sql, "-- +goose Down"); i >= 0 {
		sql = sql[:i]
	}

	seen := map[string]bool{}
	var out []string
	for _, m := range createTableRE.FindAllStringSubmatch(sql, -1) {
		name := strings.ToLower(m[1])
		// Postgres declares a DEFAULT partition of audit_events; it is an
		// implementation detail of partitioning, not a logical table, so it
		// must not make the dialects look different.
		if name == "audit_events_default" {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
