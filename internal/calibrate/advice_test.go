package calibrate

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// The arithmetic behind the advice, tested without a network.
//
// These are the assertions that matter most in the long run: a probe that
// misreports itself fails visibly, while advice that follows from nothing fails
// silently - somebody changes a setting, throughput does not move, and the
// tool is never trusted again.

func TestSweepStopsWhenTheCurveFlattens(t *testing.T) {
	// A path that saturates at four streams. Levels past the plateau must not
	// be run: each one doubles the load on somebody else's registry to learn
	// something already known.
	rates := map[int]float64{1: 1_000_000, 2: 2_000_000, 4: 3_900_000, 8: 4_000_000, 16: 4_000_000}
	var ran []int

	levels := sweep(t.Context(), registryConfig(), Options{
		Levels: []int{1, 2, 4, 8, 16}, Budget: time.Second,
	}, func(_ context.Context, cfg registry.ClientConfig, streams int, _ time.Duration) LevelResult {
		ran = append(ran, streams)
		// The pool must be sized to the level under test, or the sweep would
		// measure the configured pool over and over and find a plateau that is
		// its own.
		if cfg.MaxConnections != streams {
			t.Errorf("level %d ran with a pool of %d", streams, cfg.MaxConnections)
		}
		return LevelResult{Concurrency: streams, Rate: rates[streams]}
	})

	if want := []int{1, 2, 4, 8}; !slices.Equal(ran, want) {
		t.Errorf("ran levels %v, want %v - 16 adds load and no information", ran, want)
	}
	if len(levels) != 4 {
		t.Errorf("recorded %d levels, want 4", len(levels))
	}
}

func registryConfig() registry.ClientConfig {
	return registry.ClientConfig{Registry: "example.invalid", Repository: "x"}
}

func TestAnalyseSweepPrefersTheSmallestLevelThatIsNearlyTheBest(t *testing.T) {
	levels := []LevelResult{
		{Concurrency: 1, Rate: 1_000_000},
		{Concurrency: 2, Rate: 1_500_000},
		{Concurrency: 4, Rate: 2_000_000},
		{Concurrency: 8, Rate: 2_050_000},
	}

	best, knee, climbing := analyseSweep(levels)
	if best.Concurrency != 8 {
		t.Errorf("best = %d streams, want 8", best.Concurrency)
	}
	// Four reaches 97% of the best. Recommending eight would double the
	// concurrent load on the registry for three percent.
	if knee != 4 {
		t.Errorf("knee = %d, want 4 - the smallest level within a tenth of the best", knee)
	}
	if climbing {
		t.Error("reported as still climbing when the last level added 2%")
	}
}

func TestAnalyseSweepReportsACurveThatNeverFlattened(t *testing.T) {
	levels := []LevelResult{
		{Concurrency: 1, Rate: 1_000_000},
		{Concurrency: 2, Rate: 2_000_000},
		{Concurrency: 4, Rate: 4_000_000},
	}

	_, knee, climbing := analyseSweep(levels)
	if !climbing {
		t.Error("a curve that doubled at every level was reported as settled")
	}
	if knee != 4 {
		t.Errorf("knee = %d, want the top of the sweep", knee)
	}
}

