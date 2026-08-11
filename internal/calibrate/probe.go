package calibrate

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// The transport a probe uses, and the two things measured before any bytes
// move: the route, and the latency over it.

// transportConfig builds the probe's transport from a resolved client config.
//
// # What it keeps and what it overrides
//
// It keeps the CA bundle, the proxy, the credentials and — importantly —
// requestsPerSecond. That last one is a PROMISE TO A VENDOR about how hard
// their registry will be worked, and a calibration that quietly broke it would
// measure a throughput no honest configuration could reproduce.
//
// It overrides maxConnections, because that is the variable under test: a
// sweep to sixteen streams against a pool configured for four would measure the
// pool four times and call it a plateau.
//
// The retry policy is short and shallow, as in preflight. A production
// schedule spends minutes in backoff against a failing endpoint, and a
// throughput sample that includes two minutes of sleeping is not a throughput
// sample.
func transportConfig(cfg registry.ClientConfig) transport.Config {
	return transport.Config{
		Registry:              cfg.Registry,
		Repository:            cfg.Repository,
		Username:              cfg.Username,
		Password:              cfg.Password,
		RequestsPerSecond:     cfg.RequestsPerSecond,
		Burst:                 cfg.Burst,
		MaxConnections:        cfg.MaxConnections,
		CABundle:              cfg.CABundle,
		HTTPSProxy:            cfg.HTTPSProxy,
		NoProxy:               cfg.NoProxy,
		DirectConnect:         cfg.DirectConnect,
		InsecureSkipVerify:    cfg.InsecureSkipVerify,
		ConnectTimeout:        cfg.ConnectTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		UserAgent:             "softwaregateway-calibrate",
		// The same HTTP/1.1 forcing the data plane uses. Measuring over h2
		// would measure a protocol no transfer speaks (docs/design/05 §5).
		ForceHTTP1:       true,
		MaxRetryAttempts: 2,
		RetryBaseDelay:   200 * time.Millisecond,
		RetryMaxDelay:    time.Second,
	}
}

// withConnections sizes the pool for one sweep level.
func withConnections(cfg registry.ClientConfig, streams int) registry.ClientConfig {
	out := cfg
	out.MaxConnections = streams
	return out
}

// withDirect returns the same config routed around any proxy.
func withDirect(cfg registry.ClientConfig) registry.ClientConfig {
	out := cfg
	out.DirectConnect = true
	out.HTTPSProxy = ""
	out.NoProxy = nil
	return out
}

// repositoryClient builds a client for one probe.
//
// Deliberately NOT shared between levels. A shared connection pool would carry
// warm connections from the previous level into the next, so level 8 would
// start with level 4's sockets already open and measure something the first
// level never got.
func repositoryClient(cfg registry.ClientConfig) (*generic.Repository, error) {
	return generic.New(generic.Config{
		Registry:   cfg.Registry,
		Repository: cfg.Repository,
		PlainHTTP:  cfg.PlainHTTP,
		Transport:  transportConfig(cfg),
	})
}

// describeRoute reports the route in force, before any probing.
func describeRoute(cfg registry.ClientConfig) RouteReport {
	described := transport.DescribeProxy(transportConfig(cfg))
	return RouteReport{
		Configured: described,
		ProxyInUse: proxyInUse(cfg),
	}
}

// proxyInUse reports whether requests will actually traverse a proxy —
// including one nobody configured here.
//
// The environment case is the whole reason this function exists. An unset
// httpsProxy reads as "no proxy" and means "whatever HTTPS_PROXY says", and a
// corporate proxy inherited that way has already cost one real transfer most of
// its bandwidth.
func proxyInUse(cfg registry.ClientConfig) bool {
	if cfg.DirectConnect {
		return false
	}
	if cfg.HTTPSProxy != "" {
		return true
	}
	probe := &http.Request{URL: requestURL(cfg)}
	u, err := http.ProxyFromEnvironment(probe)
	return err == nil && u != nil
}

