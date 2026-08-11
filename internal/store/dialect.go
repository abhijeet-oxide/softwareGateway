package store

import (
	"strconv"
	"strings"
)

// Dialect papers over the two SQL flavours where they genuinely differ.
//
// The logical schema is identical across Postgres and SQLite (docs/design/03
// section 2), and most statements are portable verbatim. Three things are not,
// and they are handled here rather than by duplicating every query:
//
//   - Placeholders. Queries are written with `?` and rewritten to `$N` for
//     Postgres, so a query lives in one place.
//   - "now". Postgres has now(); SQLite needs strftime.
//   - Boolean literals. Postgres has TRUE/FALSE; SQLite stores 0/1.
//
// Anything beyond this — the dequeue statement, in particular — gets a
// dialect-specific implementation rather than being contorted through here.
// See docs/design/04 section 4.1.
type Dialect interface {
	// Rewrite converts `?` placeholders to the dialect's form.
	Rewrite(query string) string
	// Now is the SQL expression for the current timestamp.
	Now() string
	// Bool renders a boolean literal.
	Bool(b bool) string
	// TimeAgo renders "the moment N seconds before now", where N is a
	// placeholder expression.
	//
	// A dialect method because the two databases have nothing in common here:
	// Postgres subtracts an interval from a timestamp, SQLite does date
	// arithmetic on a string. Both produce a value comparable with a stored
	// timestamp, which is all the caller needs.
	//
	// N MUST BE POSITIVE. Passing a negative to mean "in the future" works in
	// Postgres and silently produces NULL in SQLite, where the expression is
	// built by string concatenation: '-' || -5 || ' seconds' is '--5 seconds',
	// which strftime cannot parse and answers NULL to. Use TimeAhead instead —
	// it exists because that bug shipped once, wrote NULL lease expiries, and
	// left the reaper with nothing to find.
	TimeAgo(secondsExpr string) string
	// TimeAhead renders "the moment N seconds from now".
	TimeAhead(secondsExpr string) string
	// Name identifies the dialect.
	Name() Driver
}

// DialectFor returns the dialect for a driver.
func DialectFor(d Driver) Dialect {
	if d == DriverPostgres {
		return postgresDialect{}
	}
	return sqliteDialect{}
}

type postgresDialect struct{}

// Rewrite replaces each `?` with a positional parameter.
//
// Quoted string literals are skipped so a `?` inside one — which is legal SQL
// and appears in LIKE patterns — is not mistaken for a placeholder.
func (postgresDialect) Rewrite(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)

	n := 0
	inSingle, inDouble := false, false

	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' && !inDouble:
			// '' inside a string is an escaped quote, not a terminator.
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte(c)
				b.WriteByte(query[i+1])
				i++
				continue
			}
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '?' && !inSingle && !inDouble:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (postgresDialect) Now() string { return "now()" }

func (postgresDialect) TimeAgo(secondsExpr string) string {
	return "(now() - make_interval(secs => " + secondsExpr + "))"
}

func (postgresDialect) TimeAhead(secondsExpr string) string {
	return "(now() + make_interval(secs => " + secondsExpr + "))"
}
func (postgresDialect) Bool(b bool) string { return map[bool]string{true: "TRUE", false: "FALSE"}[b] }
func (postgresDialect) Name() Driver       { return DriverPostgres }

type sqliteDialect struct{}

// Rewrite is the identity: SQLite already uses `?`.
func (sqliteDialect) Rewrite(query string) string { return query }

// Now matches the DEFAULT expression in the SQLite migration, so a value
// written by the application sorts identically to one written by the schema.
func (sqliteDialect) Now() string { return "strftime('%Y-%m-%dT%H:%M:%fZ','now')" }

func (sqliteDialect) TimeAgo(secondsExpr string) string {
	return "strftime('%Y-%m-%dT%H:%M:%fZ','now', '-' || " + secondsExpr + " || ' seconds')"
}

func (sqliteDialect) TimeAhead(secondsExpr string) string {
	return "strftime('%Y-%m-%dT%H:%M:%fZ','now', '+' || " + secondsExpr + " || ' seconds')"
}
func (sqliteDialect) Bool(b bool) string { return map[bool]string{true: "1", false: "0"}[b] }
func (sqliteDialect) Name() Driver       { return DriverSQLite }
