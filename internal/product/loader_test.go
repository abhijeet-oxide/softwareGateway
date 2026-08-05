package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: %s
spec:
  sources:
    - name: primary
      registry: registry.vendor-a.example.com
      repository: platform/suite
      anonymous: true
      discovery:
        interval: 15m
  targets:
    - name: lab
      registry: internal.azurecr.io
      repository: vendor-a/platform
      anonymous: true
      default: true
`

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadsValidDirectory(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"a.yaml":    strings.Replace(validDoc, "%s", "vendor-a", 1),
		"b.yml":     strings.Replace(validDoc, "%s", "vendor-b", 1),
		"notes.txt": "ignored",
	})

	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Valid) != 2 {
		t.Fatalf("expected 2 valid products, got %d", len(res.Valid))
	}
	if len(res.Invalid) != 0 {
		t.Fatalf("expected 0 invalid, got %v", res.Invalid)
	}
}

func TestMissingDirectoryIsNotAnError(t *testing.T) {
	// The zero-setup development path must work before any product exists.
	res, err := NewLoader(filepath.Join(t.TempDir(), "absent"), nil).Load()
	if err != nil {
		t.Fatalf("a missing products directory should not be an error: %v", err)
	}
	if len(res.Valid) != 0 || len(res.Invalid) != 0 {
		t.Fatal("expected empty result")
	}
}

func TestOneBadProductDoesNotBlockOthers(t *testing.T) {
	// Fail-closed PER PRODUCT: a syntax error in vendor-b must never stop
	// vendor-a from replicating. docs/design/02 section 7.
	dir := writeDir(t, map[string]string{
		"good.yaml": strings.Replace(validDoc, "%s", "vendor-a", 1),
		"bad.yaml":  "apiVersion: softwaregateway.io/v1alpha1\nkind: Product\nthis: is: not: yaml",
	})

	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatalf("directory-level load should succeed: %v", err)
	}
	if len(res.Valid) != 1 || res.Valid[0].Metadata.Name != "vendor-a" {
		t.Fatalf("the good product must still load, got %+v", res.Valid)
	}
	if len(res.Invalid) != 1 {
		t.Fatalf("expected 1 invalid, got %d", len(res.Invalid))
	}
}

func TestRejectsUnknownFields(t *testing.T) {
	// A typo such as `tagPatern` would otherwise be silently ignored, leaving
	// a rule that never matches and a user with no indication why.
	dir := writeDir(t, map[string]string{
		"typo.yaml": strings.Replace(validDoc, "%s", "vendor-a", 1) + `
  autoDownload:
    enabled: true
    rules:
      - name: ga
        tagPatern: '^v1$'
