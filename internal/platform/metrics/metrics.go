// Package metrics owns the Prometheus registry and the metric catalog.
//
// See docs/design/12-observability-and-audit.md section 2.
//
// Cardinality is the constraint that shapes this package. A Prometheus series
// exists per label-value combination, so a metric labelled by digest would
// create one series per blob — millions, permanently, and a dead Prometheus.
// Digests, transfer IDs and job IDs are traced and logged; they are NEVER
// metric labels. The only labels used here are bounded sets: product (tens),
// repository (hundreds), registry_type (4), outcome/state (single digits).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
)

const namespace = "softwaregateway"

// Registry holds every metric this process exposes.
type Registry struct {
	reg *prometheus.Registry

	// Build identity — the constant-1 gauge pattern, so a dashboard can
	// correlate a behaviour change with a deployment.
	BuildInfo *prometheus.GaugeVec

	// Configuration (section 2.6).
	ConfigProductsLoaded *prometheus.GaugeVec
	ConfigLoadErrors     *prometheus.GaugeVec
	ConfigLastReload     prometheus.Gauge

	// Coordinator.
	LeaderElected prometheus.Gauge

	// API.
	APIRequests *prometheus.CounterVec
	APILatency  *prometheus.HistogramVec

	// Discovery (docs/design/07 §7, docs/design/12 §2.3).
	// Delegated replication (docs/design/12 §2.6.1). Note what is NOT here:
	// any byte or throughput metric for a delegated target. We do not move
	// those bytes and cannot count them, and a gauge that looked like
	// throughput but was derived from elapsed time would be worse than the
	// absence of one.
	MirrorSyncs        *prometheus.CounterVec
	MirrorSyncDuration *prometheus.HistogramVec
	MirrorConfigDrift  *prometheus.GaugeVec
	ProxyCacheProbes   *prometheus.CounterVec

	DiscoveryScans       *prometheus.CounterVec
	DiscoveryErrors      *prometheus.CounterVec
	DiscoveryPackages    *prometheus.CounterVec
	DiscoveryDuration    *prometheus.HistogramVec
	DiscoveryLastSuccess *prometheus.GaugeVec

	// Manifest cache. Gauges rather than counters, because the question they
	// answer is "how big is it right now, and is the budget doing anything" —
	// not "how much churn has there been".
	ManifestCacheBytes     prometheus.Gauge
	ManifestCacheManifests prometheus.Gauge
	ManifestCacheEvicted   *prometheus.CounterVec
}

