package baseline_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/baseline"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
)

// loadShipped compiles the shipped pack the way the Coordinator will.
func loadShipped(t *testing.T) *compliance.Catalog {
	t.Helper()
	files, err := baseline.Files()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	cat, err := (&compliance.Loader{Compiler: comp}).Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// The merge gate: the pack this binary ships must compile. A check that does
// not load is a check that silently does not run.
func TestShippedPackCompiles(t *testing.T) {
	cat := loadShipped(t)
	for _, p := range cat.Packs() {
		if p.OK() {
			continue
		}
		t.Errorf("pack %s (%s) did not load:", p.Name, p.Path)
		for _, e := range p.Errors {
			t.Errorf("    %s", e)
		}
	}
	if cat.Len() == 0 {
		t.Fatal("the shipped pack registered no checks")
	}
	t.Logf("%d checks loaded", cat.Len())
}

// Every shipped check must explain itself. A vendor reading the report needs
// to know what was required and why, and a check that only has a title makes
// them ask.
func TestEveryShippedCheckExplainsItself(t *testing.T) {
	checks := loadShipped(t).Checks()
	if len(checks) == 0 {
		t.Fatal("no checks loaded; this test would otherwise pass by inspecting nothing")
	}
	for _, c := range checks {
		if strings.TrimSpace(c.Description) == "" {
			t.Errorf("%s has no description; the vendor report would print a title and nothing else", c.ID)
		}
		if strings.TrimSpace(c.Rationale) == "" {
			t.Errorf("%s has no rationale; nobody will know why it exists in two years", c.ID)
		}
		if strings.TrimSpace(c.Remediation) == "" {
			t.Errorf("%s has no remediation; the report tells a vendor they are wrong and not what to do", c.ID)
		}
		if c.Category == "" {
			t.Errorf("%s has no category; the report cannot group it", c.ID)
		}
		if c.Tier == 0 {
			t.Errorf("%s declares no tier", c.ID)
		}
	}
}
