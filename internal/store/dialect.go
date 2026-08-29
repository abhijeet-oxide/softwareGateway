package store

import (
	"strconv"
	"strings"
)

// Dialect papers over the two SQL flavours where they genuinely differ.
//
// The logical schema is identical across Postgres and SQLite (docs/design/03
// section 2), and most statements are portable verbatim. Four things are not,
// and they are handled here rather than by duplicating every query:
//
//   - Placeholders. Queries are written with `?` and rewritten to `$N` for
//     Postgres, so a query lives in one place.
//   - "now". Postgres has now(); SQLite needs strftime.
//   - Boolean literals. Postgres has TRUE/FALSE; SQLite stores 0/1.
//   - Elapsed time between two stored timestamps. Postgres subtracts them into
//     an interval; SQLite has to convert both to Julian days and scale.
//
// Anything beyond this - the dequeue statement, in particular - gets a
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
	// which strftime cannot parse and answers NULL to. Use TimeAhead instead -
	// it exists because that bug shipped once, wrote NULL lease expiries, and
	// left the reaper with nothing to find.
	TimeAgo(secondsExpr string) string
	// TimeAhead renders "the moment N seconds from now".
	TimeAhead(secondsExpr string) string
	// SecondsBetween renders the elapsed seconds from one stored timestamp to
	// another, as a float. NULL when either side is NULL, which is the honest
	// answer for a transfer that never started or has not finished - callers
	// must treat it as "unknown" rather than coercing it to zero, or an
	// unfinished transfer would report infinite speed.
	SecondsBetween(from, to string) string
	// TimestampText renders a possibly-NULL timestamp expression as an RFC3339
	// string in UTC, or the empty string when there is no value.
	//
	// # Why this is a dialect method and not COALESCE(x, '')
	//
	// It was `COALESCE(x, '')`, and that is a SQLite idiom rather than SQL.
	// SQLite stores a timestamp AS TEXT, so coalescing one with a string is
	// two strings; Postgres declares the column `timestamptz` and rejects the
	// empty string outright -
	//
	//	ERROR: invalid input syntax for type timestamp with time zone: ""
	//
	// - because there is no cast that turns "" into a moment. The transfer
	// projection had four, so listing or reading a transfer could not work on
	// Postgres at all while SQLite, whose dialect is largely the identity
	// function, was perfectly happy.
	//
	// It also makes the two databases agree on the FORM of the answer, which
	// they did not before. These columns are scanned into strings and served
	// to a browser; left to the driver, Postgres hands back a time.Time that
	// database/sql renders with whatever offset the session is in, and SQLite
	// hands back the milliseconds-and-Z text it wrote. One shape, chosen here,
	// so a client parses one thing.
	TimestampText(expr string) string
	// IDText renders a UUID column as text, so it can be compared with LIKE
	// and with arbitrary user input.
	//
	// SQLite stores these ids as TEXT and Postgres as UUID, and a UUID is not
	// a string it will compare loosely: `id LIKE 'abc%'` is
	// `operator does not exist: uuid ~~ unknown`, and `id = 'abc'` is
	// `invalid input syntax for type uuid`. A cast is needed for the prefix
	// search a person doing `transferctl transfer 3f2a` relies on.
	//
	// FOR PREFIX MATCHING ONLY. It defeats the primary key index, so an exact
	// lookup must compare the column itself.
	IDText(col string) string
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
// Quoted string literals are skipped so a `?` inside one - which is legal SQL
// and appears in LIKE patterns - is not mistaken for a placeholder.
//
// # COMMENTS ARE SKIPPED TOO, and leaving them out was a real bug
//
// The queries in this package are heavily commented, in prose, and English
// prose contains apostrophes: "the package's own repository", "the transfer's
// origin". Without comment handling each of those reads as the start of a
// string literal, and an ODD number of them in one query leaves the rewriter
// believing everything after the last one is inside a string. Every `?` from
// there on is left alone.
//
// The transfer projection had three. So `LIMIT ? OFFSET ?` reached Postgres
// verbatim and it answered
//
//	ERROR: syntax error at or near "OFFSET" (SQLSTATE 42601)
//
// which names neither the cause nor anything in the query the author wrote.
// Listing and reading transfers - the Downloads page, the shell, every
// `transferctl transfers` command - could not work on Postgres at all, while
// SQLite, whose Rewrite is the identity function, was perfectly happy.
//
// Handling the comments here rather than removing the apostrophes is the fix
// that stays fixed: the next person to write "the worker's lease" in a query
// comment must not silently break it.
func (postgresDialect) Rewrite(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)

	n := 0
	inSingle, inDouble := false, false

	for i := 0; i < len(query); i++ {
		c := query[i]

		// A line comment runs to the newline, and nothing in it is SQL. Checked
		// before the quote handling, because the whole point is that an
		// apostrophe inside one is punctuation rather than a delimiter.
		if !inSingle && !inDouble && c == '-' && i+1 < len(query) && query[i+1] == '-' {
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				b.WriteString(query[i:])
				return b.String()
			}
			b.WriteString(query[i : i+end+1])
			i += end
			continue
		}

		// A block comment, for the same reason. Postgres nests these; the
		// depth count is cheap and means a /* */ inside one cannot end it early.
		if !inSingle && !inDouble && c == '/' && i+1 < len(query) && query[i+1] == '*' {
			depth, j := 1, i+2
			for j < len(query) && depth > 0 {
				switch {
				case j+1 < len(query) && query[j] == '/' && query[j+1] == '*':
					depth++
					j += 2
				case j+1 < len(query) && query[j] == '*' && query[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			b.WriteString(query[i:j])
			i = j - 1
			continue
		}

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
func (postgresDialect) SecondsBetween(from, to string) string {
	return "EXTRACT(EPOCH FROM (" + to + " - " + from + "))"
}

// TimestampText formats in SQL rather than converting in Go, so the empty
// case stays inside the COALESCE - a NULL timestamp must become ” without
// the empty string ever being offered to a timestamp comparison.
//
// The format is SQLite's own output format, down to the millisecond and the
// trailing Z, which is what makes the two dialects interchangeable to a
// caller. AT TIME ZONE 'UTC' first, because the Z is only true if the value
// has been moved there.
func (postgresDialect) TimestampText(expr string) string {
	return `COALESCE(to_char((` + expr + `) AT TIME ZONE 'UTC', ` +
		`'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), '')`
}

func (postgresDialect) IDText(col string) string { return "(" + col + ")::text" }

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

// SecondsBetween goes through julianday because SQLite has no interval type
// and no epoch conversion for a text timestamp. The 86400 scales days to
// seconds; the .0 keeps it floating point, so a sub-second transfer is not
// truncated to zero and made to look infinitely fast.
func (sqliteDialect) SecondsBetween(from, to string) string {
	return "((julianday(" + to + ") - julianday(" + from + ")) * 86400.0)"
}

// TimestampText is the plain COALESCE: the column already holds the text this
// returns, written by Now() in exactly this format.
func (sqliteDialect) TimestampText(expr string) string {
	return "COALESCE(" + expr + ", '')"
}

// IDText is the column itself: SQLite already stores it as text.
func (sqliteDialect) IDText(col string) string { return col }

func (sqliteDialect) Bool(b bool) string { return map[bool]string{true: "1", false: "0"}[b] }
func (sqliteDialect) Name() Driver       { return DriverSQLite }
