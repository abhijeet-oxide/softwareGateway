package download

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// A TAG IS NOT AN IDENTITY, and a download that believes otherwise downloads
// the wrong thing.
//
// A vendor publishes the same version into every repository of a product: nine
// packages, nine different names, all tagged `25.7_mp2604_2131`. Resolution
// here used to be a map keyed by tag alone, so those nine collapsed onto
// whichever row the listing returned last.
//
// Every symptom of that was the same bug wearing three faces:
//
//   - downloading one component downloaded a different one;
//   - the recent-downloads row named a package nobody had asked for;
//   - and the SECOND download, of a genuinely different package, reported
//     "already requested" - because the idempotency key is built from the
//     resolved package's ID, and both resolutions had produced the same row.
//     The key was right. The resolution was not.
//
// Nothing caught it because nothing tested resolution against a product that
// publishes one version under several names, which is every product this
// system exists for.
func TestOneVersionInSeveralRepositoriesResolvesToTheOneAsked(t *testing.T) {
	h := newFindHarness(t)

	const version = "25.7_mp2604_2131"
	k8s := h.seed("orbs/cfx-5000-k8s", version)
	ncp := h.seed("orbs/cfx-5000-k8s-215952-ncp", version)
	if k8s == ncp {
		t.Fatal("the fixture seeded one package, not two")
	}

	for _, want := range []struct {
		ref string
		id  int64
	}{
		{"orbs/cfx-5000-k8s:" + version, k8s},
		{"orbs/cfx-5000-k8s-215952-ncp:" + version, ncp},
	} {
		rows, err := h.service.find(t.Context(), h.product, []string{want.ref})
		if err != nil {
			t.Fatalf("%s: %v", want.ref, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s resolved to %d packages", want.ref, len(rows))
		}
		if rows[0].ID != want.id {
			t.Errorf("%s resolved to package %d, want %d - a download of one "+
				"component would move a different one",
				want.ref, rows[0].ID, want.id)
		}
	}
}

// And a bare version, which matches both, is REFUSED rather than resolved to
// whichever row came back first.
//
// Refusing is the only honest answer: the caller has not said which package
// they mean, and picking one is how a download of the wrong component looks
// exactly like a download of the right one.
func TestABareVersionMatchingSeveralRepositoriesIsRefused(t *testing.T) {
	h := newFindHarness(t)

	const version = "25.7_mp2604_2131"
	h.seed("orbs/cfx-5000-k8s", version)
	h.seed("orbs/cfx-5000-k8s-215952-ncp", version)

	_, err := h.service.find(t.Context(), h.product, []string{version})
	if err == nil {
		t.Fatal("a version matching two repositories resolved to one of them " +
			"without saying which")
	}

	// And it says WHICH, or the caller cannot act on it.
	for _, repo := range []string{"orbs/cfx-5000-k8s", "orbs/cfx-5000-k8s-215952-ncp"} {
		if !strings.Contains(err.Error(), repo) {
			t.Errorf("the refusal does not name %s, so nobody can tell it what "+
				"they meant: %v", repo, err)
		}
	}
}

// A version published into ONE repository still resolves bare. The point is
// refusing to guess, not making people type more than they have to.
func TestAVersionInOneRepositoryStillResolvesBare(t *testing.T) {
	h := newFindHarness(t)
	id := h.seed("orbs/cfx-5000-k8s", "25.7_mp2604_2131")

	rows, err := h.service.find(t.Context(), h.product, []string{"25.7_mp2604_2131"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Errorf("a version unique to one repository did not resolve bare: %+v", rows)
	}
}

type findHarness struct {
	t         *testing.T
	service   *Service
	packages  *store.Packages
	product   *product.Product
	productID int64
	n         int
}

func newFindHarness(t *testing.T) *findHarness {
	t.Helper()
	ctx := t.Context()

	st, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "download.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(ctx, st, nil); err != nil {
		t.Fatal(err)
	}

	packages := store.NewPackages(st)
	var productID int64
	if err := st.DB().QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config)
		 VALUES ('cfx-5000-product', 'hash', '{}') RETURNING id`).Scan(&productID); err != nil {
		t.Fatal(err)
	}

	p := &product.Product{}
	p.Metadata.Name = "cfx-5000-product"

	return &findHarness{
		t: t, service: NewService(packages, nil, nil),
		packages: packages, product: p, productID: productID,
	}
}

// seed writes one package in one repository, and returns its id.
func (h *findHarness) seed(repository, tag string) int64 {
	h.t.Helper()
	ctx := h.t.Context()
	h.n++

	tx, err := h.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Named the way a source that enumerates a catalogue names its rows:
	// "<source>/<path>", one per repository it found.
	repoID, err := h.packages.EnsureRepository(ctx, tx, h.productID, "source",
		"vendor/"+repository, "registry.example.com", repository, "generic", "config", "")
	if err != nil {
		h.t.Fatal(err)
	}

	id, err := h.packages.InsertPackage(ctx, tx, store.PackageRow{
		ProductID:    h.productID,
		SourceRepoID: repoID,
		Tag:          tag,
		// Distinct content per repository, which is the real situation: these
		// are different releases that happen to carry one version number.
		ManifestDigest: "sha256:" + strings.Repeat(string(rune('a'+h.n)), 64),
		MediaType:      "application/vnd.oci.image.index.v1+json",
		ArtifactCount:  1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}
