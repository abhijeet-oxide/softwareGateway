package render

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// Turning a directory of delivered artifacts into a Release the engine can
// judge.
//
// # Why a directory and not a registry reference here
//
// This package's job ends at "bytes on disk become addressed resources". Where
// the bytes came from - an unpacked chart artifact read by digest from the
// source registry, or a path an engineer passed to the CLI - is the caller's,
// and keeping it out means the whole rendering path is testable with a
// directory and no registry at all.

// Loader turns a directory into a release.
type Loader struct {
	Helm Helm
	// Probe controls whether the second render happens. Off, every finding
	// carries determinacy `unknown` - honest, and much less useful.
	Probe bool
	// HelmAvailable is set from a start-up probe. When false, charts are not
	// rendered and each one produces an error rather than an absence.
	HelmAvailable bool
	// HelmVersion is recorded on the release so a finding can say what
	// produced it.
	HelmVersion string

	// Evidence bounds how much rendered text is kept so a finding can be shown
	// against the manifest it came from. Zero uses the defaults; negative
	// disables keeping any.
	Evidence EvidenceBudget
	// Keeper carries that budget ACROSS several Loads, for a caller that loads
	// a release one chart directory at a time. Nil means this Load gets its own,
	// which is right when the directory is the whole release.
	Keeper *EvidenceKeeper
}

// EvidenceBudget bounds the rendered text a run keeps.
//
// # Why keeping it needs a budget at all
//
// The manifests are what makes a finding checkable rather than assertable, and
// they are also the one part of a run whose size is set by the vendor. A chart
// that renders four hundred ConfigMaps of embedded certificates produces
// megabytes of YAML per release, and every release keeps its own copy.
//
// Two numbers rather than one, for the reason the fetch budgets take two: one
// pathological chart and a release of four hundred ordinary ones are different
// problems, and without the per-document cap the pathological one spends the
// whole budget and every chart after it loses its evidence for an unrelated
// reason.
//
// Over the cap, a document is kept TRUNCATED and says so, rather than dropped.
// The first few thousand lines of a chart are still the answer for most of its
// findings, and a finding past the cut is told that the evidence stops there
// instead of being shown the wrong lines.
type EvidenceBudget struct {
	PerDocument int64
	PerRelease  int64
}

// Defaults sized against what charts actually render to: a large one is a few
// hundred kilobytes, and a release of a hundred fits inside the total with room
// to spare.
const (
	DefaultEvidencePerDocument = 4 << 20
	DefaultEvidencePerRelease  = 24 << 20
)

func (b EvidenceBudget) perDocument() int64 {
	if b.PerDocument == 0 {
		return DefaultEvidencePerDocument
	}
	return b.PerDocument
}

func (b EvidenceBudget) perRelease() int64 {
	if b.PerRelease == 0 {
		return DefaultEvidencePerRelease
	}
	return b.PerRelease
}

// off reports that this deployment does not want evidence kept at all.
func (b EvidenceBudget) off() bool { return b.PerDocument < 0 || b.PerRelease < 0 }

// Load walks a directory, renders every chart it finds, and returns the
// release plus a determinacy probe over it.
//
// # What a failure produces
//
// Not an error return. A chart that will not render becomes a chart with
// RenderStatus failed and its error recorded, and the caller turns that into
// `error` results for the checks that needed it. One unrenderable chart in a
// ninety-seven chart release must not lose the other ninety-six, and it must
// not be silently dropped either.
func (l Loader) Load(ctx context.Context, dir string, base compliance.Address) (*compliance.Release, *Probe, error) {
	dirs, err := discoverCharts(dir)
	if err != nil {
		return nil, nil, err
	}
	plain, err := discoverPlainManifests(dir, dirs)
	if err != nil {
		return nil, nil, err
	}
	if len(dirs) == 0 && len(plain) == 0 {
		return nil, nil, fmt.Errorf("no Helm charts and no Kubernetes manifests under %s", dir)
	}

	rel := &compliance.Release{
		Product:       base.Product,
		Tag:           base.Release,
		PackageDigest: base.PackageDigest,
		HelmVersion:   l.HelmVersion,
		KubeVersion:   l.Helm.WithDefaults().KubeVersion,
	}
	var baseline, perturbed []compliance.Resource
	probeUsable := l.Probe && l.HelmAvailable

	keeper := l.Keeper
	if keeper == nil {
		keeper = NewEvidenceKeeper(l.Evidence)
	}

	for _, cd := range dirs {
		chart, manifests, resources, alt, ok := l.loadChart(ctx, cd, base)
		rel.Charts = append(rel.Charts, chart)
		// The stream, not the objects: a result's RenderedLine is an offset
		// into this text, so slicing it per object would make every one of
		// those offsets wrong.
		keeper.Keep(&rel.Rendered, compliance.RenderedDoc{
			Chart: chart.Name, ChartVersion: chart.Version,
		}, manifests)
		for i := range resources {
			resources[i].Chart = chart
		}
		rel.Resources = append(rel.Resources, resources...)
		baseline = append(baseline, resources...)
		if l.Probe && ok {
			perturbed = append(perturbed, alt...)
		} else if l.Probe {
			// One chart without a second render makes determinacy unknown for
			// that chart, not for the release.
			probeUsable = probeUsable && len(dirs) == 0
		}
	}

	for _, f := range plain {
		body, err := os.ReadFile(f) //nolint:gosec // path from the artifact under inspection
		if err != nil {
			continue
		}
		addr := base
		addr.SourceFile = relative(dir, f)
		// A manifest shipped as-is was never rendered, and the file IS the
		// evidence - kept beside the charts so a finding against one is shown
		// the same way as a finding against the other.
		keeper.Keep(&rel.Rendered, compliance.RenderedDoc{SourceFile: addr.SourceFile}, body)
		resources, _ := compliance.ParseManifests(body, addr)
		rel.Resources = append(rel.Resources, resources...)
		baseline = append(baseline, resources...)
		// A plain manifest has no values, so nothing about it is overridable.
		perturbed = append(perturbed, resources...)
	}

	probe := Unusable()
	if probeUsable {
		probe = NewProbe(baseline, perturbed, true)
	}
	return rel, probe, nil
}