`,
	})

	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invalid) != 1 {
		t.Fatalf("a misspelled field must be rejected, got %d invalid", len(res.Invalid))
	}
	if !strings.Contains(res.Invalid[0].Err.Error(), "tagPatern") {
		t.Fatalf("error should name the unknown field, got %v", res.Invalid[0].Err)
	}
}

func TestDuplicateProductNamesRejected(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"a.yaml": strings.Replace(validDoc, "%s", "same-name", 1),
		"b.yaml": strings.Replace(validDoc, "%s", "same-name", 1),
	})

	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Valid) != 1 {
		t.Fatalf("expected the first to win, got %d valid", len(res.Valid))
	}
	if len(res.Invalid) != 1 || !strings.Contains(res.Invalid[0].Err.Error(), "already declared") {
		t.Fatalf("expected a duplicate-name error, got %+v", res.Invalid)
	}
}

func TestDefaultsApplied(t *testing.T) {
	dir := writeDir(t, map[string]string{"a.yaml": strings.Replace(validDoc, "%s", "vendor-a", 1)})
	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	p := res.Valid[0]

	if p.Spec.Sources[0].Type != RegistryGeneric {
		t.Fatalf("registry type should default to generic, got %q", p.Spec.Sources[0].Type)
	}
	if got := p.Spec.Targets[0].RateLimits.MaxConcurrentUploads; got != DefaultMaxConcurrentUploads {
		t.Fatalf("target upload budget should default, got %d", got)
	}
	// Sources are read-only, so an upload budget would be meaningless.
	if got := p.Spec.Sources[0].RateLimits.MaxConcurrentUploads; got != 0 {
		t.Fatalf("source upload budget should be zero, got %d", got)
	}
	if p.ConfigHash == "" {
		t.Fatal("config hash should be recorded for audit")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	dir := writeDir(t, map[string]string{"a.yaml": strings.Replace(validDoc, "%s", "vendor-a", 1)})
	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Valid[0].Spec.Sources[0].Discovery.Interval.Duration().Minutes(); got != 15 {
		t.Fatalf("interval should parse as 15m, got %v minutes", got)
	}
}

// rewriteAndReload simulates what kubelet does on a ConfigMap update: the same
// file path gains new content, and the loader runs again.
func rewriteAndReload(t *testing.T, dir, file, body string, reg *Registry) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Swap(res)
}

func TestRegistryRetainsPreviousVersionOnInvalidReload(t *testing.T) {
	// A bad edit degrades to "the change did not take effect", never to
	// "the product silently stopped replicating". docs/design/02 section 7.
	dir := t.TempDir()
	reg := NewRegistry()

	rewriteAndReload(t, dir, "a.yaml", strings.Replace(validDoc, "%s", "vendor-a", 1), reg)
	if reg.Count() != 1 {
		t.Fatal("expected the product to load")
	}

	// A VALIDATION failure — the document parses, so the name is known.
	rewriteAndReload(t, dir, "a.yaml",
		strings.Replace(validDoc, "%s", "vendor-a", 1)+"\n  autoDownload:\n    enabled: true\n    rules:\n      - name: r\n        tagPattern: '^v('\n", reg)

	if _, ok := reg.Get("vendor-a"); !ok {
		t.Fatal("the previous valid version must be retained after an invalid reload")
	}
	if len(reg.Invalid()) != 1 {
		t.Fatalf("the failure must still be reported, got %d", len(reg.Invalid()))
	}
}

func TestRegistryRetainsPreviousVersionWhenFileNoLongerParses(t *testing.T) {
	// The harder case: the document fails to PARSE, so it yields no product
	// name at all. Without file-to-name tracking the product would be silently
	// dropped — a stray syntax error would stop replication rather than merely
	// failing to apply.
	dir := t.TempDir()
	reg := NewRegistry()

	rewriteAndReload(t, dir, "a.yaml", strings.Replace(validDoc, "%s", "vendor-a", 1), reg)
	if reg.Count() != 1 {
		t.Fatal("expected the product to load")
	}

	rewriteAndReload(t, dir, "a.yaml", "this: is: not: valid: yaml:\n  - [", reg)

	if _, ok := reg.Get("vendor-a"); !ok {
		t.Fatal("an unparseable file must still retain the last known-good product")
	}
	if len(reg.Invalid()) != 1 {
		t.Fatalf("the parse failure must still be reported, got %d", len(reg.Invalid()))
	}

	// And a subsequent good edit must take effect.
	rewriteAndReload(t, dir, "a.yaml", strings.Replace(validDoc, "%s", "vendor-a", 1), reg)
	if len(reg.Invalid()) != 0 {
		t.Fatalf("recovery should clear the error, got %v", reg.Invalid())
	}
}

func TestRegistryDropsProductWhenFileIsRemoved(t *testing.T) {
	// Retention applies to a BROKEN file, not a DELETED one. Removing a
	// product from Git must actually remove it, or a decommissioned vendor
	// would keep being polled forever.
	dir := t.TempDir()
	reg := NewRegistry()

	rewriteAndReload(t, dir, "a.yaml", strings.Replace(validDoc, "%s", "vendor-a", 1), reg)
	if reg.Count() != 1 {
		t.Fatal("expected the product to load")
	}

	if err := os.Remove(filepath.Join(dir, "a.yaml")); err != nil {
		t.Fatal(err)
	}
	res, err := NewLoader(dir, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Swap(res)

	if _, ok := reg.Get("vendor-a"); ok {
		t.Fatal("a deleted product must be removed, not retained")
	}
}

func TestRegistryListIsSorted(t *testing.T) {
	reg := NewRegistry()
	reg.Swap(LoadResult{Valid: []*Product{
		{Metadata: Metadata{Name: "zulu"}},
		{Metadata: Metadata{Name: "alpha"}},
		{Metadata: Metadata{Name: "mike"}},
	}})

	got := reg.List()
	want := []string{"alpha", "mike", "zulu"}
	for i, p := range got {
		if p.Metadata.Name != want[i] {
			t.Fatalf("position %d: got %q, want %q", i, p.Metadata.Name, want[i])
		}
	}
}

func TestDefaultTargetResolution(t *testing.T) {
	p := valid()
	tgt, ok := p.DefaultTarget()
	if !ok || tgt.Name != "lab" {
		t.Fatalf("explicit default not found: %v %v", tgt, ok)
	}

	// A single non-promotionOnly target is the default implicitly.
	p.Spec.Targets[0].Default = false
	if tgt, ok = p.DefaultTarget(); !ok || tgt.Name != "lab" {
		t.Fatal("a sole target should serve as the default")
	}

	// ...but a sole promotionOnly target must not.
	p.Spec.Targets[0].PromotionOnly = true
	if _, ok = p.DefaultTarget(); ok {
		t.Fatal("a promotionOnly target must never be the implicit default")
	}
}
