package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/baseline"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
)

// The dev estate's charts have to be real charts.
//
// A layer of filler under a chart media type exercises exactly one code path -
// the one that reports "not a gzip archive" - and would make the compliance
// feature undemonstrable on a laptop while looking, from the seed script's
// output, exactly like a working estate.
//
// So: unpack what the fake registry will serve, render it with the real helm,
// and evaluate it with the real baseline.
func TestDevChartsRenderAndProduceFindings(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}

	dir := t.TempDir()
	if err := untar(chartTarball("User Plane Function", "25.7.2131"), dir); err != nil {
		t.Fatal(err)
	}
	chartDir := findChart(t, dir)

	helm := render.Helm{}.WithDefaults()
	out, err := helm.Render(context.Background(), chartDir)
	if err != nil {
		t.Fatalf("the dev chart does not render: %v", err)
	}

	resources, errs := compliance.ParseManifests(out.Manifests, compliance.Address{Chart: "upf"})
	if len(errs) > 0 {
		t.Fatalf("the rendered output does not parse: %v", errs)
	}
	if len(resources) < 3 {
		t.Fatalf("want a Deployment, a Service and a ServiceAccount at least, got %d objects", len(resources))
	}

	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	files, err := baseline.Files()
	if err != nil {
		t.Fatal(err)
	}
	packDir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(packDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := (&compliance.Loader{Compiler: comp}).Load(packDir)
	if err != nil {
		t.Fatal(err)
	}

	rel := &compliance.Release{
		Product: "dev", Tag: "25.7.2131", Resources: resources,
		Config: map[string]any{"approvedRegistries": []any{"registry.mavenir.example.com"}},
	}
	results, err := (&compliance.Engine{Catalog: cat}).Run(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}

	counts := compliance.Tally(results)
	if counts.Pass == 0 {
		t.Error("no check passed against the dev chart, which means the chart is not being reached")
	}
	// Deliberate defects, so a development estate teaches what a finding looks
	// like - and so a broken renderer, which also produces zero findings, is
	// distinguishable from a compliant chart.
	if counts.Fail == 0 {
		t.Error("no check failed; the dev charts are supposed to carry deliberate defects")
	}
	if counts.Error > 0 {
		for _, r := range results {
			if r.Outcome == compliance.OutcomeError {
				t.Errorf("%s could not be decided: %s", r.CheckID, r.Error)
			}
		}
	}
	t.Logf("dev chart: %d pass, %d fail (%d blocking, %d warning), %d skip",
		counts.Pass, counts.Fail, counts.Blocking, counts.Warning, counts.Skip)
}

// Every component's chart must render, not just the one above: the defects are
// chosen from the component name, so a template that only breaks for one
// combination would slip through a single-chart test.
func TestEveryDevChartRenders(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	seen := map[string]bool{}
	for _, releases := range catalogue {
		for _, rel := range releases {
			for _, comp := range rel.components {
				if comp.layers != 1 || seen[comp.name] {
					continue
				}
				seen[comp.name] = true

				dir := t.TempDir()
				if err := untar(chartTarball(comp.name, rel.tag), dir); err != nil {
					t.Fatalf("%s: %v", comp.name, err)
				}
				helm := render.Helm{}.WithDefaults()
				if _, err := helm.Render(context.Background(), findChart(t, dir)); err != nil {
					t.Errorf("%s does not render: %v", comp.name, err)
				}
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no chart components in the catalogue")
	}
	t.Logf("%d chart components render", len(seen))
}

func findChart(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(root, e.Name())
		}
	}
	t.Fatal("the tarball has no chart directory")
	return ""
}

func untar(body []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(dir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, dir) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}