// loadChart renders one chart, or records why it could not be.
//
// Returns the rendered stream as well as the objects parsed out of it: it is
// the evidence a finding is shown against, and it is the unit the line numbers
// on those objects are counted in.
func (l Loader) loadChart(ctx context.Context, chartDir string, base compliance.Address) (
	*compliance.Chart, []byte, []compliance.Resource, []compliance.Resource, bool,
) {
	meta, metaErr := ReadChartMeta(chartDir)
	chart := &compliance.Chart{
		Name:         meta.Name,
		Version:      meta.Version,
		AppVersion:   meta.AppVersion,
		SubchartPath: subchartPath(chartDir),
		RenderStatus: compliance.RenderOK,
	}
	if chart.Name == "" {
		chart.Name = filepath.Base(chartDir)
	}
	if metaErr != nil {
		chart.RenderStatus = compliance.RenderFailed
		chart.RenderError = metaErr.Error()
		return chart, nil, nil, nil, false
	}

	values, valErr := ReadValues(chartDir)
	if valErr != nil {
		chart.RenderStatus = compliance.RenderFailed
		chart.RenderError = valErr.Error()
		return chart, nil, nil, nil, false
	}
	chart.Values = values

	if !l.HelmAvailable {
		chart.RenderStatus = compliance.RenderSkipped
		chart.RenderError = ErrHelmUnavailable.Error()
		return chart, nil, nil, nil, false
	}

	out, err := l.Helm.Render(ctx, chartDir)
	if err != nil {
		chart.RenderStatus = compliance.RenderFailed
		chart.RenderError = err.Error()
		return chart, nil, nil, nil, false
	}

	addr := base
	addr.Chart = chart.Name
	addr.ChartVersion = chart.Version
	addr.SubchartPath = chart.SubchartPath
	resources, parseErrs := compliance.ParseManifests(out.Manifests, addr)
	if len(parseErrs) > 0 {
		// The chart rendered; some of what it produced is not readable. Both
		// facts are kept: the readable objects are still checked, and the
		// unreadable ones are named.
		chart.RenderError = errors.Join(parseErrs...).Error()
	}

	var alt []compliance.Resource
	ok := false
	if l.Probe {
		if manifests, rendered := ProbeRender(ctx, l.Helm, chartDir, values); rendered {
			alt, _ = compliance.ParseManifests(manifests, addr)
			ok = true
		}
	}
	return chart, out.Manifests, resources, alt, ok
}

// discoverCharts finds chart directories, without descending into their
// subcharts.
//
// # Why subcharts are not rendered separately
//
// `helm template` on a parent renders its subcharts, with the parent's values
// applied - which is what will actually be installed. Rendering a subchart
// again on its own would produce a second copy of every object, under the
// subchart's own defaults, and report findings against a configuration nobody
// deploys.
func discoverCharts(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if IsChart(path) {
			out = append(out, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s for charts: %w", root, err)
	}
	sort.Strings(out)
	return out, nil
}

// discoverPlainManifests finds YAML outside any chart.
//
// A release ships plain manifests as well as charts - a namespace, an operator
// subscription, a CRD bundle - and they are as much part of what gets installed
// as anything a chart renders.
func discoverPlainManifests(root string, chartDirs []string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			for _, cd := range chartDirs {
				if path == cd {
					return filepath.SkipDir
				}
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml":
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s for manifests: %w", root, err)
	}
	sort.Strings(out)
	return out, nil
}

// subchartPath is the dependency path when a chart sits under another's
// charts/ directory. Without it a vendor looks for the template in the parent
// chart and does not find it.
func subchartPath(chartDir string) string {
	parts := strings.Split(filepath.ToSlash(chartDir), "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "charts" {
			return strings.Join(parts[i:], "/")
		}
	}
	return ""
}

func relative(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(path)
}