// New builds the registry for a component and registers the Go runtime and
// process collectors alongside our own.
func New(component string) *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Registry{
		reg: reg,

		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Build identity. Always 1; the information is in the labels.",
		}, []string{"version", "commit", "go_version", "component"}),

		ConfigProductsLoaded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "config_products_loaded",
			Help:      "Products currently valid and loaded.",
		}, []string{}),

		// Worth alerting on: a product whose config fails validation keeps
		// running on its previous valid version, which is correct behaviour
		// and also means a broken edit can go unnoticed indefinitely.
		ConfigLoadErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "config_load_errors",
			Help:      "Products whose most recent configuration failed validation.",
		}, []string{"product"}),

		ConfigLastReload: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "config_last_reload_timestamp_seconds",
			Help:      "Unix time of the last successful configuration reload.",
		}),

		LeaderElected: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "leader_elected",
			Help:      "1 on the leader replica, 0 on followers.",
		}),

		// `route` is the TEMPLATE (/api/v1/products/{product}), never the
		// populated path — otherwise every product name becomes a series.
		APIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "api_requests_total",
			Help:      "API requests by route template, method and status class.",
		}, []string{"route", "method", "status_class"}),

		APILatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "api_request_duration_seconds",
			Help:      "API request latency by route template and method.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"route", "method"}),

		MirrorSyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "mirror_sync_total",
			Help:      "Observed registry mirror syncs by product, target and result.",
		}, []string{"product", "target", "result"}),

		MirrorSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "mirror_sync_duration_seconds",
			Help:      "How long a mirror sync took, as OBSERVED between our request and the registry reporting it done. Not a measurement of the registry's own work.",
			// Minutes to hours, not the sub-second buckets a request would
			// want: a mirror sync of a 45 GB release is not a fast operation
			// and DefBuckets would put every observation in the overflow.
			Buckets: []float64{30, 60, 300, 900, 1800, 3600, 7200, 21600, 43200},
		}, []string{"product", "target"}),

		MirrorConfigDrift: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "mirror_config_drift",
			Help:      "1 when a target's registry configuration differs from what Git says.",
		}, []string{"product", "target"}),

		ProxyCacheProbes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "proxy_cache_probe_total",
			Help:      "Proxy-cache reachability probes by product, target and result.",
		}, []string{"product", "target", "result"}),

		DiscoveryScans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "discovery_scans_total",
			Help:      "Discovery scans by product, source and outcome.",
		}, []string{"product", "source", "outcome"}),

		DiscoveryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "discovery_errors_total",
			Help:      "Discovery failures by product, source and error class.",
		}, []string{"product", "source", "class"}),

		DiscoveryPackages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "discovery_packages_total",
			Help:      "Packages recorded by discovery, by product and source.",
		}, []string{"product", "source"}),

		DiscoveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "discovery_scan_duration_seconds",
			Help:      "Full-scan duration by product and source.",
			// A scan is one HEAD per tag, so it scales with tag count rather
			// than with bytes: seconds, not milliseconds, and the long tail is
			// the interesting part. DefBuckets tops out at 10s and would put
			// every slow vendor in one bucket.
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"product", "source"}),

		// THE metric to alert on, and the reason it is a timestamp rather than
		// a counter: the dangerous failure mode is not "discovery is erroring
		// loudly" but "discovery quietly stopped finding anything". Alert on
		// staleness of this gauge, not on error rate.
		DiscoveryLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "discovery_last_success_timestamp_seconds",
			Help:      "Unix time of the last successful scan, by product and source.",
		}, []string{"product", "source"}),

		ManifestCacheBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "manifest_cache_bytes",
			Help:      "Manifest bodies currently cached, in bytes.",
		}),

		ManifestCacheManifests: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "manifest_cache_manifests",
			Help:      "Manifest bodies currently cached.",
		}),

		// `reason` is `expired` or `budget`, which is the distinction worth
		// alerting on: steady expiry is the cache working, sustained budget
		// eviction means the budget is smaller than the working set and every
		// transfer is paying to re-fetch.
		ManifestCacheEvicted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "manifest_cache_evicted_total",
			Help:      "Manifest bodies reclaimed, by reason.",
		}, []string{"reason"}),
	}

	reg.MustRegister(
		m.BuildInfo,
		m.ConfigProductsLoaded,
		m.ConfigLoadErrors,
		m.ConfigLastReload,
		m.LeaderElected,
		m.APIRequests,
		m.APILatency,
		m.MirrorSyncs,
		m.MirrorSyncDuration,
		m.MirrorConfigDrift,
		m.ProxyCacheProbes,
		m.DiscoveryScans,
		m.DiscoveryErrors,
		m.DiscoveryPackages,
		m.DiscoveryDuration,
		m.DiscoveryLastSuccess,
		m.ManifestCacheBytes,
		m.ManifestCacheManifests,
		m.ManifestCacheEvicted,
	)

	info := version.Get(component)
	m.BuildInfo.WithLabelValues(info.Version, info.Commit, info.GoVersion, info.Component).Set(1)

	return m
}

// Prometheus exposes the underlying registry for the /metrics handler.
func (m *Registry) Prometheus() *prometheus.Registry { return m.reg }

// StatusClass buckets an HTTP status into a bounded label value.
// Returning the raw status would give ~40 series per route; this gives 5.
func StatusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
