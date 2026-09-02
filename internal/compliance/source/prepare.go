package source

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Preparer turns a release in a registry into a Release the engine can judge.
//
// It is the compliance.Source the Coordinator's runner uses: fetch the chart
// artifacts, unpack them, render each one, probe determinacy, and hand back the
// resources with what could not be produced recorded rather than dropped.
type Preparer struct {
	Fetcher  Fetcher
	Helm     render.Helm
	Probe    bool
	Packages PackageLookup
	// Config is site configuration a check may consult - the approved registry
	// list, probe bounds. Data, not policy: the rule lives in the check, and
	// the registries this organization runs live in configuration.
	Config func() map[string]any
}

// PackageLookup is what the preparer needs from the store: the release row and
// its chart artifacts.
type PackageLookup interface {
	GetPackageByID(ctx context.Context, id int64) (store.PackageRow, error)
	ChartArtifacts(ctx context.Context, packageID int64) ([]store.ChartArtifact, error)
}

var _ compliance.Source = (*Preparer)(nil)

// ErrNoCharts means the release ships nothing this can check.
//
// Reported as a failure rather than as a clean run. A release with no charts
// may be an image-only delivery, which is a legitimate thing to say - but
// saying it as "compliant, no findings" would put a green badge on a release
// nobody examined.
var ErrNoCharts = errors.New("compliance: this release ships no Helm charts, so there is nothing to check at tier 1")

// Prepare acquires, unpacks and renders.
func (p *Preparer) Prepare(
	ctx context.Context, req compliance.Request,
	report func(compliance.Stage, int, int, string),
) (*compliance.Release, compliance.Determiner, func(), error) {
	noop := func() {}

	pkg, err := p.Packages.GetPackageByID(ctx, req.PackageID)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("reading the release: %w", err)
	}
	artifacts, err := p.Packages.ChartArtifacts(ctx, req.PackageID)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("listing the release's charts: %w", err)
	}
	if len(artifacts) == 0 {
		return nil, nil, noop, ErrNoCharts
	}

	report(compliance.StageFetching, 0, len(artifacts), "")
	fetched, err := p.Fetcher.Fetch(ctx, req.Product, pkg, artifacts)
	if err != nil {
		return nil, nil, noop, err
	}
	cleanup := func() {
		if fetched.Root != "" {
			_ = os.RemoveAll(fetched.Root)
		}
	}
	report(compliance.StageFetching, len(fetched.Charts), len(artifacts), "")

	helm := p.Helm.WithDefaults()
	version, helmErr := helm.Version(ctx)
	available := helmErr == nil

	rel := &compliance.Release{
		Product:       req.Product,
		Tag:           req.Release,
		PackageDigest: req.Digest,
		HelmVersion:   version,
		KubeVersion:   helm.KubeVersion,
		Config:        p.config(),
	}
	// Probe: false here on purpose. The loader's own probe is per chart; the
	// determinacy answer has to be release-wide because a check may compare a
	// resource in one chart against one in another, so the second render is
	// driven below and merged once.
	loader := render.Loader{Helm: helm, Probe: false, HelmAvailable: available, HelmVersion: version}

	var baseline, perturbed []compliance.Resource
	probeUsable := p.Probe && available

	for i, c := range fetched.Charts {
		if err := ctx.Err(); err != nil {
			return nil, nil, cleanup, err
		}
		report(compliance.StageRendering, i, len(fetched.Charts), c.Ref)

		base := compliance.Address{
			Product: req.Product, Release: req.Release, PackageDigest: req.Digest,
			ArtifactDigest: c.Digest, ArtifactRef: c.Ref,
		}

		// A chart that could not be fetched is a chart with no resources and a
		// recorded reason. It is NOT omitted: the runner turns a chart in this
		// state into an `error` result for every check, and an omitted chart
		// would instead make every check applicable to nothing - which reads as
		// a pass.
		if c.Err != nil {
			rel.Charts = append(rel.Charts, &compliance.Chart{
				Name:         chartNameFor(c),
				RenderStatus: compliance.RenderFailed,
				RenderError:  c.Err.Error(),
			})
			continue
		}

		chartRel, _, err := loader.Load(ctx, c.Dir, base)
		if err != nil {
			rel.Charts = append(rel.Charts, &compliance.Chart{
				Name:         chartNameFor(c),
				RenderStatus: compliance.RenderFailed,
				RenderError:  err.Error(),
			})
			continue
		}
		for _, ch := range chartRel.Charts {
			ch.Digest = c.Digest
			ch.Ref = c.Ref
			rel.Charts = append(rel.Charts, ch)
		}
		rel.Resources = append(rel.Resources, chartRel.Resources...)
		baseline = append(baseline, chartRel.Resources...)

		if probeUsable {
			values, verr := render.ReadValues(c.Dir)
			if verr != nil {
				probeUsable = false
				continue
			}
			manifests, ok := render.ProbeRender(ctx, helm, c.Dir, values)
			if !ok {
				// One chart without a second render costs determinacy for the
				// release rather than producing a wrong answer for that chart.
				// Reporting `fixed` where nothing was measured would invent
				// vendor defects.
				probeUsable = false
				continue
			}
			alt, _ := compliance.ParseManifests(manifests, base)
			perturbed = append(perturbed, alt...)
		}
	}
	report(compliance.StageRendering, len(fetched.Charts), len(fetched.Charts), "")

	// Whatever the budget refused is on the run, as a chart that was skipped
	// with a reason. A chart nobody checked is not a chart that passed.
	for _, s := range fetched.Skipped {
		rel.Charts = append(rel.Charts, &compliance.Chart{
			Name: "(not fetched)", RenderStatus: compliance.RenderSkipped, RenderError: s,
		})
	}

	determiner := compliance.Determiner(render.Unusable())
	if probeUsable {
		determiner = render.NewProbe(baseline, perturbed, true)
	}
	return rel, determiner, cleanup, nil
}

func (p *Preparer) config() map[string]any {
	if p.Config == nil {
		return map[string]any{}
	}
	if c := p.Config(); c != nil {
		return c
	}
	return map[string]any{}
}

// chartNameFor names a chart that could not be read, so the run says which one.
func chartNameFor(c Chart) string {
	if c.Ref != "" {
		return c.Ref
	}
	return compliance.ShortDigest(c.Digest)
}
