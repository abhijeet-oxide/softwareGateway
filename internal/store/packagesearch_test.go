package store

import (
	"strings"
	"testing"
)

// THE SEARCH IS THE SERVER'S JOB, and these are the cases that decide it.
//
// It used to be the browser's: the Packages page fetched a hundred rows per
// product and ran a substring test over them. That searches what happened to be
// loaded rather than what exists, so a release on the second page came back as
// "nothing matches" - a confident wrong answer, which is the worst kind. These
// tests pin the four fields it looks at, the case-insensitivity that has to
// hold on both dialects, and the two things a substring search must NOT do.

func TestListPackagesSearchesEitherSpellingOfBothFields(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 1, 10)
	h.seed("orb_23.8.1077", strings.Repeat("2", 64), 1, 10)

	cases := []struct {
		name   string
		search string
		want   int
	}{
		// The stored tag, and the shortened one a listing actually renders.
		// Both, because the text a reader pastes is the text on their screen.
		{"stored tag", "orb_23.8.1076", 1},
		{"display tag", "23.8.1076", 1},
		// A substring of a version, which is what somebody types when they are
		// looking for a release rather than naming one.
		{"partial tag", "23.8.107", 2},
		// The repository path, in both spellings.
		{"stored repository", "orbs/cfx-5000", 2},
		{"display repository", "cfx-5000-k8s", 2},
		// UPPER CASE. SQLite folds ASCII case for LIKE and PostgreSQL does not,
		// so a search that relied on the default would work in development and
		// fail in production.
		{"mixed case", "CFX-5000", 2},
		{"upper tag", "ORB_23.8.1077", 1},
		// A term matching nothing must match NOTHING. Accepting two spellings
		// must never become accepting anything.
		{"no match", "23.8.9999", 0},
		// The wildcards LIKE understands are ESCAPED, so a literal percent is a
		// literal percent rather than "every release".
		{"literal wildcard", "%", 0},
		{"literal underscore wildcard", "cfx_5000", 0},
		// Whitespace is not a search. A cleared box must list everything rather
		// than matching the space that was left in it.
		{"blank", "   ", 2},
		// THE THREE SPELLINGS OF A RELEASE. People write one down the way they
		// say it, and no single column contains the separator - the path is one
		// field and the tag is another - so all three have to become terms.
		{"colon pair", "cfx-5000-k8s:23.8.1076", 1},
		{"at pair", "cfx-5000-k8s@23.8.1076", 1},
		{"spaced pair", "cfx-5000-k8s 23.8.1076", 1},
		// EVERY term has to match. A pair whose halves name different releases
		// is not a release.
		{"pair naming nothing", "cfx-5000-k8s:99.9.9999", 0},
		// A DIGEST stays whole: `ccbd…` does not open like a version, so the
		// colon is not a separator and the query is matched as typed.
		{"digest is not a pair", "sha256:1111", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := h.packages.ListPackages(t.Context(), ListPackagesFilter{
				ProductName: "vendor-a", Search: tc.search, Limit: 50,
			})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != tc.want {
				t.Fatalf("search %q matched %d package(s), want %d", tc.search, len(rows), tc.want)
			}
		})
	}
}

// A listing has to say which product each row belongs to, or the estate-wide
// listing has an id where the product column goes.
func TestListPackagesCarriesTheProductName(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 1, 10)

	rows, err := h.packages.ListPackages(t.Context(), ListPackagesFilter{
		ProductName: "vendor-a", Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d package(s), want 1", len(rows))
	}
	if rows[0].ProductName != "vendor-a" {
		t.Errorf("product name = %q, want vendor-a", rows[0].ProductName)
	}
}

