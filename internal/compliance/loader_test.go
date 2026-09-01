package compliance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loader(t *testing.T) *compliance.Loader {
	t.Helper()
	c, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	return &compliance.Loader{Compiler: c}
}

const goodPack = `apiVersion: softwaregateway.io/v1alpha1
kind: PolicyPack
metadata:
  name: acme-platform
  prefix: ACME
  version: 1.0.0
spec:
  checks:
    - id: ACME-01
      title: Containers declare a memory limit
      severity: block
      appliesTo:
        kinds: [Deployment]
        containers: all
      assert:
        required: [resources.limits.memory]
`

func packStatus(cat *compliance.Catalog, name string) (compliance.PackStatus, bool) {
	for _, p := range cat.Packs() {
		if p.Name == name {
			return p, true
		}
	}
	return compliance.PackStatus{}, false
}

func TestLoadGoodPack(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "acme/pack.yaml", goodPack)

	cat, err := loader(t).Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cat.Len() != 1 {
		t.Fatalf("want 1 check, got %d", cat.Len())
	}
	st, ok := packStatus(cat, "acme-platform")
	if !ok || !st.OK() {
		t.Fatalf("pack did not load cleanly: %+v", st)
	}
	if !strings.HasPrefix(cat.BundleDigest, "sha256:") {
		t.Errorf("bundle digest = %q, want a sha256", cat.BundleDigest)
	}
	// A check must carry the pack that defined it, or the catalogue page cannot
	// say who to ask about a rule.
	ch, _ := cat.Check("ACME-01")
	if ch.Pack != "acme-platform" {
		t.Errorf("check pack = %q", ch.Pack)
	}
}

// The digest answers "which rulebook produced this report" a year later, so it
// must not move when nothing did, and must move when something did.
func TestBundleDigestIsStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "acme/pack.yaml", goodPack)
	a, _ := loader(t).Load(dir)
	b, _ := loader(t).Load(dir)
	if a.BundleDigest != b.BundleDigest {
		t.Fatal("two loads of the same directory produced different digests")
	}
	write(t, dir, "acme/pack.yaml", strings.Replace(goodPack, "severity: block", "severity: warn", 1))
	c, _ := loader(t).Load(dir)
	if c.BundleDigest == a.BundleDigest {
		t.Fatal("a severity change did not change the bundle digest")
	}
}

// One broken pack must not take the others down, and must not disappear.
func TestABrokenPackIsIsolatedAndReported(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "acme/pack.yaml", goodPack)
	write(t, dir, "broken/pack.yaml", `apiVersion: softwaregateway.io/v1alpha1
kind: PolicyPack
metadata:
  name: broken
  prefix: BRK
spec:
  checks:
    - id: BRK-01
      title: nonsense
      severity: block
      appliesTo:
        kinds: [Pod]
      assert:
        expr: pdbForr(self) != null
`)
	cat, err := loader(t).Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cat.Len() != 1 {
		t.Errorf("the good pack should still be loaded; got %d checks", cat.Len())
	}
	st, ok := packStatus(cat, "broken")
	if !ok {
		t.Fatal("the broken pack vanished; a check that disappears looks like a check that passed")
	}
	if st.OK() {
		t.Fatal("the broken pack reported no errors")
	}
	if !strings.Contains(strings.Join(st.Errors, " "), "BRK-01") {
		t.Errorf("the error does not name the check: %v", st.Errors)
	}
}

// A pack is atomic. One bad check must not leave the other half registered.
func TestAPackWithOneBadCheckRegistersNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "p.yaml", `apiVersion: softwaregateway.io/v1alpha1
kind: PolicyPack
metadata: {name: half, prefix: HALF}
spec:
  checks:
    - id: HALF-01
      title: fine
      severity: info
      appliesTo: {kinds: [Pod]}
      assert: {required: [spec]}
    - id: HALF-02
      title: broken
      severity: info
      appliesTo: {kinds: [Pod]}
      assert: {expr: "nope(self)"}
`)
	cat, _ := loader(t).Load(dir)
	if cat.Len() != 0 {
		t.Errorf("half a pack loaded: %d checks registered", cat.Len())
	}
}

// Two packs owning one prefix means a check ID is ambiguous.
func TestPrefixCollisionRejectsTheSecondPack(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a-first.yaml", goodPack)
	write(t, dir, "b-second.yaml", strings.Replace(goodPack, "name: acme-platform", "name: acme-copy", 1))
	cat, _ := loader(t).Load(dir)
	if cat.Len() != 1 {
		t.Errorf("want 1 check registered, got %d", cat.Len())
	}
	st, ok := packStatus(cat, "acme-copy")
	if !ok || st.OK() {
		t.Fatalf("the second pack should be rejected: %+v", st)
	}
	if !strings.Contains(strings.Join(st.Errors, " "), "acme-platform") {
		t.Errorf("the rejection does not name the owner: %v", st.Errors)
	}
}

// A typo in a field name must be an error, not a control that quietly does
// something other than what its author believes.
func TestUnknownFieldIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "p.yaml", strings.Replace(goodPack, "severity: block", "severtiy: block", 1))
	cat, _ := loader(t).Load(dir)
	if cat.Len() != 0 {
		t.Error("a pack with a misspelled field loaded")
	}
}

// A check that declares engine: builtin with nothing behind it is present in
// the catalogue and absent from the run - worse than missing from both.
func TestBuiltinWithNoImplementationIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "p.yaml", `apiVersion: softwaregateway.io/v1alpha1
kind: PolicyPack
metadata: {name: b, prefix: BLT}
spec:
  checks:
    - id: BLT-01
      title: needs Go
      severity: block
      engine: builtin
      appliesTo: {kinds: [Pod]}
`)
	cat, _ := loader(t).Load(dir)
	st, _ := packStatus(cat, "b")
	if st.OK() {
		t.Fatal("a builtin check with no implementation was accepted")
	}
}

// A check must not claim an ID outside its pack's namespace, or the guarantee
// that a prefix identifies an owner is worth nothing.
func TestCheckOutsideItsPackPrefixIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "p.yaml", strings.Replace(goodPack, "id: ACME-01", "id: OTHER-01", 1))
	cat, _ := loader(t).Load(dir)
	if cat.Len() != 0 {
		t.Error("a check claiming another prefix was registered")
	}
}

// The policy mount is optional and its fixtures are not packs.
func TestMissingDirectoryAndTestdataAreSkipped(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "acme/pack.yaml", goodPack)
	write(t, dir, "acme/testdata/good/chart/values.yaml", "replicas: 1\n")
	cat, err := loader(t).Load(dir, filepath.Join(dir, "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing policy directory should not be an error: %v", err)
	}
	if cat.Len() != 1 {
		t.Errorf("want 1 check, got %d", cat.Len())
	}
	for _, p := range cat.Packs() {
		if !p.OK() {
			t.Errorf("a fixture was read as a pack: %+v", p)
		}
	}
}
