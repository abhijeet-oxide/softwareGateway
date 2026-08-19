package store

import (
	"strings"
	"testing"
)

// A listing shows a vendor's shortened spelling. Everything that takes a
// repository or a tag as INPUT must therefore accept both, or the abbreviation
// is a trap: someone copies what is on their screen, gets nothing back, and
// reasonably concludes the package is gone.
//
// The stored values are always the real ones - `orbs/cfx-5000-k8s` and
// `orb_23.8.1076`. Shortening is a rendering decision and never a storage one,
// so nothing downstream of these rows has to know a vendor exists.

func TestListFiltersAcceptEitherSpelling(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 1, 10)
	h.seed("orb_23.8.1077", strings.Repeat("2", 64), 1, 10)

	cases := []struct {
		name   string
		filter ListPackagesFilter
		want   int
	}{
		{"full tag", ListPackagesFilter{ProductName: "vendor-a", Tag: "orb_23.8.1076"}, 1},
		{"short tag", ListPackagesFilter{ProductName: "vendor-a", Tag: "23.8.1076"}, 1},
		{"full repository", ListPackagesFilter{
			ProductName: "vendor-a", Repository: "orbs/cfx-5000-k8s"}, 2},
		{"short repository", ListPackagesFilter{
			ProductName: "vendor-a", Repository: "cfx-5000-k8s"}, 2},
		{"both, shortened", ListPackagesFilter{
			ProductName: "vendor-a", Repository: "cfx-5000-k8s", Tag: "23.8.1077"}, 1},
		// A tag that matches neither spelling of anything must still return
		// nothing: accepting two spellings must not become accepting anything.
		{"unknown tag", ListPackagesFilter{ProductName: "vendor-a", Tag: "23.8.9999"}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := h.packages.ListPackages(t.Context(), tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != tc.want {
				t.Fatalf("matched %d package(s), want %d", len(rows), tc.want)
			}
			for _, r := range rows {
				// Whatever was asked for, what comes back is the real thing.
				if !strings.HasPrefix(r.Tag, "orb_") {
					t.Errorf("stored tag = %q; the original must be returned, not the short form", r.Tag)
				}
				if r.SourceRepository != "orbs/cfx-5000-k8s" {
					t.Errorf("stored repository = %q, want orbs/cfx-5000-k8s", r.SourceRepository)
				}
				if r.DisplayRepository != "cfx-5000-k8s" {
					t.Errorf("display repository = %q, want cfx-5000-k8s", r.DisplayRepository)
				}
			}
		})
	}
}

// The same for the single-package read, via the scoped `repository:tag` form a
// person actually types.
func TestGetPackageAcceptsTheShortenedScopedForm(t *testing.T) {
	h := newCacheHarness(t)
	h.seed("orb_23.8.1076", strings.Repeat("1", 64), 1, 10)

	for _, ref := range []string{
		"orbs/cfx-5000-k8s:orb_23.8.1076",
		"cfx-5000-k8s:23.8.1076",
		"orbs/cfx-5000-k8s:23.8.1076",
		"cfx-5000-k8s:orb_23.8.1076",
	} {
		row, err := h.packages.GetPackage(t.Context(), "vendor-a", ref)
		if err != nil {
			t.Fatalf("GetPackage(%q): %v", ref, err)
		}
		if row.Tag != "orb_23.8.1076" {
			t.Errorf("GetPackage(%q) returned tag %q", ref, row.Tag)
		}
	}
}

// A source with no vendor gets no shortening at all - which is the point of
// gating it. Before this, a repository path was trimmed on the strength of what
// a page of results looked like, on every registry.
func TestARepositoryWithNoVendorIsNotShortened(t *testing.T) {
	h := newCacheHarness(t)
	ctx := t.Context()

	tx, err := h.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	plainRepo, err := h.packages.EnsureRepository(ctx, tx, h.productID, "source", "plain",
		"other.example.com", "suite/core", "generic", "config", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.packages.InsertPackage(ctx, tx, PackageRow{
		ProductID: h.productID, SourceRepoID: plainRepo, Tag: "v1.2.3",
		ManifestDigest: strings.Repeat("3", 64),
		MediaType:      "application/vnd.oci.image.manifest.v1+json",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rows, err := h.packages.ListPackages(ctx, ListPackagesFilter{
		ProductName: "vendor-a", Repository: "suite/core"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("matched %d package(s), want 1", len(rows))
	}
	if rows[0].DisplayRepository != "" {
		t.Errorf("display repository = %q, want empty for a source with no vendor",
			rows[0].DisplayRepository)
	}
	if rows[0].DisplayTag != "" {
		t.Errorf("display tag = %q, want empty for a source with no vendor", rows[0].DisplayTag)
	}
}
