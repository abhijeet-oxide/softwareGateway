package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// These exercise the whole path - a registry laid out the way NEAR lays one
// out, through a real scan, into real rows - rather than the Layout in
// isolation. The Layout has its own unit tests; what these catch is the wiring
// between them, which is where a grouping feature actually breaks.
//
// The Layout used here is defined locally rather than imported from
// internal/vendors/near, because depguard forbids this package from importing a
// vendor implementation. That constraint is the point: if this test needed the
// real plugin to pass, the core would not be generic.

// wrapperLayout groups `signed_X` -> X, mirroring the NEAR convention closely
// enough to exercise the scanner's grouping phase.
type wrapperLayout struct{}

func (wrapperLayout) Name() string             { return "test-wrapper" }
func (wrapperLayout) LooksForSignatures() bool { return true }

// Vocabulary exercises the path where a vendor renames the nouns, so a scan
// summary reporting "orbs" rather than "repositories" is covered rather than
// assumed.
func (wrapperLayout) Vocabulary() vendors.Vocabulary {
	return vendors.Vocabulary{
		Unit: "orb", Units: "orbs", Version: "orb version", Versions: "orb versions",
	}
}

// DisplayRepository strips the vendor's leading namespace, so the scanner's
// handling of a shortened repository path is exercised rather than assumed.
func (wrapperLayout) DisplayRepository(path string) string {
	rest, ok := strings.CutPrefix(path, "orbs/")
	if !ok || rest == "" {
		return ""
	}
	return rest
}

// DisplayTag strips the vendor's release prefix, from the tag string alone -
// which is what lets the scanner reconcile packages discovered before the
// source declared its vendor.
func (wrapperLayout) DisplayTag(tag string) string {
	rest, ok := strings.CutPrefix(tag, "orb_")
	if !ok || rest == "" {
		return ""
	}
	return rest
}

// AccessoryTags mirrors NEAR's: a payload tag needs its wrapper and its
// signature, neither of which a release-tag filter would admit.
// This fake classifies nothing: these tests are about GROUPING, and the OCI
// rules are what a Layout deferring here would get.
func (wrapperLayout) ClassifyArtifact(map[string]string) string { return "" }

func (wrapperLayout) AccessoryTags(tag string) []string {
	if !strings.HasPrefix(tag, "orb_") {
		return nil
	}
	return []string{"signed_" + tag, "signature_" + tag}
}

func (wrapperLayout) Group(
	_ context.Context, _ registry.Source, scanned []vendors.ScannedTag,
) ([]vendors.Package, error) {
	accessory := map[string]bool{}
	roots := map[string]vendors.ScannedTag{}
	sigs := map[string]vendors.Related{}

	for _, s := range scanned {
		if len(s.Children) != 2 {
			continue
		}
		var payload, sig string
		for _, c := range s.Children {
			switch c.Annotations["role"] {
			case "signature":
				sig = c.Annotations["tag"]
				sigs[c.Annotations["payload"]] = vendors.Related{
					Role: vendors.RoleSignature, Tag: sig, Descriptor: c,
				}
			case "payload":
				payload = c.Annotations["tag"]
			}
		}
		if payload == "" {
			continue
		}
		accessory[s.Tag] = true
		accessory[sig] = true
		roots[payload] = s
	}

	var out []vendors.Package
	for _, s := range scanned {
		if accessory[s.Tag] {
			continue
		}
		p := vendors.Package{
			Tag: s.Tag, Descriptor: s.Descriptor,
			DisplayTag: wrapperLayout{}.DisplayTag(s.Tag),
		}
		if root, ok := roots[s.Tag]; ok {
			p.Root = root.Descriptor
			p.RootTag = root.Tag
			p.Related = append(p.Related, vendors.Related{
				Role: vendors.RoleWrapper, Tag: root.Tag, Descriptor: root.Descriptor,
			})
		}
		if sig, ok := sigs[s.Tag]; ok {
			p.Related = append(p.Related, sig)
		}
		out = append(out, p)
	}
	return out, nil
}

