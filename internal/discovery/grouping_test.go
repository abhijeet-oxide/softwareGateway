package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// These exercise the whole path — a registry laid out the way NEAR lays one
// out, through a real scan, into real rows — rather than the Layout in
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
		p := vendors.Package{Tag: s.Tag, Descriptor: s.Descriptor}
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
      repositories: preserve
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
		t.Fatalf("recorded %d packages, want 1 — the other two are accessories", res.New)
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
		t.Error("no transfer root recorded — a transfer would move the payload without its signature")
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
// `unknown` — the layout looked, and found none.
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
