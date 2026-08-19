package store

import (
	"context"
	"errors"
	"testing"
)

func TestParsePackageRef(t *testing.T) {
	cases := []struct {
		in   string
		want PackageRef
	}{
		{"orb_23.8.1076", PackageRef{Tag: "orb_23.8.1076"}},
		{"  v2.14.0  ", PackageRef{Tag: "v2.14.0"}},

		// A digest is recognised by its shape BEFORE the repository split, or
		// `sha256:abc…` would parse as a repository called "sha256".
		{
			"sha256:ccbd37f1e3f8a5c1e6d7b8a9f0e1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d",
			PackageRef{Digest: "sha256:ccbd37f1e3f8a5c1e6d7b8a9f0e1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d"},
		},

		// The scoped form: a repository path may contain slashes, a tag may not
		// contain a colon, so the split is at the LAST colon.
		{
			"orbs/cfx-5000-k8s:orb_23.8.1076",
			PackageRef{Repository: "orbs/cfx-5000-k8s", Tag: "orb_23.8.1076"},
		},
		{"/orbs/core/:v1", PackageRef{Repository: "orbs/core", Tag: "v1"}},

		// A trailing colon is not a repository with an empty tag; it is a
		// malformed reference, and treating it as a tag lets the lookup fail
		// with "not found" rather than a confusing empty-repository match.
		{"orbs/core:", PackageRef{Tag: "orbs/core:"}},
	}

	for _, c := range cases {
		if got := ParsePackageRef(c.in); got != c.want {
			t.Errorf("ParsePackageRef(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestPackageRefRoundTrips(t *testing.T) {
	for _, in := range []string{
		"v2.14.0",
		"orbs/cfx-5000-k8s:orb_23.8.1076",
		"sha256:ccbd37f1e3f8a5c1e6d7b8a9f0e1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d",
	} {
		if got := ParsePackageRef(in).String(); got != in {
			t.Errorf("%q round-tripped to %q", in, got)
		}
	}
}

// The behaviour this is all for: a vendor's version tag appears in dozens of a
// product's repositories, and picking one silently is the worst available
// outcome - the caller gets a real package, believes it asked for that package,
// and is wrong.
func TestBareTagInSeveralRepositoriesIsRefused(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)

	const tag = "orb_23.8.1076"
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-k8s", tag)
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-db", tag)

	_, err := p.GetPackage(ctx, "vendor-a", tag)

	var amb *AmbiguousReferenceError
	if !errors.As(err, &amb) {
		t.Fatalf("a tag in two repositories must be refused, got err=%v", err)
	}
	if len(amb.Repositories) != 2 {
		t.Fatalf("the error must name every candidate, got %v", amb.Repositories)
	}
	// Sorted, so the message is the same on every run and diffable in a ticket.
	if amb.Repositories[0] != "orbs/cfx-5000-db" || amb.Repositories[1] != "orbs/cfx-5000-k8s" {
		t.Errorf("candidates should be sorted, got %v", amb.Repositories)
	}
}

func TestScopedReferenceResolvesTheAmbiguity(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)

	const tag = "orb_23.8.1076"
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-k8s", tag)
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-db", tag)

	row, err := p.GetPackage(ctx, "vendor-a", "orbs/cfx-5000-db:"+tag)
	if err != nil {
		t.Fatalf("a scoped reference must resolve: %v", err)
	}
	if row.SourceRepository != "orbs/cfx-5000-db" {
		t.Errorf("resolved to %q, want orbs/cfx-5000-db", row.SourceRepository)
	}
}

// A bare tag in ONE repository is not ambiguous and must keep working: making
// every reference scoped would be a tax on the common single-repository case.
func TestBareTagInOneRepositoryStillResolves(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	seedPackage(t, ctx, p, productID, "orbs/only", "v1.0.0")

	if _, err := p.GetPackage(ctx, "vendor-a", "v1.0.0"); err != nil {
		t.Fatalf("an unambiguous tag must resolve: %v", err)
	}
}

// A digest identifies content, and the same content really can sit in two
// repositories - but it is the SAME package either way, so this is the one
// place where several matches is not a question for the caller.
func TestDigestResolvesWithoutARepository(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	row := seedPackage(t, ctx, p, productID, "orbs/only", "v1.0.0")

	got, err := p.GetPackage(ctx, "vendor-a", row.ManifestDigest)
	if err != nil {
		t.Fatalf("a digest must resolve: %v", err)
	}
	if got.ID != row.ID {
		t.Errorf("resolved package %d, want %d", got.ID, row.ID)
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// seedPackages returns a Packages on a migrated store with one product row.
//
// The product row is inserted directly rather than through the catalog
// reconciler: catalog imports store, so using it here would be an import cycle.
func seedPackages(t *testing.T, ctx context.Context) (*Packages, int64) {
	t.Helper()
	st := openTestStore(t)

	res, err := st.DB().ExecContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a', 'hash', '{}')`)
	if err != nil {
		t.Fatalf("insert product: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("product id: %v", err)
	}
	return NewPackages(st), id
}

// seedPackage inserts one package in one repository, creating the repository
// row if it does not exist.
func seedPackage(
	t *testing.T, ctx context.Context, p *Packages, productID int64, repoPath, tag string,
) PackageRow {
	t.Helper()

	tx, err := p.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	repoID, err := p.EnsureRepository(ctx, tx, productID, "source", repoPath,
		"registry.example.com", repoPath, "generic", "discovery", "")
	if err != nil {
		t.Fatalf("ensure repository %s: %v", repoPath, err)
	}

	// Distinct content per (repository, tag), so a digest lookup is unambiguous
	// and a supersession never fires by accident.
	digest := "sha256:" + hex64(repoPath+"|"+tag)

	row := PackageRow{
		ProductID:      productID,
		SourceRepoID:   repoID,
		Tag:            tag,
		ManifestDigest: digest,
		MediaType:      "application/vnd.oci.image.index.v1+json",
		ArtifactCount:  1,
	}
	id, err := p.InsertPackage(ctx, tx, row)
	if err != nil {
		t.Fatalf("insert package %s:%s: %v", repoPath, tag, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	row.ID = id
	row.SourceRepository = repoPath
	return row
}

// hex64 makes a stable 64-character hex string from a seed, so a fixture digest
// is well-formed without pulling in a real hash of real content.
func hex64(seed string) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	acc := 0
	for i := range out {
		if i < len(seed) {
			acc += int(seed[i])
		}
		acc = acc*31 + i
		out[i] = digits[(acc%16+16)%16]
	}
	return string(out)
}

// A listing shows the display tag, so typing it back must work. Anything else
// makes the abbreviation a trap.
func TestDisplayTagResolves(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	seedDisplayPackage(t, ctx, p, productID, "orbs/cfx-5000-k8s", "orb_23.8.1076", "23.8.1076")

	for _, ref := range []string{"orb_23.8.1076", "23.8.1076"} {
		row, err := p.GetPackage(ctx, "vendor-a", ref)
		if err != nil {
			t.Fatalf("GetPackage(%q): %v", ref, err)
		}
		// Whichever spelling was used, the REAL tag comes back - the identity
		// is never the shortened form.
		if row.Tag != "orb_23.8.1076" {
			t.Errorf("resolved %q to tag %q, want orb_23.8.1076", ref, row.Tag)
		}
	}
}

// The display form must not create a NEW ambiguity: two repositories whose
// display tags collide are exactly as ambiguous as their real tags colliding.
func TestAmbiguousDisplayTagIsRefused(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	seedDisplayPackage(t, ctx, p, productID, "orbs/cfx-5000-k8s", "orb_23.8.1076", "23.8.1076")
	seedDisplayPackage(t, ctx, p, productID, "orbs/cfx-5000-db", "orb_23.8.1076", "23.8.1076")

	_, err := p.GetPackage(ctx, "vendor-a", "23.8.1076")
	var amb *AmbiguousReferenceError
	if !errors.As(err, &amb) {
		t.Fatalf("a display tag in two repositories must be refused, got %v", err)
	}
}

// seedDisplayPackage inserts a package with an explicit display tag.
func seedDisplayPackage(
	t *testing.T, ctx context.Context, p *Packages, productID int64, repoPath, tag, display string,
) {
	t.Helper()

	tx, err := p.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	repoID, err := p.EnsureRepository(ctx, tx, productID, "source", repoPath,
		"registry.example.com", repoPath, "generic", "discovery", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.InsertPackage(ctx, tx, PackageRow{
		ProductID:      productID,
		SourceRepoID:   repoID,
		Tag:            tag,
		DisplayTag:     display,
		ManifestDigest: "sha256:" + hex64(repoPath+"|"+tag),
		MediaType:      "application/vnd.oci.image.index.v1+json",
		ArtifactCount:  1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// The listing drops the prefix every repository shares, so `cfx-5000-db` is
// what a user sees and copies. It has to resolve.
func TestShortenedRepositoryResolves(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-k8s", "orb_23.8.1076")
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-db", "orb_23.8.1076")

	for _, ref := range []string{
		"orbs/cfx-5000-db:orb_23.8.1076", // full, as stored
		"cfx-5000-db:orb_23.8.1076",      // shortened, as displayed
	} {
		row, err := p.GetPackage(ctx, "vendor-a", ref)
		if err != nil {
			t.Fatalf("GetPackage(%q): %v", ref, err)
		}
		if row.SourceRepository != "orbs/cfx-5000-db" {
			t.Errorf("%q resolved to %q", ref, row.SourceRepository)
		}
	}
}

// Whole trailing segments only. A substring match would resolve `db` against
// `orbs/cfx-5000-db`, which is a different repository.
func TestRepositoryMatchesWholeSegmentsOnly(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	seedPackage(t, ctx, p, productID, "orbs/cfx-5000-db", "v1")

	if _, err := p.GetPackage(ctx, "vendor-a", "5000-db:v1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a partial segment must not match, got %v", err)
	}
	if _, err := p.GetPackage(ctx, "vendor-a", "cfx-5000-db:v1"); err != nil {
		t.Errorf("a whole trailing segment must match: %v", err)
	}
}

// Two repositories whose short forms collide are genuinely ambiguous, and must
// be refused with both rather than silently resolved to one.
func TestAmbiguousShortRepositoryIsRefused(t *testing.T) {
	ctx := context.Background()
	p, productID := seedPackages(t, ctx)
	seedPackage(t, ctx, p, productID, "orbs/core", "v1")
	seedPackage(t, ctx, p, productID, "charts/core", "v1")

	_, err := p.GetPackage(ctx, "vendor-a", "core:v1")
	var amb *AmbiguousRepositoryError
	if !errors.As(err, &amb) {
		t.Fatalf("expected an ambiguous-repository error, got %v", err)
	}
	if len(amb.Paths) != 2 {
		t.Errorf("the error must name both candidates, got %v", amb.Paths)
	}
}
