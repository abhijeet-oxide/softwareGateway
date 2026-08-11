// Package calibrate measures one source-to-target path and says what to
// configure.
//
// # Why this exists
//
// Throughput here is decided by at least six settings that interact:
// concurrency.perRegistry, concurrency.requestsPerSecond,
// worker.maxConcurrentJobs, the number of workers, network.proxy, and where the
// workers run. Nothing about a configuration file says which of them is
// currently the binding constraint, so tuning is guesswork — and the guesses
// are usually wrong in the same direction. Raising concurrency when the link is
// already saturated adds load and no bytes; adding a worker when a proxy is
// halving the line rate hides the real problem behind more parallelism.
//
// The one thing that answers this is a measurement of the actual path, so this
// package makes one: it reads real blobs from the source, writes real bytes to
// the target, sweeps the concurrency, and reports where the curve stops rising.
//
// # How it differs from preflight
//
//	preflight   Can we reach it, are the credentials right? Yes or no, a
//	            handful of requests, safe on any registry at any time.
//
//	calibrate   How fast is it, and what setting would make it faster? Moves
//	            real data, takes minutes, deliberately loads both ends.
//
// Preflight answers a question about correctness and calibrate answers one
// about capacity. They are separate commands because the second is expensive
// and nobody should run it by accident.
//
// # What it will not do
//
// It leaves nothing behind. The write probe opens an upload session, PATCHes
// bytes into it and then CANCELS it, so no blob is ever committed and no tag,
// manifest or blob appears in the target. See probeWrite.
//
// # Where it runs
//
// In the Coordinator process, because transferctl is a pure API client and
// must not touch a registry itself. That is the right answer when the workers
// share the Coordinator's network — one laptop, or pods on one cluster — and
// the wrong one when they do not, so every report says which host measured it.
// Dispatching the probe to a named worker is the natural next step and needs
// the worker's address, which the fleet does not currently report.
package calibrate

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Options configure one run.
type Options struct {
	// Source and Target name configured entries. Empty picks the product's
	// only source, and its default target.
	Source string
	Target string

	// SourceRepository overrides which repository is read from. Needed only
	// for a source that enumerates its repositories from the catalog and
	// happens to pick an empty one.
	SourceRepository string

	// Levels are the concurrency levels to sweep, ascending.
	Levels []int

	// Budget is how long ONE level runs. The whole run costs roughly
	// budget × (levels + 2) per side.
	Budget time.Duration

	// Write enables the target-side probe. On by default; the flag exists
	// because a target that is somebody else's production registry may be
	// governed by a change process that this quietly violates, even leaving
	// nothing behind.
	Write bool

	// BundleBytes is a transfer size to project the measured ceiling onto, so
	// the report can end in a duration rather than a rate. Zero omits it.
	BundleBytes int64
}

// Defaults.
const (
	DefaultBudget = 5 * time.Second

	// chunkSize is one PATCH of the write probe, and one read's accounting
	// granularity. A megabyte is large enough that per-request overhead does
	// not dominate and small enough that a level's final partial chunk is
	// rounding rather than a quarter of the sample.
	chunkSize = 1 << 20
)

// DefaultLevels is the sweep when none is given.
//
// Powers of two, because the question is where the curve BENDS and a linear
// sweep spends most of its time confirming a plateau it has already found.
//
// It stops at sixteen, and that is not a statement about what a network can do.
// It is a limit on what this is willing to do to somebody else's registry
// without being asked: a vendor seeing thirty-two concurrent blob reads from
// one client has a legitimate complaint, and every path measured so far has its
// knee well below sixteen. `--concurrency` raises it deliberately.
func DefaultLevels() []int { return []int{1, 2, 4, 8, 16} }

func (o Options) withDefaults() Options {
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}

	// Filtered BEFORE the default is applied, so a caller that passes only
	// nonsense — `levels: [0]` over the API — gets the default sweep rather
	// than a run that probes nothing and reports an empty table with no
	// explanation for it.
	levels := make([]int, 0, len(o.Levels))
	for _, l := range o.Levels {
		if l > 0 {
			levels = append(levels, l)
		}
	}
	if len(levels) == 0 {
		levels = DefaultLevels()
	}
	sort.Ints(levels)
	o.Levels = levels
	return o
}

