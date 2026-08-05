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
	}

	reg.MustRegister(
		m.BuildInfo,
		m.ConfigProductsLoaded,
		m.ConfigLoadErrors,
		m.ConfigLastReload,
		m.LeaderElected,
		m.APIRequests,
		m.APILatency,
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
