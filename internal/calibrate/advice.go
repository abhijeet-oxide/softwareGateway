package calibrate

import (
	"fmt"
	"sort"
	"time"
)

// Turning measurements into settings.
//
// # The rule every suggestion here follows
//
// It names a configuration key, and it carries the measurement it rests on.
// Advice without a number is the guesswork this package was built to replace,
// and advice with a number but no key is a fact the reader still has to
// translate. Both halves, or it does not belong in this list.
//
// # What is deliberately not suggested
//
// A change whose benefit was not measured. It is tempting to recommend a bigger
// copyBufferSize or a different timeout on general principle, and neither is
// something these probes observe — so neither appears, however plausible. The
// list is short on purpose: an operator who finds two of eight suggestions
// unfounded stops reading the other six.

// advise builds the suggestion list, most severe first.
func advise(rep Report, src, tgt endpoint, opts Options) []Suggestion {
	var out []Suggestion

	out = append(out, routeAdvice(rep.Source, "source "+src.name)...)
	out = append(out, routeAdvice(rep.Target, "target "+tgt.name)...)

	out = append(out, concurrencyAdvice(rep.Source, src)...)
	out = append(out, concurrencyAdvice(rep.Target, tgt)...)

	out = append(out, throttlingAdvice(rep.Source, src)...)
	out = append(out, throttlingAdvice(rep.Target, tgt)...)

	out = append(out, jobConcurrencyAdvice(rep)...)
	out = append(out, workerAdvice(rep)...)
	out = append(out, ceilingAdvice(rep, opts)...)
	out = append(out, latencyAdvice(rep)...)
	out = append(out, skippedAdvice(rep)...)

	// Stable sort by severity only, so the order within a severity stays the
	// order above — route before concurrency before ceiling, which is the order
	// somebody would act in.
	rank := map[Severity]int{SeverityCritical: 0, SeverityRecommended: 1, SeverityInfo: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out
}

// routeAdvice answers "should this side bypass the proxy?".
func routeAdvice(side SideReport, scope string) []Suggestion {
	r := side.Route
	if !r.ProxyInUse {
		return nil
	}

	const setting = "network.proxy.direct"

	if r.DirectTested && !r.DirectReachable {
		// The most valuable negative result this produces. Somebody who has
		// read that bypassing the proxy helped elsewhere needs to know that
		// here it is the only route out.
		return []Suggestion{{
			Severity: SeverityInfo, Setting: setting, Scope: scope,
			Current: "unset (traffic goes through the proxy)", Suggested: "leave unset",
			Evidence: "a direct connection to this registry failed: " + r.DirectDetail,
		}}
	}
	if !r.DirectTested || r.ProxiedRate <= 0 {
		return nil
	}

	switch {
	case r.DirectRate > r.ProxiedRate*proxyMargin:
		return []Suggestion{{
			Severity: SeverityCritical, Setting: setting, Scope: scope,
			Current: "unset (traffic goes through the proxy)", Suggested: "true",
			Evidence: fmt.Sprintf("direct moved %s against %s through the proxy — %s faster. "+
				"The proxy in force is: %s",
				humanRate(r.DirectRate), humanRate(r.ProxiedRate),
				percentDiff(r.DirectRate, r.ProxiedRate), r.Configured),
		}}

	case r.ProxiedRate > r.DirectRate*proxyMargin:
		return []Suggestion{{
			Severity: SeverityInfo, Setting: setting, Scope: scope,
			Current: "unset (traffic goes through the proxy)", Suggested: "leave unset",
			Evidence: fmt.Sprintf("the proxy was FASTER: %s against %s direct",
				humanRate(r.ProxiedRate), humanRate(r.DirectRate)),
		}}

	default:
		return []Suggestion{{
			Severity: SeverityInfo, Setting: setting, Scope: scope,
			Current: "unset (traffic goes through the proxy)", Suggested: "no change",
			Evidence: fmt.Sprintf("proxied %s against %s direct, which is inside the noise "+
				"of a shared link", humanRate(r.ProxiedRate), humanRate(r.DirectRate)),
		}}
	}
}

// proxyMargin is how much faster one route must be before the difference is
// called a difference. Fifteen percent: two consecutive runs on a shared
// corporate link routinely differ by ten.
const proxyMargin = 1.15

// concurrencyAdvice compares the configured pool against the measured knee.
func concurrencyAdvice(side SideReport, e endpoint) []Suggestion {
	if !side.Measured() {
		return nil
	}

	scope := string(e.role) + " " + e.name
	configured := e.limits.PerRegistry
	var out []Suggestion

	switch {
	case side.StillClimbing:
		// The sweep ran out before the path did, so the knee is above anything
		// measured. What to say about the pool then depends on whether the pool
		// is the thing in the way: one already wider than the sweep is not, and
		// telling somebody to "raise it to 4" when it is 32 would be advice
		// derived from the probe's own ceiling rather than from the network.
		top := side.Best.Concurrency
		wider := fmt.Sprintf("Re-run with --concurrency %d,%d,%d to find where it stops",
			top, top*2, top*4)

		if configured >= top {
			out = append(out, Suggestion{
				Severity: SeverityInfo, Setting: "concurrency.perRegistry", Scope: scope,
				Current: describePool(configured), Suggested: "no change on this evidence",
				Evidence: fmt.Sprintf(
					"throughput was still rising at the top of the sweep (%s at %d streams), "+
						"so the knee is above what was measured and the pool of %s is not what "+
						"is limiting it. %s",
					humanRate(side.Best.Rate), top, describePool(configured), wider),
			})
			break
		}
		out = append(out, Suggestion{
			Severity: SeverityRecommended, Setting: "concurrency.perRegistry", Scope: scope,
			Current: describePool(configured), Suggested: fmt.Sprintf("at least %d", top),
			Evidence: fmt.Sprintf(
				"throughput was still rising at the top of the sweep (%s at %d streams). %s",
				humanRate(side.Best.Rate), top, wider),
		})

	case configured > side.Knee*2:
		out = append(out, Suggestion{
			Severity: SeverityRecommended, Setting: "concurrency.perRegistry", Scope: scope,
			Current: describePool(configured), Suggested: fmt.Sprint(side.Knee),
			Evidence: fmt.Sprintf(
				"%d streams reached %s, within a tenth of the best measured (%s at %d). "+
					"The rest of the pool adds load on the registry and no bytes",
				side.Knee, humanRate(rateAt(side, side.Knee)), humanRate(side.Best.Rate),
				side.Best.Concurrency),
		})

	case configured > 0 && configured < side.Knee:
		out = append(out, Suggestion{
			Severity: SeverityCritical, Setting: "concurrency.perRegistry", Scope: scope,
			Current: describePool(configured), Suggested: fmt.Sprint(side.Knee),
			Evidence: fmt.Sprintf(
				"throughput kept rising to %d streams (%s), and the pool is capped at %d. "+
					"Every job past the cap waits for a connection",
				side.Knee, humanRate(rateAt(side, side.Knee)), configured),
		})

	default:
		out = append(out, Suggestion{
			Severity: SeverityInfo, Setting: "concurrency.perRegistry", Scope: scope,
			Current: describePool(configured), Suggested: "no change",
			Evidence: fmt.Sprintf("the knee is at %d streams (%s) and the pool is %s",
				side.Knee, humanRate(rateAt(side, side.Knee)), describePool(configured)),
		})
	}
	return out
}

// throttlingAdvice reports a registry asking to be worked less hard.
//
// This outranks every throughput number in the report. A 429 is the other end
// stating its own limit, and tuning concurrency to sit just under the point
// where it starts refusing requests is optimising against somebody else's
// stated policy.
func throttlingAdvice(side SideReport, e endpoint) []Suggestion {
	var worst LevelResult
	for _, l := range side.Levels {
		if l.Throttled > worst.Throttled {
			worst = l
		}
	}
	if worst.Throttled == 0 {
		return nil
	}

	scope := string(e.role) + " " + e.name
	return []Suggestion{{
		Severity: SeverityCritical, Setting: "concurrency.requestsPerSecond", Scope: scope,
		Current:   describeRPS(e.limits.RequestsPerSecond),
		Suggested: "set a limit this registry accepts",
		Evidence: fmt.Sprintf(
			"%d request(s) were throttled or shed at %d streams. The registry is stating a "+
				"limit; more concurrency will produce retries rather than throughput",
			worst.Throttled, worst.Concurrency),
	}}
}

// jobConcurrencyAdvice sizes the worker against the slower half of the path.
//
// One job is one blob moving through the worker: a read stream on the source
// and a write stream on the target, alive at the same time. So the useful
// number of jobs in flight is the SMALLER of the two knees — sizing to the
// faster side just queues work in front of the slower one.
func jobConcurrencyAdvice(rep Report) []Suggestion {
	src, tgt := rep.Source, rep.Target
	if !src.Measured() {
		return nil
	}

	jobs := src.Knee
	basis := fmt.Sprintf("the source knee is %d streams", src.Knee)
	if tgt.Measured() {
		if tgt.Knee < jobs {
			jobs = tgt.Knee
		}
		basis = fmt.Sprintf("the source knee is %d streams and the target's is %d; a job holds "+
			"one of each at once", src.Knee, tgt.Knee)
	}

	return []Suggestion{{
		Severity: SeverityRecommended, Setting: "worker.maxConcurrentJobs",
		Scope: "each worker", Suggested: fmt.Sprint(jobs),
		Evidence: basis + ". Keep concurrency.perRegistry at or above this, or jobs queue " +
			"for a connection instead of running",
	}}
}

// workerAdvice answers "would another worker help?".
//
// The question the measurement can actually settle is whether the PATH has
// headroom, and that is what this reports. A second worker adds processes, not
// bandwidth: on the same host it competes for the same link, so it can only
// help if the ceiling is in this process rather than on the wire — and a
// plateau that appears with more streams is evidence that it is not.
func workerAdvice(rep Report) []Suggestion {
	src, tgt := rep.Source, rep.Target
	if !src.Measured() {
		return nil
	}

	throttled := anyThrottled(src) || anyThrottled(tgt)
	if throttled {
		return []Suggestion{{
			Severity: SeverityInfo, Setting: "number of workers", Scope: "deployment",
			Suggested: "do not add one yet",
			Evidence: "a registry throttled this probe. More workers means more concurrent " +
				"requests to the same registry, which is what it just asked for less of",
		}}
	}

	if src.StillClimbing || tgt.StillClimbing {
		return []Suggestion{{
			Severity: SeverityInfo, Setting: "number of workers", Scope: "deployment",
			Suggested: "raise concurrency before adding one",
			Evidence: "throughput had not stopped rising when the sweep ended, so this path " +
				"still has headroom. One worker with a larger pool reaches it more cheaply " +
				"than two workers with small ones",
		}}
	}

	ceiling, binding := effectiveCeiling(rep)
	return []Suggestion{{
		Severity: SeverityInfo, Setting: "number of workers", Scope: "deployment",
		Suggested: "another worker on THIS host will not help",
		Evidence: fmt.Sprintf(
			"the %s path flattened at %s and stopped improving with more streams, so the "+
				"limit is the link or the registry rather than this process. A worker on a "+
				"different network path would add capacity; one beside the first shares this one",
			binding, humanRate(ceiling)),
	}}
}

// ceilingAdvice states the number everything else is measured against.
func ceilingAdvice(rep Report, opts Options) []Suggestion {
	if !rep.Source.Measured() {
		return nil
	}

	ceiling, binding := effectiveCeiling(rep)
	evidence := fmt.Sprintf("read %s from the source", humanRate(rep.Source.Best.Rate))
	if rep.Target.Measured() {
		evidence += fmt.Sprintf(", wrote %s to the target; the %s side governs",
			humanRate(rep.Target.Best.Rate), binding)
	} else {
		evidence += " (the target was not measured, so the write half could be lower)"
	}

	out := []Suggestion{{
		Severity: SeverityInfo, Setting: "", Scope: "this path",
		Suggested: humanRate(ceiling) + " is the ceiling measured today",
		Evidence:  evidence,
	}}

	if opts.BundleBytes > 0 && ceiling > 0 {
		d := time.Duration(float64(opts.BundleBytes)/ceiling) * time.Second
		out = append(out, Suggestion{
			Severity: SeverityInfo, Scope: "this path",
			Suggested: fmt.Sprintf("%s would take about %s at that rate",
				humanSize(opts.BundleBytes), humanDuration(d)),
			Evidence: "excludes blobs already present at the destination and any the transfer " +
				"can mount instead of copying, both of which move nothing",
		})
	}
	return out
}

// latencyAdvice explains WHY parallelism is the lever on a long path.
//
// A single TCP stream cannot exceed its window divided by the round trip, so on
// a high-latency link one stream leaves most of the line idle no matter how
// much bandwidth there is. Multiplying the observed single-stream rate by the
// round trip recovers the window actually in use, and when that is small the
// answer is more streams — not a bigger buffer, not a faster disk, and not a
// different tool.
func latencyAdvice(rep Report) []Suggestion {
	side := rep.Source
	if !side.Measured() || side.RTT <= 0 {
		return nil
	}

	single := rateAt(side, 1)
	if single <= 0 {
		return nil
	}
	window := single * side.RTT.Seconds()
	if window > 4<<20 || side.RTT < 20*time.Millisecond {
		return nil
	}

	return []Suggestion{{
		Severity: SeverityInfo, Setting: "", Scope: "source " + side.Name,
		Suggested: "parallelism is the only lever on this link",
		Evidence: fmt.Sprintf(
			"round trip %s, and one stream reached %s — about %s in flight at a time. "+
				"A single connection cannot do better on a link this long, whatever the "+
				"bandwidth; %d streams reached %s",
			humanDuration(side.RTT), humanRate(single), humanSize(int64(window)),
			side.Best.Concurrency, humanRate(side.Best.Rate)),
	}}
}

// skippedAdvice says plainly when half the report is missing.
func skippedAdvice(rep Report) []Suggestion {
	var out []Suggestion
	for _, s := range []SideReport{rep.Source, rep.Target} {
		if s.Skipped == "" {
			continue
		}
		out = append(out, Suggestion{
			Severity: SeverityInfo, Scope: s.Role + " " + s.Name,
			Suggested: "not measured",
			Evidence:  s.Skipped,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// effectiveCeiling is the slower half of the path, which is the one that
// decides how fast a transfer runs.
func effectiveCeiling(rep Report) (rate float64, binding string) {
	rate, binding = rep.Source.Best.Rate, "read"
	if rep.Target.Measured() && rep.Target.Best.Rate < rate {
		rate, binding = rep.Target.Best.Rate, "write"
	}
	return rate, binding
}

// rateAt returns the measured rate at one concurrency, or 0 if that level was
// never run.
func rateAt(side SideReport, streams int) float64 {
	for _, l := range side.Levels {
		if l.Concurrency == streams {
			return l.Rate
		}
	}
	return 0
}

func anyThrottled(side SideReport) bool {
	for _, l := range side.Levels {
		if l.Throttled > 0 {
			return true
		}
	}
	return false
}

// describePool renders a connection limit, including the unset case.
//
// Zero is not "no connections" — it means nothing was set for this registry and
// the deployment-wide default applies. Printing a bare 0 would read as the
// opposite of what it means.
func describePool(n int) string {
	if n <= 0 {
		return "the deployment default"
	}
	return fmt.Sprint(n)
}

func describeRPS(rps int) string {
	if rps <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(rps)
}

// percentDiff renders how much faster a is than b.
func percentDiff(a, b float64) string {
	if b <= 0 {
		return "immeasurably"
	}
	return fmt.Sprintf("%.0f%%", (a/b-1)*100)
}