// seedRelease writes the three tags a signing vendor publishes for one release.
func seedRelease(t *testing.T, reg *fakeregistry.Registry, repo, version string) {
	t.Helper()

	payloadTag := "orb_" + version
	sigTag := "signature_orb_" + version

	payloadDigest := reg.AddImage(repo, payloadTag, fakeregistry.NewLayer(repo+version+"payload"))
	sigDigest := reg.AddImage(repo, sigTag, fakeregistry.NewLayer(repo+version+"pkcs7"))

	// The wrapper: an index naming both, with the annotations the Layout reads.
	wrapper := map[string]any{
		"schemaVersion": 2,
		"mediaType":     registry.MediaTypeOCIIndex,
		"manifests": []map[string]any{
			{
				"mediaType": registry.MediaTypeOCIManifest,
				"digest":    sigDigest,
				"size":      100,
				"annotations": map[string]string{
					"role": "signature", "tag": sigTag, "payload": payloadTag,
				},
			},
			{
				// No mediaType, exactly as the live NEAR registry omits it on
				// this child. The walk must tolerate it.
				"digest": payloadDigest,
				"size":   200,
				"annotations": map[string]string{
					"role": "payload", "tag": payloadTag,
				},
			},
		},
	}
	raw, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	reg.AddManifest(repo, "signed_orb_"+version, raw, registry.MediaTypeOCIIndex)
}