// The recommendation that was worth the most on a real transfer, and the one
// most likely to be wrong. Both directions have to work.
func TestProxyAdviceFollowsTheMeasurement(t *testing.T) {
	base := SideReport{Name: "vendor", Route: RouteReport{
		Configured: "environment http://proxy.corp:8080", ProxyInUse: true,
		DirectTested: true, DirectReachable: true,
	}}

	faster := base
	faster.Route.ProxiedRate, faster.Route.DirectRate = 2<<20, 20<<20
	got := routeAdvice(faster, "source vendor")
	if len(got) != 1 || got[0].Severity != SeverityCritical || got[0].Suggested != "true" {
		t.Fatalf("a tenfold difference produced %+v, want a critical proxy.direct: true", got)
	}
	if !strings.Contains(got[0].Evidence, "900%") {
		t.Errorf("evidence %q does not quantify the difference", got[0].Evidence)
	}

	// Unreachable direct is the case where the benchmark must NOT win. Somebody
	// acting on "bypass the proxy" here loses connectivity entirely.
	blocked := base
	blocked.Route.DirectReachable = false
	blocked.Route.DirectDetail = "dial tcp: i/o timeout"
	got = routeAdvice(blocked, "source vendor")
	if len(got) != 1 || got[0].Suggested != "leave unset" {
		t.Fatalf("an unreachable direct route produced %+v, want leave-it-alone", got)
	}

	// Within the noise: say so rather than recommend churn.
	same := base
	same.Route.ProxiedRate, same.Route.DirectRate = 10<<20, 10.5*(1<<20)
	got = routeAdvice(same, "source vendor")
	if len(got) != 1 || got[0].Suggested != "no change" {
		t.Fatalf("a 5%% difference produced %+v, want no change", got)
	}

	// No proxy at all: nothing to say, and saying it anyway is noise in a list
	// that has to stay short to be read.
	if got := routeAdvice(SideReport{}, "source vendor"); len(got) != 0 {
		t.Errorf("advice offered about a proxy that is not in use: %+v", got)
	}
}

func TestConcurrencyAdviceComparesTheKneeWithTheConfiguredPool(t *testing.T) {
	e := endpoint{name: "vendor", role: product.RoleSource,
		limits: product.Concurrency{PerRegistry: 32}}
	side := SideReport{
		Name:   "vendor",
		Levels: []LevelResult{{Concurrency: 4, Rate: 2_000_000}, {Concurrency: 8, Rate: 2_050_000}},
		Best:   LevelResult{Concurrency: 8, Rate: 2_050_000},
		Knee:   4,
	}

	got := concurrencyAdvice(side, e)
	if len(got) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(got))
	}
	if got[0].Suggested != "4" || got[0].Current != "32" {
		t.Errorf("suggested %q from %q, want 4 from 32", got[0].Suggested, got[0].Current)
	}

	// The opposite: a pool smaller than the knee is throttling the transfer
	// from inside, which is the more expensive mistake and reads as critical.
	e.limits.PerRegistry = 2
	got = concurrencyAdvice(side, e)
	if len(got) != 1 || got[0].Severity != SeverityCritical {
		t.Fatalf("a pool below the knee produced %+v, want a critical suggestion", got)
	}
}

// A registry saying "slow down" outranks every throughput number in the report.
func TestThrottlingOutranksThroughput(t *testing.T) {
	e := endpoint{name: "vendor", role: product.RoleSource,
		limits: product.Concurrency{PerRegistry: 8}}
	side := SideReport{
		Name: "vendor",
		Levels: []LevelResult{
			{Concurrency: 4, Rate: 2_000_000},
			{Concurrency: 8, Rate: 3_000_000, Errors: 9, Throttled: 9},
		},
		Best: LevelResult{Concurrency: 8, Rate: 3_000_000},
		Knee: 8,
	}

	got := throttlingAdvice(side, e)
	if len(got) != 1 || got[0].Severity != SeverityCritical {
		t.Fatalf("throttling produced %+v, want a critical suggestion", got)
	}
	if got[0].Setting != "concurrency.requestsPerSecond" {
		t.Errorf("setting = %q, want the rate limit", got[0].Setting)
	}

	// And it must not then be told to add workers, which is more of exactly
	// what the registry just refused.
	rep := Report{Source: side}
	workers := workerAdvice(rep)
	if len(workers) != 1 || !strings.Contains(workers[0].Suggested, "do not add") {
		t.Fatalf("worker advice under throttling = %+v, want a recommendation against", workers)
	}
}

// One job holds a read stream and a write stream at once, so the useful number
// of jobs is the SMALLER of the two knees. Sizing to the faster side queues
// work in front of the slower one and calls it concurrency.
func TestJobConcurrencyFollowsTheSlowerHalf(t *testing.T) {
	rep := Report{
		Source: SideReport{Knee: 16, Levels: []LevelResult{{Concurrency: 16, Rate: 9e6}},
			Best: LevelResult{Concurrency: 16, Rate: 9e6}},
		Target: SideReport{Knee: 4, Levels: []LevelResult{{Concurrency: 4, Rate: 3e6}},
			Best: LevelResult{Concurrency: 4, Rate: 3e6}},
	}

	got := jobConcurrencyAdvice(rep)
	if len(got) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(got))
	}
	if got[0].Suggested != "4" {
		t.Errorf("maxConcurrentJobs = %q, want the target's knee of 4", got[0].Suggested)
	}
}