// measureRTT times /v2/ several times and keeps the fastest.
//
// The minimum, not the mean: it is the closest available estimate of the
// path's intrinsic round trip, with scheduling and queueing noise removed. It
// matters because a single TCP stream cannot exceed its window divided by the
// round trip, and that ratio is what decides whether parallelism will help.
func measureRTT(ctx context.Context, cfg registry.ClientConfig) time.Duration {
	client, err := repositoryClient(withConnections(cfg, 1))
	if err != nil {
		return 0
	}

	best := time.Duration(0)
	for range 3 {
		if ctx.Err() != nil {
			break
		}
		started := time.Now()
		if err := client.Ping(ctx); err != nil {
			continue
		}
		if d := time.Since(started); best == 0 || d < best {
			best = d
		}
	}
	return best
}

// compareRoutes decides which route the sweep should run on.
//
// # Why this is measured rather than assumed
//
// "Bypass the proxy" is the advice that turned out to be worth the most on the
// first real transfer, and it is also the advice most likely to be wrong: on
// plenty of networks the proxy is the ONLY route out, and going direct fails
// entirely rather than slowly. Both facts are cheap to establish — one probe
// for reachability, one short throughput sample for the difference — and
// guessing either is how a recommendation becomes an outage.
//
// It returns the config the sweep should use, so the sweep measures the best
// route available rather than a route the report is about to recommend
// abandoning.
func compareRoutes(
	ctx context.Context, cfg registry.ClientConfig, route RouteReport,
	opts Options, run probe,
) (registry.ClientConfig, RouteReport) {
	if !route.ProxyInUse {
		return cfg, route
	}

	direct := withDirect(cfg)
	route.DirectTested = true

	client, err := repositoryClient(withConnections(direct, 1))
	if err != nil {
		route.DirectDetail = err.Error()
		return cfg, route
	}
	if err := client.Ping(ctx); err != nil {
		// The proxy is the only way out. That is the answer, and it is worth
		// as much as a throughput number: it stops somebody from setting
		// proxy.direct on the strength of a benchmark and losing connectivity.
		route.DirectDetail = err.Error()
		return cfg, route
	}
	route.DirectReachable = true

	// One level, on both routes, back to back. A full sweep on each would
	// double the run for a comparison that only needs to be directional.
	level := routeCompareLevel(opts)
	route.ProxiedRate = run(ctx, withConnections(cfg, level), level, opts.Budget).Rate
	route.DirectRate = run(ctx, withConnections(direct, level), level, opts.Budget).Rate

	if route.DirectRate > route.ProxiedRate {
		return direct, route
	}
	return cfg, route
}

// routeCompareLevel is the concurrency the head-to-head runs at.
//
// The middle of the sweep rather than one stream: a proxy's cost often shows
// up as a per-connection ceiling, which a single stream cannot see, and as
// contention for a shared pool, which only appears with several.
func routeCompareLevel(opts Options) int {
	if len(opts.Levels) == 0 {
		return 4
	}
	return opts.Levels[len(opts.Levels)/2]
}

// workingTime is how long the level actually spent moving bytes.
//
// Capped at the budget, because every stream is bounded by the run context and
// anything past it is teardown — cancelling upload sessions, closing bodies.
// Counting that as measurement time would report a rate a few percent below the
// truth, and the error would grow with the number of streams, which is exactly
// the axis the sweep is trying to read.
func workingTime(started time.Time, budget time.Duration) time.Duration {
	if d := time.Since(started); d < budget {
		return d
	}
	return budget
}

// median of durations, for TTFB. Returns zero for an empty sample.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// requestURL builds the URL a proxy decision would be made against.
func requestURL(cfg registry.ClientConfig) *url.URL {
	scheme := "https"
	if cfg.PlainHTTP {
		scheme = "http"
	}
	return &url.URL{Scheme: scheme, Host: cfg.Registry}
}