// The estate-wide listing names its products explicitly, and a filter that
// names none lists NOTHING.
//
// That is the whole safety property of the route above it: the handler narrows
// a caller to the products they may read, and a narrowing that came back empty
// must not fall through to "every product in the database".
func TestListPackagesAcrossNamedProducts(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 1, 10)

	rows, err := h.packages.ListPackages(t.Context(), ListPackagesFilter{
		Products: []string{"vendor-a", "vendor-absent"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list across products: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d package(s), want 1", len(rows))
	}

	if _, err := h.packages.ListPackages(t.Context(), ListPackagesFilter{Limit: 10}); err == nil {
		t.Fatal("a filter naming no product listed something; it must refuse instead")
	}
}

// The term split is the whole of what makes the three spellings work, so it is
// pinned on its own rather than only through the query above.
func TestSearchTerms(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"cmm", []string{"cmm"}},
		// Lowercased once, here, so every caller compares the same way.
		{"CMM", []string{"cmm"}},
		{"nokia/cmm:24.Q3.4", []string{"nokia/cmm", "24.q3.4"}},
		{"nokia/cmm@24.Q3.4", []string{"nokia/cmm", "24.q3.4"}},
		{"nokia/cmm v1.2", []string{"nokia/cmm", "v1.2"}},
		// The LAST separator, because a path may carry one of its own.
		{"a:b:1.0", []string{"a:b", "1.0"}},
		// Not a version, so not a pair: matched as typed.
		{"sha256:ccbd", []string{"sha256:ccbd"}},
		{"orbs/cfx:latest", []string{"orbs/cfx:latest"}},
		// Already two terms, so a colon inside one of them is part of it.
		{"a:b 1.0", []string{"a:b", "1.0"}},
		// Nothing after the separator is nothing to split off.
		{"cmm:", []string{"cmm:"}},
	}

	for _, tc := range cases {
		got := SearchTerms(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("SearchTerms(%q) = %q, want %q", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SearchTerms(%q) = %q, want %q", tc.raw, got, tc.want)
				break
			}
		}
	}
}

// The count and the listing read the SAME filter, which is the property the
// shared clause builder exists to guarantee.
//
// A pager drawn from a count that matches a different set of rows than the
// listing does is worse than no pager: it offers four pages over three, and the
// fourth comes back empty with nothing to say why.
func TestCountPackagesAgreesWithTheListing(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 1, 10)
	h.seed("orb_23.8.1077", strings.Repeat("2", 64), 1, 10)
	h.seed("orb_24.1.0001", strings.Repeat("3", 64), 1, 10)

	filters := []ListPackagesFilter{
		{ProductName: "vendor-a"},
		{ProductName: "vendor-a", Search: "23.8"},
		{ProductName: "vendor-a", Search: "cfx-5000-k8s:24.1.0001"},
		{ProductName: "vendor-a", Tag: "23.8.1076"},
		{ProductName: "vendor-a", Search: "nothing-like-this"},
		{Products: []string{"vendor-a"}},
		{Products: []string{"vendor-a", "vendor-absent"}, Search: "orb"},
	}

	for _, f := range filters {
		// The listing is asked WITHOUT a page, so its length is the whole set
		// and the count has something to be compared against.
		f.Limit = 1000
		rows, err := h.packages.ListPackages(t.Context(), f)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		total, err := h.packages.CountPackages(t.Context(), f)
		if err != nil {
			t.Fatalf("count %+v: %v", f, err)
		}
		if total != len(rows) {
			t.Errorf("count %d, listed %d, for %+v", total, len(rows), f)
		}
	}

	// THE COUNT IGNORES THE PAGE, which is the whole reason a pager can use it.
	paged := ListPackagesFilter{ProductName: "vendor-a", Limit: 1, Offset: 1}
	rows, err := h.packages.ListPackages(t.Context(), paged)
	if err != nil {
		t.Fatalf("list a page: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("page of 1 returned %d row(s)", len(rows))
	}
	total, err := h.packages.CountPackages(t.Context(), paged)
	if err != nil {
		t.Fatalf("count a page: %v", err)
	}
	if total != 3 {
		t.Errorf("count = %d over a page of 1, want 3 - the page must not narrow it", total)
	}

	// And a filter naming no scope refuses, exactly as the listing does: a
	// narrowing that came back empty must not count the whole database.
	if _, err := h.packages.CountPackages(t.Context(), ListPackagesFilter{}); err == nil {
		t.Error("a filter naming no product counted something; it must refuse instead")
	}
}