// The ceiling is the slower half of the path, and the projection follows it.
func TestCeilingIsTheSlowerHalf(t *testing.T) {
	rep := Report{
		Source: SideReport{Levels: []LevelResult{{Concurrency: 1, Rate: 10 << 20}},
			Best: LevelResult{Concurrency: 1, Rate: 10 << 20}},
		Target: SideReport{Levels: []LevelResult{{Concurrency: 1, Rate: 2 << 20}},
			Best: LevelResult{Concurrency: 1, Rate: 2 << 20}},
	}

	rate, binding := effectiveCeiling(rep)
	if binding != "write" || rate != 2<<20 {
		t.Fatalf("ceiling = %v (%s), want the 2 MiB/s write side", rate, binding)
	}

	got := ceilingAdvice(rep, Options{BundleBytes: 30 << 30})
	if len(got) != 2 {
		t.Fatalf("got %d findings, want the ceiling and the projection", len(got))
	}
	// 30 GiB at 2 MiB/s is 15360 seconds, a bit over four hours.
	if !strings.Contains(got[1].Suggested, "4h") {
		t.Errorf("projection = %q, want about four hours", got[1].Suggested)
	}
}

// On a long link one stream leaves the line idle no matter how much bandwidth
// there is, and that is the fact that makes the concurrency advice make sense.
func TestLatencyAdviceExplainsWhyParallelismIsTheLever(t *testing.T) {
	side := SideReport{
		Name: "vendor", RTT: 200 * time.Millisecond,
		Levels: []LevelResult{
			{Concurrency: 1, Rate: 500 << 10},
			{Concurrency: 8, Rate: 4 << 20},
		},
		Best: LevelResult{Concurrency: 8, Rate: 4 << 20},
	}

	got := latencyAdvice(Report{Source: side})
	if len(got) != 1 {
		t.Fatalf("got %d findings on a 200ms link, want the bandwidth-delay explanation", len(got))
	}
	if !strings.Contains(got[0].Evidence, "200ms") {
		t.Errorf("evidence %q does not state the round trip it rests on", got[0].Evidence)
	}

	// A short link with a fast single stream needs no such explanation.
	quiet := side
	quiet.RTT = 2 * time.Millisecond
	if got := latencyAdvice(Report{Source: quiet}); len(got) != 0 {
		t.Errorf("bandwidth-delay advice offered for a 2ms link: %+v", got)
	}
}

func TestSuggestionsAreOrderedBySeverity(t *testing.T) {
	rep := Report{
		Source: SideReport{
			Name: "vendor",
			Route: RouteReport{
				ProxyInUse: true, DirectTested: true, DirectReachable: true,
				ProxiedRate: 1 << 20, DirectRate: 16 << 20,
			},
			Levels: []LevelResult{{Concurrency: 4, Rate: 2e6}},
			Best:   LevelResult{Concurrency: 4, Rate: 2e6}, Knee: 4,
		},
	}
	src := endpoint{name: "vendor", role: product.RoleSource,
		limits: product.Concurrency{PerRegistry: 4}}
	tgt := endpoint{name: "lab", role: product.RoleTarget}

	got := advise(rep, src, tgt, Options{})
	if len(got) == 0 {
		t.Fatal("no suggestions")
	}
	if got[0].Severity != SeverityCritical {
		t.Errorf("first suggestion is %s; the critical proxy finding must lead",
			got[0].Severity)
	}

	seen := map[Severity]bool{}
	last := SeverityCritical
	for _, s := range got {
		if seen[last] && s.Severity != last && seen[s.Severity] {
			t.Errorf("severity %s reappears after %s: the list is not grouped",
				s.Severity, last)
		}
		seen[s.Severity] = true
		last = s.Severity
	}
}
