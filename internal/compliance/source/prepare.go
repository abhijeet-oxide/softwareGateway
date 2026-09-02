package source

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
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
	// Classify names what an artifact is, and it is the SAME classifier the
	// artifact listing, the transfer breakdown and the comparison use.
	//
	// Not a media-type comparison of this package's own. A NEAR orb's charts
	// are plain image manifests whose only distinguishing evidence is an
	// annotation the vendor wrote, and an index's children have no config
	// media type recorded at all - so a second opinion here answered "no Helm
	// charts" for a release of ninety-five of them, while the page beside it
	// counted them correctly.
	//
	// Nil means the OCI rules, which is the right fallback for a conformant
	// registry and never a panic.
	Classify func(productName string) vendors.Classifier
	// Config is site configuration a check may consult - the approved registry
	// list, probe bounds. Data, not policy: the rule lives in the check, and
	// the registries this organization runs live in configuration.
	Config func() map[string]any
}

// PackageLookup is what the preparer needs from the store: the release row and
// the artifacts that might hold charts.
type PackageLookup interface {
	GetPackageByID(ctx context.Context, id int64) (store.PackageRow, error)
	ChartCandidates(ctx context.Context, packageID int64) ([]store.ChartCandidate, error)
}

var _ compliance.Source = (*Preparer)(nil)

// ErrNoCharts means the release ships nothing this can check.
//
// Reported as a failure rather than as a clean run. A release with no charts
// may be an image-only delivery, which is a legitimate thing to say - but
// saying it as "compliant, no findings" would put a green badge on a release
// nobody examined.
var ErrNoCharts = errors.New("compliance: this release ships no Helm charts, so there is nothing to check at tier 1")

// ErrNotAnalysed means the release's manifest tree has not been walked.
//
// A different problem from ErrNoCharts with a different fix, so it is a
// different error. A run needs each chart artifact's LAYER digest, which the
// walk records; before it there is nothing to fetch, and reporting that as "no
// Helm charts" would send somebody looking at the vendor's packaging for a
// problem that is on this side.
var ErrNotAnalysed = errors.New(
	"compliance: this release has not been analysed, so its chart artifacts have no content recorded yet - " +
		"analyse the package first")

// noChartsIn says how it reached that conclusion.
//
// # Why the count is in the message
//
// The bare sentence asserted something about the release and gave a reader no
// way to tell whether it was true. It said "no Helm charts" about a release
// whose own page counted ninety-five, and the only way to find out why was to
// read this code. A message that says how many artifacts it looked at turns
// the same failure into a diagnosis: "0 of 250" is a classification problem,
// "0 of 0" is a release that has not been analysed yet.
func noChartsIn(candidates int) error {
	if candidates == 0 {
		return ErrNotAnalysed
	}
	return fmt.Errorf("%w (0 of %d artifacts with content were classified as charts)",
		ErrNoCharts, candidates)
}

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
	candidates, err := p.Packages.ChartCandidates(ctx, req.PackageID)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("listing the release's artifacts: %w", err)
	}
	artifacts, skipped := p.chartsAmong(req.Product, candidates)
	if len(artifacts) == 0 {
		return nil, nil, noop, noChartsIn(len(candidates))
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

	// Whatever the budget refused, and whatever was recognised as a chart but
	// could not be used, is on the run as a chart that was skipped with a
	// reason. A chart nobody checked is not a chart that passed.
	for _, s := range append(skipped, fetched.Skipped...) {
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

// chartsAmong picks the charts out of a release's artifacts.
//
// # Why the classifier decides and not this function
//
// Because there must be exactly one answer to "is this a chart" in this
// product. The artifact listing, the transfer breakdown and the comparison all
// ask vendors.Classifier; a compliance run that asked something else would
// disagree with the page a person was looking at when they pressed the button
// - which is what happened, and what this comment exists to stop happening
// again.
func (p *Preparer) chartsAmong(productName string, candidates []store.ChartCandidate) ([]store.ChartCandidate, []string) {
	classify := vendors.OCIOnly
	if p.Classify != nil {
		if c := p.Classify(productName); c != nil {
			classify = c
		}
	}

	var (
		charts  []store.ChartCandidate
		skipped []string
	)
	for _, c := range candidates {
		if classify(c.MediaType, c.ArtifactType, c.ConfigMediaType, c.Annotations) != oci.KindChart {
			continue
		}
		// A packaged chart is one tarball. Several layers means this is not the
		// shape the unpacker understands, and unpacking the first of them would
		// report on a fraction of what shipped while looking complete.
		if c.LayerCount != 1 {
			skipped = append(skipped, fmt.Sprintf(
				"%s is classified as a chart but has %d layers, so it was not unpacked",
				displayOf(c), c.LayerCount))
			continue
		}
		charts = append(charts, c)
	}
	return charts, skipped
}