// Report is one calibration run.
type Report struct {
	Product string
	// MeasuredFrom is the host that ran the probes. Load-bearing: a
	// measurement of the Coordinator's network is a measurement of the
	// workers' network only when they are the same network.
	MeasuredFrom string
	StartedAt    time.Time
	Duration     time.Duration

	Source SideReport
	Target SideReport

	Suggestions []Suggestion
	// Notes are caveats about the measurement itself — a sample too small to
	// trust, a probe that could not run. Kept separate from suggestions
	// because they qualify the evidence rather than propose a change.
	Notes []string
}

// SideReport is everything measured about one end of the path.
type SideReport struct {
	Role       string
	Name       string
	Registry   string
	Repository string

	Route RouteReport
	// RTT is the round trip to /v2/, taken as the minimum of several probes.
	// The minimum rather than the mean: it is the closest thing to the path's
	// intrinsic latency, with queueing and scheduling noise removed.
	RTT time.Duration

	Levels []LevelResult
	// Best is the level that moved the most bytes per second.
	Best LevelResult
	// Knee is the smallest concurrency reaching 90% of Best — the level worth
	// configuring, since everything past it buys a few percent for
	// proportionally more load on somebody else's registry.
	Knee int
	// StillClimbing means the top of the sweep was the fastest level AND it
	// improved meaningfully on the one below, so the ceiling was not found.
	StillClimbing bool

	// Skipped explains why there are no measurements, when there are none.
	Skipped string
}

// Measured reports whether this side produced usable numbers.
func (s SideReport) Measured() bool { return len(s.Levels) > 0 && s.Best.Rate > 0 }

// RouteReport is what the traffic goes through, and what it would do if it
// went the other way.
type RouteReport struct {
	// Configured is transport.DescribeProxy: the route in force, including one
	// inherited from the environment.
	Configured string
	ProxyInUse bool

	// DirectTested records whether the direct route was probed at all. It is
	// not, when no proxy is in play — there is nothing to compare against.
	DirectTested    bool
	DirectReachable bool
	// DirectDetail carries the failure when the direct route is unreachable,
	// which is itself the answer to "can we just turn the proxy off".
	DirectDetail string

	// ProxiedRate and DirectRate are a head-to-head at one fixed concurrency,
	// in bytes per second. Zero means not measured.
	ProxiedRate float64
	DirectRate  float64
}

// LevelResult is one concurrency level's measurement.
type LevelResult struct {
	Concurrency int
	Bytes       int64
	Duration    time.Duration
	// Rate is aggregate bytes per second across all streams.
	Rate float64
	// PerStream is Rate divided by Concurrency. Its DECLINE is the signal: a
	// per-stream rate that halves as streams double means the streams are
	// sharing a fixed ceiling rather than each getting the line.
	PerStream float64

	Requests int
	Errors   int
	// Throttled counts 429 and 503. A registry saying "slow down" is the one
	// piece of evidence that outranks a throughput number.
	Throttled int
	// TTFB is the median time to first byte at this level. Rising sharply with
	// concurrency means requests are queueing somewhere.
	TTFB time.Duration

	// FirstError is the first failure seen at this level, kept verbatim.
	//
	// A count alone is unactionable: "14 errors" and "14 × 403 Forbidden" are
	// the same number and only one of them tells you what to do.
	FirstError string
}

// Severity ranks a suggestion.
type Severity string

const (
	// SeverityCritical is a setting that is actively costing throughput or is
	// wrong, with measured evidence.
	SeverityCritical Severity = "critical"
	// SeverityRecommended is a change with a measured benefit that is not
	// dramatic.
	SeverityRecommended Severity = "recommended"
	// SeverityInfo states a measured fact with no change attached — the
	// ceiling, the projection, the reason not to do something.
	SeverityInfo Severity = "info"
)

