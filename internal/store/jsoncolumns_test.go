package store

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE INCIDENT THIS PREVENTS.
//
// `COALESCE(pa.annotations, ”)` is a SQLite idiom rather than SQL. SQLite
// stores that column as TEXT, so coalescing it with a string is two strings;
// Postgres declares it JSONB and must therefore read the empty string AS JSON,
// which it refuses:
//
//	ERROR: invalid input syntax for type json (SQLSTATE 22P02)
//
// Four queries had one. Listing a release's files, reading one of its files,
// listing its chart candidates and summarising what a transfer skipped could
// not work on Postgres at all - `task run`, which is SQLite and whose dialect
// is largely the identity function, was perfectly happy. It reached a browser
// as a 503 on the release page, from which the interface concluded the whole
// service was down.
//
// It is the same mistake `TimestampText` was added for, one column type along,
// and `JSONText` already existed to fix it. What was missing was anything that
// noticed a query had not used it.
//
// The remedy is always safe to apply: JSONText renders `(x)::text` on Postgres
// and the plain COALESCE on SQLite, so a column this test names that is
// actually TEXT loses nothing by going through it.
func TestNoJSONColumnIsCoalescedWithAString(t *testing.T) {
	json := jsonColumns(t)
	// Guard against the scan passing because the regexp found nothing: an
	// empty column set would make every query below trivially clean.
	if len(json) < 5 {
		t.Fatalf("found only %d JSON columns in the Postgres migrations: %v", len(json), json)
	}

	// `COALESCE(<qualifier>.<column>, '')`, which is the shape that breaks.
	// Written to match the query text as an author would type it, including
	// across the line breaks these queries are formatted with.
	suspect := regexp.MustCompile(`COALESCE\(\s*(?:[A-Za-z_][A-Za-z0-9_]*\.)?([A-Za-z_][A-Za-z0-9_]*)\s*,\s*''\s*\)`)

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			for _, m := range suspect.FindAllStringSubmatch(line, -1) {
				if json[m[1]] {
					found = append(found, src+": "+strings.TrimSpace(line))
				}
			}
		}
	}

	if len(found) > 0 {
		sort.Strings(found)
		t.Fatalf("a JSON column coalesced with a string literal - Postgres reads '' as JSON and "+
			"answers SQLSTATE 22P02. Use dialect.JSONText(\"<column>\") instead:\n  %s",
			strings.Join(found, "\n  "))
	}
}

// jsonColumns is every column the Postgres schema declares JSON or JSONB.
//
// Read from the migrations rather than listed here, so a column added in a
// later migration is covered the day it is added rather than the day somebody
// remembers this test exists. Names only: a query's table is not derivable
// from its alias, and the remedy is harmless on a column that turns out to be
// text.
func jsonColumns(t *testing.T) map[string]bool {
	t.Helper()

	sql := readMigration(t, "postgres")
	// A column definition or an ADD COLUMN, both of which are `<name> JSON[B]`
	// with the type as the next word.
	decl := regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s+JSONB?\b`)

	out := map[string]bool{}
	for _, m := range decl.FindAllStringSubmatch(sql, -1) {
		name := strings.ToLower(m[1])
		// `ADD COLUMN annotations JSONB` matches the column, and `COLUMN` is
		// not one.
		if name == "column" || name == "as" {
			continue
		}
		out[name] = true
	}
	return out
}
