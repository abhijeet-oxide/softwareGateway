package compliance

import (
	"context"
	"errors"
	"time"
)

// A run: the whole of what one evaluation of one release produces.

// Run is the record of evaluating a release.
type Run struct {
	// What was judged.
	Product       string    `json:"product"`
	Release       string    `json:"release,omitempty"`
	PackageDigest string    `json:"packageDigest,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`

	// What produced it. Rule 5: reproducible, or it is an opinion. Every field
	// here is something that, if it changed, could change a result - so a
	// report that cannot state them cannot be re-derived a year later.
	BundleDigest string `json:"bundleDigest,omitempty"`
	HelmVersion  string `json:"helmVersion,omitempty"`
	KubeVersion  string `json:"kubeVersion,omitempty"`
	Checks       int    `json:"checks"`

	// What it found.
	Verdict Verdict  `json:"verdict"`
	Counts  Counts   `json:"counts"`
	Results []Result `json:"results,omitempty"`

	// Charts records what rendered and what did not, so a reader can see the
	// denominator of the whole run rather than only its findings.
	Charts []ChartStatus `json:"charts,omitempty"`

	// Truncated says the result list was cut short. A silently shortened
	// report is worse than a failed one, because it looks complete.
	Truncated bool `json:"truncated,omitempty"`

	// Log is the run's transcript: what each chart produced, which refused and
	// why, what each stage cost. Recorded with the run rather than left in the
	// Coordinator's memory, because the question it answers - "why did this take
	// nine minutes and come back with eleven charts missing" - is asked after
	// the run, not during it.
	Log []ProgressEvent `json:"log,omitempty"`

	// Rendered is the manifest text the results were judged against, so a
	// reader can verify a finding instead of trusting it. Excluded from this
	// type's JSON deliberately: it is megabytes, it is served by its own
	// endpoints a document at a time, and a report that inlined it would be a
	// report nothing can open.
	Rendered []RenderedDoc `json:"-"`
}

// ChartStatus is one chart's contribution to a run.
type ChartStatus struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	// ErrorKind classifies the failure and Attempts says how hard it was
	// tried, so a reader can tell a chart that requires values it does not ship
	// from one whose template is broken - and a retried failure from one a
	// retry could not have fixed.
	ErrorKind string `json:"errorKind,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	Resources int    `json:"resources"`
}

// Execute evaluates a release and assembles the run.
//
// # Why unrendered charts produce results rather than silence
//
// A chart that would not render contributes no resources, so every check
// applies to nothing from it - and a check that applies to nothing reports a
// skip, or is quietly satisfied by the other ninety-six charts that did
// render. Either way the failure disappears, and the screen a release manager
// reads is indistinguishable from a clean one.
//
// So each check that could not be decided for that chart says so, addressed to
// the chart, with helm's own error. The run is inconclusive, the volume is one
// row per check per failed chart, and nobody reads a green screen over a
// release that was never rendered.
func Execute(ctx context.Context, eng *Engine, rel *Release, started time.Time) (*Run, error) {
	run := &Run{
		Product:       rel.Product,
		Release:       rel.Tag,
		PackageDigest: rel.PackageDigest,
		StartedAt:     started,
		BundleDigest:  eng.Catalog.BundleDigest,
		HelmVersion:   rel.HelmVersion,
		KubeVersion:   rel.KubeVersion,
		Checks:        eng.Catalog.Len(),
	}

	counted := map[string]int{}
	for i := range rel.Resources {
		counted[rel.Resources[i].Address.Chart]++
	}
	for _, c := range rel.Charts {
		run.Charts = append(run.Charts, ChartStatus{
			Name: c.Name, Version: c.Version, Status: c.RenderStatus,
			Error: c.RenderError, ErrorKind: c.RenderErrorKind, Attempts: c.RenderAttempts,
			Resources: counted[c.Name],
		})
	}

	results, err := eng.Run(ctx, rel)
	if err != nil && !errors.Is(err, ErrTruncated) {
		return nil, err
	}
	run.Truncated = errors.Is(err, ErrTruncated)

	run.Rendered = rel.Rendered

	results = append(results, undecidedFor(eng.Catalog, rel)...)
	Sort(results)

	run.Results = results
	run.Counts = Tally(results)
	run.Verdict = Decide(run.Counts)
	run.FinishedAt = time.Now().UTC()
	return run, nil
}

// undecidedFor produces the error results for charts that did not render.
func undecidedFor(cat *Catalog, rel *Release) []Result {
	var out []Result
	for _, c := range rel.Charts {
		if c.RenderStatus == RenderOK {
			continue
		}
		reason := c.RenderError
		if reason == "" {
			reason = "the chart did not render, and no reason was recorded"
		}
		for _, check := range cat.Checks() {
			if check.Deprecated {
				continue
			}
			out = append(out, Result{
				CheckID:     check.ID,
				CheckTitle:  check.Title,
				Severity:    check.Severity,
				Tier:        check.Tier,
				Category:    check.Category,
				Pack:        check.Pack,
				Remediation: check.Remediation,
				Reference:   check.Reference,
				Outcome:     OutcomeError,
				Determinacy: DeterminacyUnknown,
				Address: Address{
					Product: rel.Product, Release: rel.Tag, PackageDigest: rel.PackageDigest,
					Chart: c.Name, ChartVersion: c.Version, SubchartPath: c.SubchartPath,
				},
				Error:   reason,
				Message: "chart " + c.Name + " did not render, so this check could not be decided for it",
			})
		}
	}
	return out
}