// Suggestion is one thing to change, or one reason not to.
type Suggestion struct {
	Severity Severity
	// Setting is the configuration key, in the spelling the file uses. Empty
	// for a finding with no knob behind it.
	Setting string
	Scope   string
	// Current and Suggested are values, rendered. Empty Suggested means the
	// suggestion is not a change.
	Current   string
	Suggested string
	// Evidence is the measurement this rests on. Every suggestion has one:
	// advice without a number is the guesswork this package replaces.
	Evidence string
}

// Calibrator runs calibrations.
type Calibrator struct {
	secrets *product.SecretResolver
	// host is reported as the measuring host, resolved once.
	host string
}

// NewCalibrator builds one.
func NewCalibrator(secrets *product.SecretResolver) *Calibrator {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown host"
	}
	return &Calibrator{secrets: secrets, host: host}
}

// Run measures the path between one source and one target.
//
// It returns an error only when the run could not START — an unresolvable
// source, a target the product does not declare. A probe that fails once it is
// running is reported as a skipped side with the reason, because the other side
// was still measured and half a report is worth having.
func (c *Calibrator) Run(ctx context.Context, p *product.Product, opts Options) (Report, error) {
	opts = opts.withDefaults()

	src, err := c.resolveSource(p, opts)
	if err != nil {
		return Report{}, err
	}
	tgt, err := c.resolveTarget(p, opts)
	if err != nil {
		return Report{}, err
	}

	started := time.Now()
	rep := Report{
		Product:      p.Metadata.Name,
		MeasuredFrom: c.host,
		StartedAt:    started,
	}

	rep.Source, rep.Notes = c.measureSource(ctx, &src, opts, rep.Notes)

	// The target probe writes to base + the source path the read probe settled
	// on, which is a path a transfer genuinely uses. Deriving it here — after
	// the source is known — is why the two halves are not independent.
	tgt.cfg.Repository = writeProbePath(tgt, src.cfg.Repository)

	if opts.Write {
		rep.Target, rep.Notes = c.measureTarget(ctx, tgt, opts, rep.Notes)
	} else {
		rep.Target = SideReport{
			Role: string(product.RoleTarget), Name: tgt.name,
			Registry: tgt.cfg.Registry, Repository: tgt.cfg.Repository,
			Skipped: "the write probe was not requested",
		}
		rep.Target.Route = describeRoute(tgt.cfg)
	}

	rep.Duration = time.Since(started)
	rep.Suggestions = advise(rep, src, tgt, opts)
	rep.Notes = append(rep.Notes, measurementNotes(rep, opts)...)
	return rep, nil
}

// measureSource probes the read half: route, latency, and a concurrency sweep
// over real blobs.
func (c *Calibrator) measureSource(
	ctx context.Context, s *endpoint, opts Options, notes []string,
) (SideReport, []string) {
	out := SideReport{
		Role: string(product.RoleSource), Name: s.name,
		Registry: s.cfg.Registry, Repository: s.cfg.Repository,
		Route: describeRoute(s.cfg),
	}

	samples, chosen, err := collectSamples(ctx, s.cfg, s.candidates)
	if err != nil {
		out.Skipped = "no blob could be found to read: " + err.Error()
		return out, notes
	}

	// The repository the search settled on, which is not necessarily the one it
	// started with. Recorded on the endpoint as well as the report, because the
	// target probe derives its own path from it.
	s.cfg.Repository = chosen
	out.Repository = chosen
	if len(s.candidates) > 1 {
		notes = append(notes, fmt.Sprintf(
			"the source spans %d repositories; %s was measured. Use --source-repository "+
				"to measure a different one", len(s.candidates), chosen))
	}
	if n := len(samples); n < 2 {
		notes = append(notes, fmt.Sprintf(
			"the source read probe had %d sample blob(s) to work with, so a cache "+
				"anywhere on the path would inflate its numbers", n))
	}

	read := func(ctx context.Context, cfg registry.ClientConfig, streams int, budget time.Duration) LevelResult {
		return probeRead(ctx, cfg, samples, streams, budget)
	}

	cfg := s.cfg
	out.RTT = measureRTT(ctx, cfg)
	cfg, out.Route = compareRoutes(ctx, cfg, out.Route, opts, read)
	out.Levels = sweep(ctx, cfg, opts, read)
	out.Best, out.Knee, out.StillClimbing = analyseSweep(out.Levels)
	return out, notes
}

