package store

import (
	"strings"
	"testing"
)

func TestDialectRewritesPlaceholders(t *testing.T) {
	pg := DialectFor(DriverPostgres)
	if got := pg.Rewrite("SELECT ? , ? FROM t WHERE x = ?"); got != "SELECT $1 , $2 FROM t WHERE x = $3" {
		t.Fatalf("got %q", got)
	}
	// A `?` inside a string literal is data, not a placeholder - otherwise a
	// LIKE pattern would silently shift every subsequent parameter number.
	if got := pg.Rewrite("SELECT ? WHERE name LIKE 'a?b'"); got != "SELECT $1 WHERE name LIKE 'a?b'" {
		t.Fatalf("literal-quoted ? must be preserved, got %q", got)
	}
	if got := pg.Rewrite(`SELECT ? WHERE s = 'it''s ?'`); got != `SELECT $1 WHERE s = 'it''s ?'` {
		t.Fatalf("escaped quotes must not end the literal, got %q", got)
	}
	if got := DialectFor(DriverSQLite).Rewrite("SELECT ?"); got != "SELECT ?" {
		t.Fatalf("sqlite rewrite should be identity, got %q", got)
	}
}

func TestDialectNowAndBool(t *testing.T) {
	if DialectFor(DriverPostgres).Bool(true) != "TRUE" || DialectFor(DriverSQLite).Bool(true) != "1" {
		t.Fatal("boolean literals differ by dialect")
	}
	if DialectFor(DriverPostgres).Now() == DialectFor(DriverSQLite).Now() {
		t.Fatal("now() expressions should differ by dialect")
	}
}

// The bug this exists for.
//
// The queries in this package are commented in prose, and English prose has
// apostrophes: "the package's own repository", "the transfer's origin". Each of
// those read as the start of a string literal, so an ODD number of them left the
// rewriter believing the rest of the query was inside one - and every `?` after
// the last apostrophe was passed to Postgres verbatim.
//
// The transfer projection had three. `LIMIT ? OFFSET ?` reached the server
// unchanged and it answered `syntax error at or near "OFFSET"`, so listing and
// reading transfers could not work on Postgres at all. SQLite, whose Rewrite is
// the identity function, was unaffected - which is why nothing caught it.
func TestRewriteIgnoresApostrophesInComments(t *testing.T) {
	pg := postgresDialect{}

	for name, query := range map[string]string{
		"one line comment": `SELECT a -- the package's name
			FROM t WHERE x = ? LIMIT ? OFFSET ?`,
		"an odd number across several": `SELECT a
			-- the package's OWN repository, which is not the transfer's origin
			-- reading the package's name from it said the wrong thing
			FROM t WHERE x = ? LIMIT ? OFFSET ?`,
		"a block comment": `SELECT a /* the worker's lease */ FROM t
			WHERE x = ? LIMIT ? OFFSET ?`,
		"a comment with no trailing newline": "SELECT ? -- the package's name",
	} {
		t.Run(name, func(t *testing.T) {
			got := pg.Rewrite(query)
			if strings.Contains(got, "?") {
				t.Errorf("a placeholder was left unrewritten, so Postgres sees a bare "+
					"`?` and refuses the statement:\n%s", got)
			}
		})
	}
}

// The behaviour the comment handling must not cost: a `?` inside a real string
// literal is data, not a placeholder. LIKE patterns contain them.
func TestRewriteStillSkipsRealStringLiterals(t *testing.T) {
	pg := postgresDialect{}

	got := pg.Rewrite(`SELECT a FROM t WHERE name LIKE '%?%' AND x = ? AND y = ?`)
	if !strings.Contains(got, `'%?%'`) {
		t.Errorf("a `?` inside a string literal was rewritten as a placeholder:\n%s", got)
	}
	if !strings.Contains(got, "x = $1") || !strings.Contains(got, "y = $2") {
		t.Errorf("the real placeholders were not numbered in order:\n%s", got)
	}
}

// A comment inside a string literal is not a comment. Getting this backwards
// would swallow the rest of the query.
func TestRewriteDoesNotTreatCommentMarkersInsideStringsAsComments(t *testing.T) {
	pg := postgresDialect{}

	got := pg.Rewrite(`SELECT '-- not a comment' AS s, ? AS a, ? AS b`)
	if !strings.Contains(got, "$1") || !strings.Contains(got, "$2") {
		t.Errorf("placeholders after a string containing `--` were not rewritten:\n%s", got)
	}
}

// The projection this was found in, rewritten for real.
func TestTheTransferProjectionRewritesCompletely(t *testing.T) {
	p := &Packages{dialect: postgresDialect{}}

	got := p.dialect.Rewrite(p.transferSelect(true) +
		" WHERE pr.name = ? ORDER BY t.created_at DESC LIMIT ? OFFSET ?")
	if strings.Contains(got, "?") {
		t.Errorf("the transfer projection still leaves a bare `?` for Postgres:\n%s", got)
	}
	for _, want := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(got, want) {
			t.Errorf("placeholder %s is missing from the rewritten query", want)
		}
	}
}
