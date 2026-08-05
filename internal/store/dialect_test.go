package store

import "testing"

func TestDialectRewritesPlaceholders(t *testing.T) {
	pg := DialectFor(DriverPostgres)
	if got := pg.Rewrite("SELECT ? , ? FROM t WHERE x = ?"); got != "SELECT $1 , $2 FROM t WHERE x = $3" {
		t.Fatalf("got %q", got)
	}
	// A `?` inside a string literal is data, not a placeholder — otherwise a
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