// measureTarget probes the write half.
func (c *Calibrator) measureTarget(
	ctx context.Context, t endpoint, opts Options, notes []string,
) (SideReport, []string) {
	out := SideReport{
		Role: string(product.RoleTarget), Name: t.name,
		Registry: t.cfg.Registry, Repository: t.cfg.Repository,
		Route: describeRoute(t.cfg),
	}

	if err := checkUploadSupported(ctx, t.cfg); err != nil {
		out.Skipped = "the target would not open an upload session: " + err.Error()
		return out, notes
	}

	write := func(ctx context.Context, cfg registry.ClientConfig, streams int, budget time.Duration) LevelResult {
		return probeWrite(ctx, cfg, streams, budget)
	}

	cfg := t.cfg
	out.RTT = measureRTT(ctx, cfg)
	cfg, out.Route = compareRoutes(ctx, cfg, out.Route, opts, write)
	out.Levels = sweep(ctx, cfg, opts, write)
	out.Best, out.Knee, out.StillClimbing = analyseSweep(out.Levels)
	return out, notes
}

// probe is one measurement at one concurrency, on one route.
type probe func(
	ctx context.Context, cfg registry.ClientConfig, streams int, budget time.Duration,
) LevelResult

// sweep runs the probe at each level, stopping when the curve flattens.
//
// # Why it stops early
//
// Each additional level doubles the load this puts on somebody else's
// registry, and a level that improves on its predecessor by less than a tenth
// has already answered the question: the ceiling is here. Continuing to
// sixteen streams to watch a flat line is load with no information in it.
func sweep(ctx context.Context, cfg registry.ClientConfig, opts Options, run probe) []LevelResult {
	var out []LevelResult

	for _, level := range opts.Levels {
		if ctx.Err() != nil {
			return out
		}
		res := run(ctx, withConnections(cfg, level), level, opts.Budget)
		out = append(out, res)

		if len(out) < 2 {
			continue
		}
		prev := out[len(out)-2]
		if prev.Rate > 0 && res.Rate < prev.Rate*plateauFactor {
			return out
		}
	}
	return out
}

// plateauFactor is the improvement a level must show for the sweep to keep
// doubling. Ten percent: below that the difference is inside the run-to-run
// noise of a shared link anyway.
const plateauFactor = 1.10

// analyseSweep reduces a sweep to the three facts advice is built from.
func analyseSweep(levels []LevelResult) (best LevelResult, knee int, climbing bool) {
	for _, l := range levels {
		if l.Rate > best.Rate {
			best = l
		}
	}
	if best.Rate <= 0 {
		return best, 0, false
	}

	// The smallest level that gets within a tenth of the best. Configuring the
	// best level rather than this one buys single-digit percent for double the
	// concurrent load.
	knee = best.Concurrency
	for _, l := range levels {
		if l.Rate >= best.Rate*0.9 {
			knee = l.Concurrency
			break
		}
	}

	last := levels[len(levels)-1]
	if last.Concurrency == best.Concurrency && len(levels) > 1 {
		prev := levels[len(levels)-2]
		climbing = prev.Rate > 0 && last.Rate > prev.Rate*plateauFactor
	}
	return best, knee, climbing
}

// measurementNotes records what would make the numbers untrustworthy.
func measurementNotes(rep Report, opts Options) []string {
	var notes []string

	notes = append(notes, fmt.Sprintf(
		"measured from %s. If workers run elsewhere, their path to these registries "+
			"is not the one measured here", rep.MeasuredFrom))

	if opts.Budget < 3*time.Second {
		notes = append(notes, fmt.Sprintf(
			"each level ran for only %s, which is short enough that TCP is still "+
				"opening its congestion window when the measurement ends", opts.Budget))
	}
	if !opts.Write {
		notes = append(notes, "the target was not measured, so the ceiling below is the "+
			"read half only — the write half can be lower")
	}
	return notes
}