func groupingScanner(t *testing.T, reg *fakeregistry.Registry, repo string) (*Scanner, *store.Packages, int64) {
	t.Helper()

	doc := fmt.Sprintf(`
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: %s
      repository: %s
      anonymous: true
      signatures:
        layout: test-wrapper
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      anonymous: true
      default: true
`, reg.Host(), repo)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := product.NewLoader(dir, product.NewSecretResolver(t.TempDir())).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invalid) > 0 {
		t.Fatalf("config invalid: %v", res.Invalid[0].Err)
	}
	p := res.Valid[0]

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "g.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	rec, err := catalog.NewCatalog(st).Reconcile(t.Context(), res.Valid)
	if err != nil {
		t.Fatal(err)
	}
	ref := rec.Products["vendor-a"]

	newClient, err := SourceClientFactory(p, p.Spec.Sources[0], product.NewSecretResolver(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}

	packages := store.NewPackages(st)
	s, err := NewScanner(ScannerConfig{
		Packages: packages, Product: p, ProductID: ref.ID, SourceName: "vendor",
		NewClient: newClient, RepoIDs: ref.Repositories, Layout: wrapperLayout{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, packages, ref.ID
}

// The headline behaviour, end to end: a registry holding three tags produces
// ONE package row, not three.
func TestScanGroupsVendorTagsIntoOnePackage(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	s, packages, productID := groupingScanner(t, reg, "orbs/cfx-5000-k8s")

	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.TagsListed != 3 {
		t.Errorf("listed %d tags, want the vendor's 3", res.TagsListed)
	}
	if res.New != 1 {
		t.Fatalf("recorded %d packages, want 1 - the other two are accessories", res.New)
	}

	list, err := packages.ListPackages(t.Context(), store.ListPackagesFilter{ProductName: "vendor-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		names := make([]string, len(list))
		for i, p := range list {
			names[i] = p.Tag
		}
		t.Fatalf("listed %v, want one package", names)
	}

	pkg := list[0]
	if pkg.Tag != "orb_23.8.1076" {
		t.Errorf("package tag = %q, want the payload tag a person would type", pkg.Tag)
	}
	// The load-bearing one: transferring the payload alone would leave the
	// signature behind and foreclose destination-side verification for good.
	if pkg.TransferRootDigest == "" {
		t.Error("no transfer root recorded - a transfer would move the payload without its signature")
	}
	if pkg.TransferRootTag != "signed_orb_23.8.1076" {
		t.Errorf("transfer root tag = %q, want signed_orb_23.8.1076", pkg.TransferRootTag)
	}
	if pkg.SignatureStatus != string(vendors.SignatureSigned) {
		t.Errorf("signature status = %q, want signed", pkg.SignatureStatus)
	}

	rels, err := packages.ListRelations(t.Context(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, r := range rels {
		roles[r.Role] = true
	}
	if !roles["signature"] || !roles["wrapper"] {
		t.Errorf("relations = %+v, want both a signature and a wrapper", rels)
	}
	_ = productID
}

// A release the vendor did not sign must read `unsigned`, not `signed` and not
// `unknown` - the layout looked, and found none.
func TestUnsignedReleaseIsReportedAsUnsigned(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("orbs/cfx-5000-k8s", "orb_22.1.0001", fakeregistry.NewLayer("old"))

	s, packages, _ := groupingScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	list, err := packages.ListPackages(t.Context(), store.ListPackagesFilter{ProductName: "vendor-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d packages, want 1", len(list))
	}
	if list[0].SignatureStatus != string(vendors.SignatureUnsigned) {
		t.Errorf("status = %q, want unsigned", list[0].SignatureStatus)
	}
	if list[0].TransferRootDigest != "" {
		t.Error("an unsigned release has no wrapper, so it must plan from its own manifest")
	}
}

// The steady state must stay cheap. Grouping runs only over NEW tags, so a
// re-scan of unchanged content still costs one HEAD per tag and records nothing.
func TestRescanWithGroupingIsIdempotent(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.9.0001")

	s, packages, _ := groupingScanner(t, reg, "orbs/cfx-5000-k8s")

	first, err := s.Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.New != 2 {
		t.Fatalf("first scan recorded %d packages, want 2", first.New)
	}

	second, err := s.Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.New != 0 {
		t.Errorf("re-scan recorded %d packages, want 0", second.New)
	}

	list, err := packages.ListPackages(t.Context(), store.ListPackagesFilter{ProductName: "vendor-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d packages after two scans, want 2", len(list))
	}
}

// End to end, because this is where the shortening feature actually breaks: the
// Layout has unit tests and the store has unit tests, and what fails is the
// wiring between them.
//
// The rule under test is that shortening comes from the SOURCE declaring a
// vendor, and reaches the row through the scanner. It used to be inferred at
// render time from whichever prefix a page of results happened to share, which
// meant it applied to registries that have no such convention and changed a
// row's rendering depending on what else was on the page.
func TestScanRecordsTheVendorShortenedNames(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	s, packages, _ := groupingScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	list, err := packages.ListPackages(t.Context(), store.ListPackagesFilter{ProductName: "vendor-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d packages, want 1", len(list))
	}
	pkg := list[0]

	// The originals are what is stored. Everything downstream - a transfer, an
	// audit record, `-o json` - reads these.
	if pkg.Tag != "orb_23.8.1076" {
		t.Errorf("stored tag = %q, want orb_23.8.1076", pkg.Tag)
	}
	if pkg.SourceRepository != "orbs/cfx-5000-k8s" {
		t.Errorf("stored repository = %q, want orbs/cfx-5000-k8s", pkg.SourceRepository)
	}

	// The shortened forms sit alongside them, for rendering only.
	if pkg.DisplayTag != "23.8.1076" {
		t.Errorf("display tag = %q, want 23.8.1076", pkg.DisplayTag)
	}
	if pkg.DisplayRepository != "cfx-5000-k8s" {
		t.Errorf("display repository = %q, want cfx-5000-k8s", pkg.DisplayRepository)
	}

	// And what a listing showed must be typeable back, or the abbreviation is a
	// trap: someone copies what is on screen and gets "not found" for a package
	// plainly in front of them.
	for _, ref := range []string{
		"orb_23.8.1076",
		"23.8.1076",
		"orbs/cfx-5000-k8s:orb_23.8.1076",
		"cfx-5000-k8s:23.8.1076",
	} {
		row, err := packages.GetPackage(t.Context(), "vendor-a", ref)
		if err != nil {
			t.Errorf("GetPackage(%q): %v", ref, err)
			continue
		}
		if row.ID != pkg.ID {
			t.Errorf("GetPackage(%q) resolved to package %d, want %d", ref, row.ID, pkg.ID)
		}
	}
}

// The other half of the same rule: a source that declares no vendor gets no
// shortening, whatever its repository paths happen to look like.
func TestScanShortensNothingWithoutAVendor(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("orbs/cfx-5000-k8s", "orb_23.8.1076", fakeregistry.NewLayer("plain"))

	s, packages, _ := plainScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	list, err := packages.ListPackages(t.Context(), store.ListPackagesFilter{ProductName: "vendor-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d packages, want 1", len(list))
	}

	// The paths look exactly like NEAR's. Without a source saying so, that is a
	// coincidence and nothing may act on it.
	if got := list[0].DisplayTag; got != "" {
		t.Errorf("display tag = %q; a source with no vendor must not be shortened", got)
	}
	if got := list[0].DisplayRepository; got != "" {
		t.Errorf("display repository = %q; a source with no vendor must not be shortened", got)
	}
}

// plainScanner is groupingScanner with the standard layout - a source that
// declares no vendor at all.
func plainScanner(t *testing.T, reg *fakeregistry.Registry, repo string) (*Scanner, *store.Packages, int64) {
	t.Helper()

	doc := fmt.Sprintf(`
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: %s
      repository: %s
      anonymous: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      anonymous: true
      default: true
`, reg.Host(), repo)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := product.NewLoader(dir, product.NewSecretResolver(t.TempDir())).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invalid) > 0 {
		t.Fatalf("config invalid: %v", res.Invalid[0].Err)
	}
	p := res.Valid[0]

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "plain.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	rec, err := catalog.NewCatalog(st).Reconcile(t.Context(), res.Valid)
	if err != nil {
		t.Fatal(err)
	}
	ref := rec.Products["vendor-a"]

	newClient, err := SourceClientFactory(p, p.Spec.Sources[0], product.NewSecretResolver(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}

	packages := store.NewPackages(st)
	// vendors.Standard is what a source with no `vendor` resolves to.
	s, err := NewScanner(ScannerConfig{
		Packages: packages, Product: p, ProductID: ref.ID, SourceName: "vendor",
		NewClient: newClient, RepoIDs: ref.Repositories, Layout: vendors.Standard{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, packages, ref.ID
}

// THE BUG THIS FIXES, reproduced exactly.
//
// Turning `vendor: near` on had no effect on packages already in the database.
// Display names were computed once, at discovery, and discovery SKIPS a tag it
// has already recorded - one HEAD, no fetch, no grouping - so the rows kept
// their unshortened names forever. Re-scanning, which is the obvious thing to
// try, changed nothing.
//
// The fix reconciles stored display names against the layout on every scan,
// which costs one query per repository and no registry traffic.
func TestVendorSetAfterDiscoveryRenamesExistingPackages(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	// First: scanned with no vendor, exactly as a source starts life.
	plain, packages, _ := plainScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := plain.Scan(t.Context()); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	before := onlyPackage(t, packages, "orb_23.8.1076")
	if before.DisplayTag != "" {
		t.Fatalf("display tag = %q before a vendor was set, want empty", before.DisplayTag)
	}

	// Now the operator adds `vendor: near` and the next scan runs. Same store,
	// same registry, same tags - only the layout differs.
	vendored := rescanWithLayout(t, plain, wrapperLayout{})
	res, err := vendored.Scan(t.Context())
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}

	// Nothing new was discovered: the tags were all already known. That is the
	// condition under which the old code did nothing at all.
	if res.New != 0 {
		t.Errorf("discovered %d new packages on a re-scan, want 0", res.New)
	}
	if res.Renamed == 0 {
		t.Error("no display names were corrected; setting `vendor` did not take effect " +
			"on packages that already existed")
	}

	after := onlyPackage(t, packages, "orb_23.8.1076")
	if after.DisplayTag != "23.8.1076" {
		t.Errorf("display tag = %q after setting the vendor, want 23.8.1076", after.DisplayTag)
	}
	// The stored tag is untouched. Shortening is cosmetic from beginning to end.
	if after.Tag != "orb_23.8.1076" {
		t.Errorf("stored tag = %q, want orb_23.8.1076 - renaming must not touch the identity", after.Tag)
	}
	// And the shortened form now resolves, which is the whole point of storing it.
	if _, err := packages.GetPackage(t.Context(), "vendor-a", "23.8.1076"); err != nil {
		t.Errorf("the corrected display tag does not resolve as input: %v", err)
	}
}

// The correction runs in both directions: removing a vendor must clear the
// names again, or a source that was misconfigured stays misdescribed.
func TestRemovingTheVendorClearsDisplayNames(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	vendored, packages, _ := groupingScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := vendored.Scan(t.Context()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if got := onlyPackage(t, packages, "orb_23.8.1076").DisplayTag; got != "23.8.1076" {
		t.Fatalf("display tag = %q after a vendored scan, want 23.8.1076", got)
	}

	plain := rescanWithLayout(t, vendored, vendors.Standard{})
	res, err := plain.Scan(t.Context())
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if res.Renamed == 0 {
		t.Error("removing the vendor corrected nothing")
	}
	if got := onlyPackage(t, packages, "orb_23.8.1076").DisplayTag; got != "" {
		t.Errorf("display tag = %q after removing the vendor, want empty", got)
	}
}

// rescanWithLayout applies the config edit in place.
//
// In place rather than by copying: a Scanner carries mutexes and a client
// cache, and the whole point of the test is that the STORE is the same one the
// first scan wrote to. The loop rebuilds its scanners on a config reload; what
// survives that reload, and what this reproduces, is the database.
func rescanWithLayout(t *testing.T, s *Scanner, l vendors.Layout) *Scanner {
	t.Helper()
	s.layout = l
	return s
}

func onlyPackage(t *testing.T, packages *store.Packages, tag string) store.PackageRow {
	t.Helper()
	row, err := packages.GetPackage(t.Context(), "vendor-a", tag)
	if err != nil {
		t.Fatalf("get package %s: %v", tag, err)
	}
	return row
}

// THE SIGNATURE HALF of the same "vendor set after the fact" problem, and the
// more serious one.
//
// Grouping runs over the tags a scan finds NEW - deliberately, because that is
// what keeps a re-scan of an unchanged repository down to one HEAD per tag. The
// consequence: a repository scanned before its source declared a vendor was
// never grouped again. Its packages read `unknown` forever, carried no
// signature relation, and had NO TRANSFER ROOT - so moving one would have taken
// the payload and left the signature behind, which is precisely what the layout
// exists to prevent. Re-scanning could not fix it, because re-scanning is the
// path that skips known tags.
func TestVendorSetAfterDiscoveryRegroupsExistingPackages(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	// Scanned with no vendor: three tags, three packages, nothing related.
	plain, packages, _ := plainScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := plain.Scan(t.Context()); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	before := listPackages(t, packages)
	if len(before) != 3 {
		t.Fatalf("listed %d packages under the standard layout, want 3", len(before))
	}
	payload := onlyPackage(t, packages, "orb_23.8.1076")
	if payload.SignatureStatus != "unknown" {
		t.Fatalf("signature status = %q before a vendor was set, want unknown", payload.SignatureStatus)
	}

	// The operator sets `vendor: near`. The next scan finds no new tags at all.
	vendored := rescanWithLayout(t, plain, wrapperLayout{})
	res, err := vendored.Scan(t.Context())
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if res.New != 0 {
		t.Errorf("discovered %d new packages on a re-scan, want 0", res.New)
	}
	if res.Regrouped == 0 {
		t.Fatal("nothing was regrouped; setting `vendor` did not reach packages that already existed")
	}

	payload = onlyPackage(t, packages, "orb_23.8.1076")

	// The three things a transfer and a security decision actually read.
	if payload.SignatureStatus != "signed" {
		t.Errorf("signature status = %q, want signed", payload.SignatureStatus)
	}
	if payload.TransferRootDigest == "" {
		t.Error("no transfer root recorded - a transfer would move the payload without its signature")
	}
	if payload.TransferRootTag != "signed_orb_23.8.1076" {
		t.Errorf("transfer root tag = %q, want signed_orb_23.8.1076", payload.TransferRootTag)
	}

	rels, err := packages.ListRelations(t.Context(), payload.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, r := range rels {
		roles[r.Role] = true
	}
	if !roles["signature"] {
		t.Error("no signature relation attached")
	}
	if !roles["wrapper"] {
		t.Error("no wrapper relation attached")
	}

	// And the two accessory tags stop being listed as releases of their own -
	// which is most of the noise the layout exists to remove. They are not
	// deleted: a transfer may reference them, and what was shipped has to stay
	// answerable.
	after := listPackages(t, packages)
	if len(after) != 1 {
		names := make([]string, len(after))
		for i, p := range after {
			names[i] = p.Tag
		}
		t.Errorf("listed %v after regrouping, want only the payload", names)
	}
	if _, err := packages.GetPackage(t.Context(), "vendor-a", "signature_orb_23.8.1076"); err != nil {
		t.Errorf("an absorbed accessory must stay reachable by name: %v", err)
	}
}

// The pass must run ONCE. Keying it off the recorded layout name rather than
// off a symptom is what guarantees that - a symptom such as "some package still
// reads unknown" is a state a repository can legitimately stay in forever, and
// would re-fetch every tag on every scan for the rest of time.
func TestRegroupingDoesNotRepeat(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	plain, _, _ := plainScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := plain.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	vendored := rescanWithLayout(t, plain, wrapperLayout{})
	first, err := vendored.Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.Regrouped == 0 {
		t.Fatal("the first scan under the new vendor regrouped nothing")
	}

	second, err := vendored.Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Regrouped != 0 {
		t.Errorf("regrouped %d packages on a second pass; the work must happen once",
			second.Regrouped)
	}
}

// Removing the vendor puts the accessories back. A repository misconfigured
// with the wrong vendor must be recoverable by fixing the configuration.
func TestRemovingTheVendorRestoresAccessories(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	plain, packages, _ := plainScanner(t, reg, "orbs/cfx-5000-k8s")
	if _, err := plain.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	vendored := rescanWithLayout(t, plain, wrapperLayout{})
	if _, err := vendored.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(listPackages(t, packages)); got != 1 {
		t.Fatalf("listed %d packages while vendored, want 1", got)
	}

	back := rescanWithLayout(t, vendored, vendors.Standard{})
	if _, err := back.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(listPackages(t, packages)); got != 3 {
		t.Errorf("listed %d packages after removing the vendor, want all 3 back", got)
	}
}

func listPackages(t *testing.T, packages *store.Packages) []store.PackageRow {
	t.Helper()
	rows, err := packages.ListPackages(t.Context(), store.ListPackagesFilter{ProductName: "vendor-a"})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// THE FILTER BUG, reproduced from a live configuration.
//
// A NEAR source configured exactly as an operator would write it:
//
//	tagFilters:
//	  include: ['^orb_']
//
// which says "track the release tags". Under the old code that also, silently,
// meant "never look at a signature": `signed_orb_X` and `signature_orb_X` do
// not match `^orb_`, so they never entered the work list, the Layout never saw
// them, and every package in the catalogue came out `unsigned` - a confident,
// wrong answer, with nothing in any output pointing at the filter.
func TestTagFiltersDoNotHideSignatures(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	s, packages, _ := filteredScanner(t, reg, "orbs/cfx-5000-k8s", "^orb_")

	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// The filter did its job: one release admitted out of three tags.
	if res.TagsListed != 3 {
		t.Errorf("listed %d tags, want 3", res.TagsListed)
	}
	if res.TagsAdmitted != 1 {
		t.Errorf("admitted %d tags, want 1 - the filter selects releases", res.TagsAdmitted)
	}
	if res.New != 1 {
		t.Fatalf("recorded %d packages, want 1", res.New)
	}

	pkg := onlyPackage(t, packages, "orb_23.8.1076")

	// And the signature was still found, because a filter selects PACKAGES and
	// the Layout is allowed to reach the tags a package is made of.
	if pkg.SignatureStatus != "signed" {
		t.Errorf("signature status = %q, want signed - the accessory tags were not reached", pkg.SignatureStatus)
	}
	if pkg.TransferRootTag != "signed_orb_23.8.1076" {
		t.Errorf("transfer root tag = %q, want signed_orb_23.8.1076", pkg.TransferRootTag)
	}

	// The accessory tags must NOT have become packages: they were never
	// admitted, and pulling them in is a mechanism, not a selection.
	if got := len(listPackages(t, packages)); got != 1 {
		t.Errorf("listed %d packages, want 1 - accessory tags must not become releases", got)
	}
}

// A release the vendor did NOT sign must read `unsigned`, not `signed`, and
// must not cost a request per scan looking for a tag that does not exist.
func TestAnUnsignedReleaseStaysUnsigned(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	// Only the payload: no wrapper, no signature.
	reg.AddImage("orbs/cfx-5000-k8s", "orb_23.8.1076", fakeregistry.NewLayer("payload"))

	s, packages, _ := filteredScanner(t, reg, "orbs/cfx-5000-k8s", "^orb_")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	pkg := onlyPackage(t, packages, "orb_23.8.1076")
	if pkg.SignatureStatus != "unsigned" {
		t.Errorf("signature status = %q, want unsigned - the layout looked and found none",
			pkg.SignatureStatus)
	}
}

// The steady state must stay cheap. Accessory tags are resolved only for
// releases actually being grouped, so a re-scan that finds nothing new must not
// re-fetch a wrapper and a signature per release, forever.
func TestAccessoryTagsAreNotRefetchedOnAQuietScan(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	s, _, _ := filteredScanner(t, reg, "orbs/cfx-5000-k8s", "^orb_")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	before := reg.ManifestCalls.Load()
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	quiet := reg.ManifestCalls.Load() - before

	// One HEAD for the admitted release, and nothing else. Two more would mean
	// the wrapper and the signature are being re-resolved every fifteen minutes
	// for the life of the deployment.
	if quiet != 1 {
		t.Errorf("a scan that found nothing new made %d manifest requests, want 1; "+
			"accessory tags must only be resolved for releases actually being grouped", quiet)
	}
}

// filteredScanner is groupingScanner with a real tagFilters.include, which is
// the configuration that exposed the bug.
func filteredScanner(
	t *testing.T, reg *fakeregistry.Registry, repo, include string,
) (*Scanner, *store.Packages, int64) {
	t.Helper()

	doc := fmt.Sprintf(`
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: %s
      repository: %s
      anonymous: true
      vendor: test-wrapper
      discovery:
        tagFilters:
          include: ['%s']
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      anonymous: true
      default: true
`, reg.Host(), repo, include)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := product.NewLoader(dir, product.NewSecretResolver(t.TempDir())).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invalid) > 0 {
		t.Fatalf("config invalid: %v", res.Invalid[0].Err)
	}
	p := res.Valid[0]

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "f.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	rec, err := catalog.NewCatalog(st).Reconcile(t.Context(), res.Valid)
	if err != nil {
		t.Fatal(err)
	}
	ref := rec.Products["vendor-a"]

	newClient, err := SourceClientFactory(p, p.Spec.Sources[0], product.NewSecretResolver(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}

	packages := store.NewPackages(st)
	sc, err := NewScanner(ScannerConfig{
		Packages: packages, Product: p, ProductID: ref.ID, SourceName: "vendor",
		NewClient: newClient, RepoIDs: ref.Repositories, Layout: wrapperLayout{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sc, packages, ref.ID
}

// Inspecting a signed package must record WHAT the signature is, not merely
// that one exists.
//
// Discovery can only record existence: it sees the wrapper's descriptor of the
// signature manifest and stops, because fetching that manifest is exactly the
// walk discovery defers. So the bytes a verifier reads were recorded like any
// other blob and were indistinguishable from a chart layer beside them.
//
// Verification is a later milestone. This is the material it will need,
// captured at the only moment it is free - the walk inspect was doing anyway.
func TestInspectRecordsTheSignatureMaterial(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	s, packages, _ := filteredScanner(t, reg, "orbs/cfx-5000-k8s", "^orb_")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	pkg := onlyPackage(t, packages, "orb_23.8.1076")

	// Before inspection: the signature is known to exist, and nothing more.
	before := signatureRelation(t, packages, pkg.ID)
	if before.Digest == "" {
		t.Fatal("no signature relation recorded by discovery")
	}
	if before.BlobDigest != "" {
		t.Errorf("discovery claimed to know the signature blob %q without fetching the manifest",
			before.BlobDigest)
	}
	if before.ResolvedAt != "" {
		t.Errorf("resolvedAt = %q before any inspection", before.ResolvedAt)
	}

	client, err := s.clientFor("orbs/cfx-5000-k8s")
	if err != nil {
		t.Fatal(err)
	}
	res, err := InspectPackage(t.Context(), packages, pkg, client, 4)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if res.SignatureResolved != 1 {
		t.Errorf("resolved %d signatures, want 1", res.SignatureResolved)
	}

	after := signatureRelation(t, packages, pkg.ID)
	if after.BlobDigest == "" {
		t.Fatal("the signature blob was not recorded, so a verifier still has to " +
			"re-derive the vendor's layout to find it")
	}
	if after.BlobDigest == after.Digest {
		t.Error("the blob digest is the MANIFEST digest; the material is the layer inside it")
	}
	if after.BlobSize <= 0 {
		t.Errorf("blob size = %d, want the real size", after.BlobSize)
	}
	if after.ResolvedAt == "" {
		t.Error("no resolvedAt, so 'carries no blob' cannot be told from 'never inspected'")
	}

	// Idempotent: the tree under a digest cannot change, so a second inspection
	// resolves the same material rather than a different answer.
	if _, err := InspectPackage(t.Context(), packages, pkg, client, 4); err != nil {
		t.Fatalf("second inspect: %v", err)
	}
	again := signatureRelation(t, packages, pkg.ID)
	if again.BlobDigest != after.BlobDigest {
		t.Errorf("a second inspection resolved a different blob: %q then %q",
			after.BlobDigest, again.BlobDigest)
	}
}

func signatureRelation(t *testing.T, packages *store.Packages, packageID int64) store.RelationRow {
	t.Helper()
	rels, err := packages.ListRelations(t.Context(), packageID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rels {
		if r.Role == "signature" {
			return r
		}
	}
	return store.RelationRow{}
}

// Inspect must measure what a TRANSFER moves, which for a wrapped release is
// the wrapper's tree - payload plus signature - not the payload alone.
//
// The two disagreed: the planner honoured the transfer root while inspect
// walked the package's own manifest. So `packages inspect` reported a size that
// excluded the signature while saying, in as many words, that these were the
// numbers a transfer would move. The recorded tree was the payload's, which is
// also why the signature manifest was never fetched and its material never
// resolved.
func TestInspectWalksTheTransferRoot(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	seedRelease(t, reg, "orbs/cfx-5000-k8s", "23.8.1076")

	s, packages, _ := filteredScanner(t, reg, "orbs/cfx-5000-k8s", "^orb_")
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	pkg := onlyPackage(t, packages, "orb_23.8.1076")
	if pkg.TransferRootDigest == "" {
		t.Fatal("no transfer root, so this test proves nothing")
	}

	client, err := s.clientFor("orbs/cfx-5000-k8s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPackage(t.Context(), packages, pkg, client, 4); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	artifacts, err := packages.ListArtifacts(t.Context(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one root, and it is the wrapper.
	var roots []string
	byDigest := map[string]store.ArtifactRow{}
	for _, a := range artifacts {
		byDigest[a.Digest] = a
		if a.Depth == 0 {
			roots = append(roots, a.Digest)
		}
	}
	if len(roots) != 1 {
		t.Errorf("tree has %d roots (%v); re-rooting must not leave two", len(roots), roots)
	}
	if len(roots) == 1 && roots[0] != pkg.TransferRootDigest {
		t.Errorf("tree root = %s, want the transfer root %s", roots[0], pkg.TransferRootDigest)
	}

	// The payload is now a CHILD of the wrapper rather than the root.
	if payload, ok := byDigest[pkg.ManifestDigest]; !ok {
		t.Error("the payload is missing from the tree")
	} else if payload.Depth == 0 || payload.ParentID == nil {
		t.Errorf("the payload is still a root (depth %d); re-rooting did not reparent it",
			payload.Depth)
	}

	// And the measured size now includes the signature, because a transfer will
	// move it.
	measured := onlyPackage(t, packages, "orb_23.8.1076")
	if measured.TotalBytes == nil {
		t.Fatal("no measured size after inspection")
	}
	sig := signatureRelation(t, packages, pkg.ID)
	if _, ok := byDigest[sig.Digest]; !ok {
		t.Error("the signature manifest is not in the measured tree, so the reported " +
			"transfer size is smaller than the transfer")
	}
}
