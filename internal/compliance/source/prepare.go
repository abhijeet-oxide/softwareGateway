package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"

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
	Fetcher Fetcher
	Helm    render.Helm
	Probe   bool
	// Evidence bounds the rendered manifests a run keeps so a finding can be
	// SHOWN against the text it came from. Held here rather than on the loader
	// because the budget is per RELEASE and a release is loaded one chart
	// directory at a time: a budget started per directory is not a budget.
	Evidence render.EvidenceBudget
	// RenderConcurrency is how many charts are rendered at once. Zero picks a
	// default from the machine; one renders them in sequence, which is what a
	// Coordinator sharing a small node with everything else may want.
	RenderConcurrency int
	// RenderCache reuses a chart's rendered output across runs and across
	// releases. Nil renders everything, which is a working configuration and
	// what every test uses.
	RenderCache compliance.RenderStore
	// Log records degradations that do not change a run's answer - a render
	// cache that could not be read or written. Optional.
	Log      *slog.Logger
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
	ctx context.Context, req compliance.Request, rep compliance.Reporter,
) (*compliance.Release, compliance.Determiner, func(), error) {
	noop := func() {}
	if rep == nil {
		rep = compliance.NopReporter{}
	}

	rep.Stage(compliance.StageResolving, 0, 0, "")
	pkg, err := p.Packages.GetPackageByID(ctx, req.PackageID)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("reading the release: %w", err)
	}
	candidates, err := p.Packages.ChartCandidates(ctx, req.PackageID)
	if err != nil {
		return nil, nil, noop, fmt.Errorf("listing the release's artifacts: %w", err)
	}
	rep.Stage(compliance.StageResolving, len(candidates), len(candidates), "")
	artifacts, skipped := p.chartsAmong(req.Product, candidates)
	rep.Count(func(c *compliance.ProgressCounts) { c.ChartsFound = len(artifacts) })
	rep.Event(compliance.EventInfo, "Classified %d of %d recorded artifacts as Helm charts",
		len(artifacts), len(candidates))
	for _, sk := range skipped {
		rep.Event(compliance.EventWarn, "%s", sk)
	}
	if len(artifacts) == 0 {
		return nil, nil, noop, noChartsIn(len(candidates))
	}

	// THE RENDERER'S IDENTITY, before anything is fetched.
	//
	// helm's version is part of the render cache's key, so it has to be known
	// before the cache can be consulted - and the cache has to be consulted
	// before the fetch, because a hit means the chart's bytes are never needed
	// at all.
	helm := p.Helm.WithDefaults()
	version, helmErr := helm.Version(ctx)
	available := helmErr == nil
	if !available {
		rep.Event(compliance.EventWarn,
			"helm is unavailable: no chart can be rendered, and every check requiring a "+
				"rendered chart will report as unchecked")
	}

	cache := compliance.NewRenderCache(p.RenderCache, compliance.RenderInputs{
		HelmVersion: version, KubeVersion: helm.KubeVersion,
		APIVersions: helm.APIVersions,
		ReleaseName: helm.ReleaseName, Namespace: helm.Namespace,
	})

	/*
	 * WHAT ALREADY EXISTS, AND THEREFORE WHAT HAS TO BE FETCHED.
	 *
	 * The cache is keyed by each chart's LAYER DIGEST, which the release's own
	 * record carries - so this question is answerable before a single byte is
	 * pulled from the vendor's registry. A chart whose baseline and probe are
	 * both held is skipped end to end: no request, no unpack, no subprocess.
	 *
	 * Both variants or neither. Rendering only the missing half would work and
	 * would save one subprocess of the two; it is not worth the second code
	 * path, because by the time either is missing the chart has been fetched
	 * and the fetch was the expensive part.
	 */
	type slot struct {
		candidate store.ChartCandidate
		base      compliance.CachedRender
		probe     compliance.CachedRender
		reused    bool
	}
	slots := make([]slot, len(artifacts))
	var (
		needed  []store.ChartCandidate
		needAt  []int
		reusing int
	)

	found, lookupErr := cache.Lookup(ctx, digestsOf(artifacts))
	if lookupErr != nil {
		// A cache that cannot be read is a cache that is not used. Failing the
		// run over it would turn a slow answer into no answer.
		p.warn(rep, "The render cache could not be read: %v", lookupErr)
		found = nil
	}
	for i, ca := range artifacts {
		slots[i].candidate = ca
		if available {
			base, okBase := cache.Get(found, ca.LayerDigest, compliance.VariantBase)
			probe, okProbe := cache.Get(found, ca.LayerDigest, compliance.VariantProbe)
			if okBase && (!p.Probe || okProbe) {
				slots[i].base, slots[i].probe, slots[i].reused = base, probe, true
				reusing++
				continue
			}
		}
		needed = append(needed, ca)
		needAt = append(needAt, i)
	}
	cache.Hit(reusing)
	cache.Miss(len(needed))
	rep.Count(func(c *compliance.ProgressCounts) { c.ChartsReused = reusing })
	if reusing > 0 {
		rep.Event(compliance.EventOK,
			"Reused %d of %d chart renders from the render cache; %d charts require download",
			reusing, len(artifacts), len(needed))
	}

	rep.Stage(compliance.StageFetching, 0, len(needed), "")
	fetched, err := p.Fetcher.Fetch(ctx, req.Product, pkg, needed, rep)
	if err != nil {
		return nil, nil, noop, err
	}
	cleanup := func() {
		if fetched.Root != "" {
			_ = os.RemoveAll(fetched.Root)
		}
	}
	rep.Stage(compliance.StageFetching, len(fetched.Charts), len(needed), "")

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
	//
	// The evidence budget passed here is the PER-DOCUMENT half only. The
	// release-wide half is applied when the results are merged, in chart order,
	// because that is the only way it can be deterministic: charts render
	// concurrently and finish in whatever order the machine decides, so a
	// budget consumed as they land would truncate a different chart on every
	// run - and a report that differs between two runs of the same bytes is
	// exactly what rule 5 forbids.
	loader := render.Loader{
		Helm: helm, Probe: false, HelmAvailable: available, HelmVersion: version,
		Evidence: render.EvidenceBudget{PerDocument: p.Evidence.PerDocument, PerRelease: -1},
	}
	if p.Evidence.PerDocument >= 0 && p.Evidence.PerRelease != -1 {
		loader.Evidence.PerRelease = 0 // the per-Load default; the run-wide cap is below
	}

	workers := renderWorkers(p.RenderConcurrency)
	rep.Stage(compliance.StageRendering, 0, len(fetched.Charts), "")
	rep.Concurrency(workers)

	/*
	 * RENDERED CONCURRENTLY, ASSEMBLED IN ORDER.
	 *
	 * Rendering is a helm subprocess per chart, twice per chart when the
	 * determinacy probe is on. For a ninety-five chart orb that is a hundred
	 * and ninety processes run one after another, and it was the reason a check
	 * of a real release took the better part of ten minutes with a Coordinator
	 * that was idle for most of it.
	 *
	 * The results are written into a slice by INDEX and merged afterwards in
	 * that order. Nothing about the report may depend on which worker finished
	 * first: the seq of a result, which chart the evidence budget runs out on,
	 * the order of the coverage table - all of it has to be the same on the
	 * second run of the same bytes, or none of it is reproducible.
	 */
	out := make([]rendered, len(artifacts))
	produced := make([][]compliance.CachedRender, len(artifacts))

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
	)
	for n := range fetched.Charts {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		go func(n int, c Chart) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			i := needAt[n]

			name := shortChartName(c)
			rep.Begin(name)
			defer func() {
				rep.End(name)
				rep.Advance(1)
			}()

			base := compliance.Address{
				Product: req.Product, Release: req.Release, PackageDigest: req.Digest,
				ArtifactDigest: c.Digest, ArtifactRef: c.Ref,
			}

			// A chart that could not be fetched is a chart with no resources
			// and a recorded reason. It is NOT omitted: the runner turns a
			// chart in this state into an `error` result for every check, and
			// an omitted chart would instead make every check applicable to
			// nothing - which reads as a pass.
			if c.Err != nil {
				out[i].charts = []*compliance.Chart{{
					Name:         chartNameFor(c),
					RenderStatus: compliance.RenderFailed, RenderError: c.Err.Error(),
				}}
				out[i].failed = c.Err.Error()
				rep.Event(compliance.EventFail, "Download failed for %s: %v", name, c.Err)
				rep.Count(func(k *compliance.ProgressCounts) { k.ChartsFailed++ })
				return
			}

			chartRel, attempts, err := loadWithRetry(ctx, loader, c.Dir, base, rep, name)
			if err != nil {
				kind := render.ClassifyFailure(err)
				out[i].charts = []*compliance.Chart{{
					Name:            chartNameFor(c),
					RenderStatus:    compliance.RenderFailed,
					RenderError:     err.Error(),
					RenderErrorKind: string(kind),
					RenderAttempts:  attempts,
				}}
				out[i].failed = err.Error()
				rep.Event(compliance.EventFail, "%s", renderFailureLine(name, out[i].charts))
				rep.Count(func(k *compliance.ProgressCounts) { k.ChartsFailed++ })
				return
			}
			for _, ch := range chartRel.Charts {
				ch.Digest = c.Digest
				ch.Ref = c.Ref
			}
			out[i].charts = chartRel.Charts
			out[i].res = chartRel.Resources
			out[i].docs = chartRel.Rendered

			bad := 0
			// The charts that actually failed, in order. Named rather than
			// counted: an umbrella whose fourth subchart is broken used to be
			// reported as Charts[0] - the umbrella, which rendered perfectly -
			// so the log gave a classification belonging to a chart that had no
			// failure and no cause at all.
			var failed []*compliance.Chart
			for _, ch := range chartRel.Charts {
				if ch.RenderStatus == compliance.RenderOK {
					continue
				}
				bad++
				failed = append(failed, ch)
				ch.RenderAttempts = attempts
				if ch.RenderErrorKind == "" && ch.RenderError != "" {
					ch.RenderErrorKind = string(render.ClassifyFailure(errors.New(ch.RenderError)))
				}
			}
			if bad > 0 {
				rep.Count(func(k *compliance.ProgressCounts) { k.ChartsFailed += bad })
				rep.Event(compliance.EventFail, "%s", renderFailureLine(name, failed))
			} else {
				rep.Count(func(k *compliance.ProgressCounts) { k.ChartsRendered++ })
				rep.Event(compliance.EventOK, "Rendered %s: %d objects", name, len(chartRel.Resources))
			}

			// What this render produced, for the next run. Recorded only when
			// the chart rendered cleanly and against the LAYER digest, which is
			// what content-addresses the bytes that produced it.
			values := render.ReadValuesFile(c.Dir)
			if bad == 0 && len(chartRel.Rendered) == 1 {
				meta := chartRel.Charts[0]
				produced[i] = append(produced[i], compliance.CachedRender{
					ChartDigest: slots[i].candidate.LayerDigest, Variant: compliance.VariantBase,
					ChartName: meta.Name, ChartVersion: meta.Version, AppVersion: meta.AppVersion,
					SubchartPath: meta.SubchartPath, ValuesYAML: values,
					Manifests: chartRel.Rendered[0].Content,
				})
			}

			if p.Probe && available {
				vals, verr := render.ReadValues(c.Dir)
				if verr != nil {
					return
				}
				manifests, ok := render.ProbeRender(ctx, helm, c.Dir, vals)
				if !ok {
					return
				}
				out[i].alt, _ = compliance.ParseManifests(manifests, base)
				out[i].probed = true
				if bad == 0 {
					produced[i] = append(produced[i], compliance.CachedRender{
						ChartDigest: slots[i].candidate.LayerDigest, Variant: compliance.VariantProbe,
						ValuesYAML: values, Manifests: manifests,
					})
				}
			}
		}(n, fetched.Charts[n])
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, cleanup, err
	}

	// The reused charts, parsed out of the cache in the release's order. Parsing
	// is the same code path a fresh render takes, so a hit and a miss produce
	// identical resources from identical bytes - which is the property the whole
	// cache rests on.
	for i := range slots {
		if !slots[i].reused {
			continue
		}
		ca := slots[i].candidate
		addr := compliance.Address{
			Product: req.Product, Release: req.Release, PackageDigest: req.Digest,
			ArtifactDigest: ca.Digest, ArtifactRef: ca.Ref,
		}
		out[i] = p.fromCache(addr, ca, slots[i].base, slots[i].probe)
	}

	// Save what this run produced, before the merge so a cancelled merge does
	// not lose renders that are already correct. A failure here is logged and
	// never returned: the renders are in hand and the run is about to judge
	// them, so failing over the CACHE would turn a slow next run into no
	// result at all.
	var toSave []compliance.CachedRender
	for i := range produced {
		toSave = append(toSave, produced[i]...)
	}
	if err := cache.Save(context.WithoutCancel(ctx), toSave); err != nil {
		p.warn(rep, "The render cache could not be written: %v", err)
	}

	// THE MERGE, in chart order. See the comment above for why this is not done
	// in the workers.
	keeper := render.NewEvidenceKeeper(p.Evidence)
	/*
	 * DETERMINACY IS PER CHART, and that is a correction rather than a
	 * loosening.
	 *
	 * It used to be per release: one chart whose second render did not happen
	 * set `probeUsable = false` and the whole run answered `unknown` to every
	 * question. On a ninety-five chart orb that is what happened every time -
	 * one chart out of ninety-five is enough - so every finding in the report
	 * read "Ownership not established", the split between the vendor's defect
	 * and the site's decision was on no screen anywhere, and the most useful
	 * column in the table was dead weight.
	 *
	 * The blunt version existed for a real reason: an object present in the
	 * baseline index and absent from the perturbed one reads as "its existence
	 * depends on values", so feeding one render of a chart into the probe
	 * without the other would report every field of it as `configurable` -
	 * inventing an excuse for a real defect. That is avoided by pairing, not by
	 * discarding: a chart contributes to BOTH indexes or to NEITHER. A chart
	 * that was not probed is absent from the baseline index, and
	 * Probe.Determinacy already answers `unknown` for a key it does not hold.
	 *
	 * So the guarantee is unchanged - nothing is ever answered from a single
	 * render - and it now costs one chart's findings rather than the release's.
	 */
	var baseline, perturbed []compliance.Resource
	probeOn := p.Probe && available
	probedCharts, renderedCharts := 0, 0
	for i := range out {
		rel.Charts = append(rel.Charts, out[i].charts...)
		rel.Resources = append(rel.Resources, out[i].res...)
		for _, d := range out[i].docs {
			keeper.Keep(&rel.Rendered, compliance.RenderedDoc{
				Chart: d.Chart, ChartVersion: d.ChartVersion, SourceFile: d.SourceFile,
				Truncated: d.Truncated,
			}, d.Content)
		}
		if out[i].failed != "" {
			continue
		}
		renderedCharts++
		if !probeOn || !out[i].probed {
			continue
		}
		probedCharts++
		baseline = append(baseline, out[i].res...)
		perturbed = append(perturbed, out[i].alt...)
	}
	// Said on the run, because a report whose ownership column is empty for
	// forty of ninety-five charts is a report a reader has to be told about
	// rather than left to infer.
	if probeOn && probedCharts < renderedCharts {
		p.warn(rep, "Ownership was established for %d of %d rendered charts; the rest "+
			"could not be rendered a second time, so their findings report ownership "+
			"as not established", probedCharts, renderedCharts)
	}

	rep.Stage(compliance.StageRendering, len(fetched.Charts), len(fetched.Charts), "")
	rep.Concurrency(0)

	// Whatever the budget refused, and whatever was recognised as a chart but
	// could not be used, is on the run as a chart that was skipped with a
	// reason. A chart nobody checked is not a chart that passed.
	for _, sk := range append(skipped, fetched.Skipped...) {
		rel.Charts = append(rel.Charts, &compliance.Chart{
			Name: "(not fetched)", RenderStatus: compliance.RenderSkipped, RenderError: sk,
		})
	}

	determiner := compliance.Determiner(render.Unusable())
	if probeOn && probedCharts > 0 {
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

// shortChartName is the name for a progress line.
//
// The full reference - `orbs/cfx-5000-k8s/cfx-sepp:orb_24.7.3099` - is the
// right thing on a coverage row, where there is room for it and time to read
// it. In a log that gains a line a second the repository and the tag are the
// same on every line, so they are eighty characters of noise around the one
// word that differs.
func shortChartName(c Chart) string {
	name := chartNameFor(c)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndexByte(name, ':'); i > 0 {
		name = name[:i]
	}
	return name
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

// renderWorkers decides how many helm subprocesses run at once.
//
// # Why this is bounded by CPUs and not by charts
//
// `helm template` is CPU-bound - template execution and YAML marshalling, no
// waiting on anything - so more workers than cores makes a run slower, not
// faster, and every one of them holds a chart's rendered output in memory.
// Four is the floor because even a single-core Coordinator gains from
// overlapping a render with the next chart's file reads, and eight is the
// ceiling because past it the gain is inside the noise and the memory is not.
func renderWorkers(configured int) int {
	if configured > 0 {
		return configured
	}
	n := runtime.NumCPU()
	if n < 4 {
		return 4
	}
	if n > 8 {
		return 8
	}
	return n
}

// renderFailureLine is what the run log says about a chart that did not render.
//
// # Why this is not "name: Template error"
//
// That is what it used to be, and it is what an operator saw thirteen times in
// a row while a run was going: a classification, and nothing to act on. The
// cause was captured - helm's stderr is stored on the chart and the coverage
// table shows it - but the log is the screen that is up WHILE the run is still
// running, and it was the one place the cause was missing.
//
// So the line carries, in the order somebody reads them: which chart failed,
// what kind of failure it was, and what helm actually said. The subchart is
// named only when it differs from the artifact, because "cfx: cfx: ..." is
// noise on the common case of a single-chart artifact.
//
// When several charts under one artifact failed, the first is given in full and
// the rest are counted. A log line per subchart of a broken umbrella would push
// the other artifacts out of a bounded transcript, which is how the one failure
// somebody needed disappears.
func renderFailureLine(name string, failed []*compliance.Chart) string {
	if len(failed) == 0 {
		return name + ": render failed"
	}
	first := failed[0]
	var b strings.Builder
	b.WriteString(name)
	// The subchart, when the failure is in one. Keyed on SubchartPath rather
	// than on the names differing: a chart that could not be loaded at all is
	// recorded under its artifact REFERENCE, and printing that beside the short
	// name it was already logged under is the noise this line exists to remove.
	if first.SubchartPath != "" {
		b.WriteString(" (")
		b.WriteString(first.SubchartPath)
		b.WriteString(")")
	}
	b.WriteString(": ")
	b.WriteString(render.FailureKind(first.RenderErrorKind).Label())
	if cause := render.Cause(first.RenderError); cause != "" {
		b.WriteString(" - ")
		b.WriteString(cause)
	}
	if where := render.FailingTemplate(errors.New(first.RenderError)); where != "" {
		b.WriteString(" (")
		b.WriteString(where)
		b.WriteString(")")
	}
	if n := len(failed) - 1; n > 0 {
		b.WriteString(fmt.Sprintf(" [and %d more chart(s) under this artifact]", n))
	}
	return b.String()
}

// rendered is one chart's contribution, however it was produced - by a helm
// subprocess in this run, or out of the render cache. The two paths converge
// here on purpose: everything downstream reads this struct and cannot tell
// which produced it, which is what makes the cache safe to reason about.
type rendered struct {
	charts []*compliance.Chart
	res    []compliance.Resource
	docs   []compliance.RenderedDoc
	alt    []compliance.Resource
	probed bool
	failed string
}

// fromCache rebuilds a chart's contribution from stored renders.
//
// The manifests go through ParseManifests, exactly as a fresh render's do, so
// the resources and their line numbers are identical to what the miss path
// would have produced from the same bytes. Nothing here re-derives; it only
// re-reads.
func (p *Preparer) fromCache(
	addr compliance.Address, ca store.ChartCandidate, base, probe compliance.CachedRender,
) rendered {
	chart := &compliance.Chart{
		Name:         base.ChartName,
		Version:      base.ChartVersion,
		AppVersion:   base.AppVersion,
		SubchartPath: base.SubchartPath,
		Digest:       ca.Digest,
		Ref:          ca.Ref,
		RenderStatus: compliance.RenderOK,
	}
	if chart.Name == "" {
		chart.Name = chartNameFor(Chart{Digest: ca.Digest, Ref: ca.Ref})
	}
	if vals, err := render.ParseValues(base.ValuesYAML); err == nil {
		chart.Values = vals
	}

	full := addr
	full.Chart = chart.Name
	full.ChartVersion = chart.Version
	full.SubchartPath = chart.SubchartPath

	out := rendered{charts: []*compliance.Chart{chart}}
	out.res, _ = compliance.ParseManifests(base.Manifests, full)
	for i := range out.res {
		out.res[i].Chart = chart
	}
	out.docs = []compliance.RenderedDoc{{
		Chart: chart.Name, ChartVersion: chart.Version, Content: base.Manifests,
	}}
	if len(probe.Manifests) > 0 {
		out.alt, _ = compliance.ParseManifests(probe.Manifests, full)
		out.probed = true
	}
	return out
}

// digestsOf is the cache's lookup list: one layer digest per chart artifact.
func digestsOf(candidates []store.ChartCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.LayerDigest != "" {
			out = append(out, c.LayerDigest)
		}
	}
	return out
}

// warn records a degradation that does not change the run's answer.
//
// Both current callers are the render cache, which is derived data: a cache
// that cannot be read means renders happen, and one that cannot be written
// means the next run is slower. Neither is a reason to fail a check somebody is
// waiting for, and both are reasons to say something.
func (p *Preparer) warn(rep compliance.Reporter, format string, args ...any) {
	rep.Event(compliance.EventWarn, format, args...)
	if p.Log != nil {
		p.Log.Warn(fmt.Sprintf(format, args...))
	}
}

// loadWithRetry renders one chart, retrying only a failure a retry could fix.
//
// # Why the classification decides this and not a counter
//
// `helm template` is a pure function of the chart and the flags. A template
// that dereferenced a nil dereferences it again; a chart that requires
// `global.registry` still requires it. Retrying those is not resilience, it is
// three times the work for the same message and three times the wait for the
// person watching a ninety-five chart run.
//
// What CAN succeed on a second attempt is a render that never reached the
// chart's templates: killed by a deadline on a loaded Coordinator, refused a
// file descriptor, a helm binary replaced under us mid-run. Those are what is
// retried, and the attempt count is recorded either way so the coverage table
// can say "retried and failed again" rather than implying it was tried once.
func loadWithRetry(
	ctx context.Context, loader render.Loader, dir string,
	base compliance.Address, rep compliance.Reporter, name string,
) (*compliance.Release, int, error) {
	var lastErr error
	for attempt := 1; attempt <= render.MaxRenderAttempts; attempt++ {
		rel, _, err := loader.Load(ctx, dir, base)
		if err == nil {
			if attempt > 1 {
				rep.Event(compliance.EventOK, "%s rendered on attempt %d", name, attempt)
			}
			return rel, attempt, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, attempt, err
		}
		kind := render.ClassifyFailure(err)
		if !kind.Retryable() || attempt == render.MaxRenderAttempts {
			return nil, attempt, err
		}
		rep.Event(compliance.EventWarn, "%s: %s on attempt %d, retrying",
			name, kind.Label(), attempt)
	}
	return nil, render.MaxRenderAttempts, lastErr
}
