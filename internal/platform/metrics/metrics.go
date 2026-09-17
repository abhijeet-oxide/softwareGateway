// Package metrics owns the Prometheus registry and the metric catalog.
//
// See docs/design/12-observability-and-audit.md section 2.
//
// Cardinality is the constraint that shapes this package. A Prometheus series
// exists per label-value combination, so a metric labelled by digest would
// create one series per blob - millions, permanently, and a dead Prometheus.
// Digests, transfer IDs and job IDs are traced and logged; they are NEVER
// metric labels. The only labels used here are bounded sets: product (tens),
// repository (hundreds), registry_type (4), outcome/state (single digits).
package metrics

import (
	"database/sql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
)

const namespace = "softwaregateway"

// apiLatencyBuckets covers what this service actually does, which
// prometheus.DefBuckets does not.
//
// DefBuckets stops at ten seconds. A deployment reported a listing taking five
// MINUTES and the histogram could say only that it was over ten seconds: every
// such request fell in +Inf, so every quantile above that bucket was
// extrapolation rather than measurement, and the metric that should have
// screamed could only shrug.
//
// The fast end is kept dense because that is where a healthy read lives and
// where a regression first shows, and the slow end runs to five minutes
// because that is the shape of the failure worth catching - a registry that
// accepts a connection and never answers, or a page asking once per row.
var apiLatencyBuckets = []float64{
	.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300,
}

// Registry holds every metric this process exposes.
type Registry struct {
	reg *prometheus.Registry

	// Build identity - the constant-1 gauge pattern, so a dashboard can
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
	// APIQueries is round trips per request - see where it is built for why
	// latency alone cannot see an N+1.
	APIQueries *prometheus.HistogramVec
	// APIDBSeconds is the part of a request's latency spent waiting on the
	// database. Against APILatency it is the whole latency breakdown there is.
	APIDBSeconds *prometheus.HistogramVec
	// ActiveUsers is distinct people in a rolling window - see
	// internal/api/middleware.ActiveUsers for why it is a count and not a
	// label.
	ActiveUsers prometheus.Gauge
	// LogLines is what the log shipper did with each line, by outcome.
	LogLines *prometheus.CounterVec
	// logShipped is the last total seen per outcome, so ObserveLogShipping can
	// turn the shipper's cumulative counts into counter increments.
	logShipped map[string]int64

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
	// answer is "how big is it right now, and is the budget doing anything" -
	// not "how much churn has there been".
	ManifestCacheBytes     prometheus.Gauge
	ManifestCacheManifests prometheus.Gauge
	ManifestCacheEvicted   *prometheus.CounterVec

	// The queue, sampled from the database on a timer - see
	// store.QueueSnapshot for why these are read rather than counted, and
	// ObserveQueue for what drives them.
	//
	// This is the product's own work. Everything above measures the service
	// that fronts it; without these, a deployment where the API is fast and
	// nothing is being transferred looks perfectly healthy.
	QueueJobs           *prometheus.GaugeVec
	QueueJobsPaused     prometheus.Gauge
	QueueOldestPending  prometheus.Gauge
	QueueBytes          *prometheus.GaugeVec
	QueueTransfers      *prometheus.GaugeVec
	Workers             *prometheus.GaugeVec
	WorkerSlots         *prometheus.GaugeVec
	DatabaseBytes       prometheus.Gauge
	QueueSampleFailures prometheus.Counter
	QueueSampleDuration prometheus.Histogram

	// What the fleet actually did. The queue gauges above are the present;
	// these are the record, and they are counters because the rows they would
	// otherwise be read from are archived.
	JobsCompleted *prometheus.CounterVec
	JobDuration   *prometheus.HistogramVec
	JobBytes      *prometheus.CounterVec
	JobErrors     *prometheus.CounterVec
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
		reg:        reg,
		logShipped: map[string]int64{},

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
		// populated path - otherwise every product name becomes a series.
		APIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "api_requests_total",
			Help:      "API requests by route template, method and status class.",
		}, []string{"route", "method", "status_class"}),

		APILatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "api_request_duration_seconds",
			Help:      "API request latency by route template and method.",
			Buckets:   apiLatencyBuckets,
		}, []string{"route", "method"}),

		// HOW MANY DATABASE ROUND TRIPS ONE REQUEST MADE.
		//
		// This is the metric that makes an N+1 visible in production, and it
		// is here because latency could not: this repository shipped two
		// listings that asked the database once per row, and on a developer's
		// estate twenty-five extra round trips cost forty milliseconds and
		// nothing complained. On a real deployment they cost minutes.
		//
		// Time is data-dependent; a count is not. Twenty-five queries to draw
		// twenty-five rows is wrong at any size, and a histogram of this per
		// route says so the moment it ships rather than when somebody
		// eventually profiles it. The companion is
		// internal/api/apicost_test.go, which holds the same number in CI.
		APIQueries: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "api_request_queries",
			Help: "Database round trips made while serving one API request, " +
				"by route template. A route whose count grows with its page " +
				"size is asking once per row.",
			Buckets: []float64{0, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233},
		}, []string{"route", "method"}),

		// TIME IN THE DATABASE, per request.
		//
		// The count above says an endpoint is asking too often; this says
		// whether asking is what it is slow doing. Subtract it from
		// api_request_duration_seconds and what is left is the handler, the
		// serialisation and whatever upstream registry the route talks to -
		// and those are fixed in completely different places, so the split is
		// most of the work of deciding where to look.
		//
		// Same buckets as the latency histogram on purpose: the two are read
		// beside each other, and quantiles from different bucket boundaries
		// are not comparable.
		APIDBSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "api_request_db_seconds",
			Help: "Time one API request spent waiting on the database, by " +
				"route template. The rest of its latency is the handler.",
			Buckets: apiLatencyBuckets,
		}, []string{"route", "method"}),

		// THE ONE THAT NOTICES PEOPLE LEAVING.
		//
		// Nobody files a ticket about a slow page; they stop opening it.
		// Request rate cannot see that - one person refreshing a dashboard
		// outnumbers ten people doing a day's work - and this can: latency up
		// and this down, over the same week, is one event rather than two.
		ActiveUsers: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "api_active_users",
			Help:      "Distinct people who made a request in the last fifteen minutes.",
		}),

		// `outcome` is sent, dropped or failed.
		//
		// WITHOUT THIS, LOG LOSS IS INVISIBLE. A shipper that is dropping
		// looks exactly like a quiet service from inside the log store: the
		// lines that would have said otherwise are the ones that went
		// missing. `dropped` means the buffer was full, which is the service
		// logging faster than the store accepts; `failed` means the store
		// refused or could not be reached. Both leave stdout intact.
		LogLines: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "log_lines_total",
			Help:      "Log lines by what the shipper did with them.",
		}, []string{"outcome"}),

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

		// `state` is blocked/pending/leased. Settled jobs are NOT here: a
		// gauge of them would fall when rows are archived, which reads as
		// work being undone.
		QueueJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_jobs",
			Help:      "Jobs outstanding, by state.",
		}, []string{"state"}),

		QueueJobsPaused: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_jobs_paused",
			Help: "Outstanding jobs whose transfer is paused. A deep queue " +
				"that is paused is a decision; a deep queue that is not is an incident.",
		}),

		// THE ONE TO ALERT ON. Depth cannot tell a busy queue from a stuck
		// one - it is large in both - but this stays flat in a moving queue
		// however deep the backlog gets, and climbs in real time in a stalled
		// one. Alert on this, not on depth.
		QueueOldestPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_oldest_pending_seconds",
			Help:      "How long the oldest unstarted job has been waiting.",
		}),

		// `kind` is pending (planned size of what has not started) or
		// in_flight (what leased jobs have moved so far). Bytes rather than
		// job count because a queue of nine manifests and one 23 GB blob is
		// one job from done and hours from finished.
		QueueBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_bytes",
			Help:      "Bytes outstanding, by kind.",
		}, []string{"kind"}),

		QueueTransfers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_transfers",
			Help:      "Transfers not yet settled, by state.",
		}, []string{"state"}),

		// By state, never by worker id: a worker id is a pod name, and pod
		// names are unbounded over a cluster's life. See the package comment.
		Workers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "workers",
			Help:      "Workers known to the Coordinator, by state.",
		}, []string{"state"}),

		// `kind` is active/granted/max. Granted below max is the budget
		// controller holding back on a vendor registry; active at granted
		// with a deep queue is the fleet being the bottleneck. Those are
		// different problems and the gap between the lines says which.
		WorkerSlots: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "worker_slots",
			Help:      "Fleet concurrency across active workers, by kind.",
		}, []string{"kind"}),

		DatabaseBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "database_bytes",
			Help:      "What the database occupies on disk.",
		}),

		// Without these the queue gauges have a failure mode that looks like
		// good news: a sampler erroring every pass leaves the last values
		// frozen, and a frozen zero is indistinguishable from an empty queue.
		QueueSampleFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "queue_sample_failures_total",
			Help: "Queue samples that failed. Non-zero means every gauge " +
				"below is stale, not that the queue is empty.",
		}),

		QueueSampleDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "queue_sample_duration_seconds",
			Help: "How long one queue sample took. Watched because it runs " +
				"on a timer forever: if it grows with the table, the index " +
				"behind it has stopped being used.",
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5},
		}),

		// `outcome` is succeeded/skipped/failed/cancelled; `kind` is
		// blob/manifest. NOT labelled by product or repository: a completion
		// happens per blob, and this is the highest-frequency event in the
		// system - the two labels here are single digits each, and every
		// further one multiplies the series by the size of the estate.
		//
		// `failed` here counts TERMINAL failures. A job that failed and will
		// be retried is still outstanding and is counted by queue_jobs; the
		// retry itself is job_errors_total, below.
		JobsCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "jobs_completed_total",
			Help:      "Jobs that reached a terminal state, by kind and outcome.",
		}, []string{"kind", "outcome"}),

		// Seconds from first lease to completion, which for a blob is how long
		// it took to move. Buckets run to an hour: a 23 GB layer over a
		// congested WAN is not a fast operation, and DefBuckets would put every
		// one that matters in the overflow.
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "job_duration_seconds",
			Help:      "Time from a job's first lease to its completion, by kind.",
			Buckets: []float64{
				.1, .5, 1, 5, 15, 30, 60, 300, 900, 1800, 3600,
			},
		}, []string{"kind"}),

		// THROUGHPUT LIVES HERE. rate() of this is bytes per second actually
		// moved, which no gauge can give: queue_bytes falls as work drains and
		// rises as work is planned, so its slope is not a transfer rate.
		//
		// `disposition` separates bytes that crossed the wire from bytes a
		// dedupe or a server-side mount meant nobody had to move - which is
		// the number that justifies this system existing, and it would be
		// invisible if both were added together.
		JobBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "job_bytes_total",
			Help:      "Bytes accounted for by completed jobs, by disposition.",
		}, []string{"disposition"}),

		// Every failure, retried or not, by the class that decides how many
		// attempts it gets. The class is the actionable part: `auth` is a
		// credential nobody rotated, `transient` is a registry having a bad
		// day, `digest_mismatch` is corruption. An undifferentiated failure
		// rate cannot tell a person which of those they are looking at.
		JobErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "job_errors_total",
			Help:      "Job failures by error class, whether or not they were retried.",
		}, []string{"class", "kind"}),
	}

	reg.MustRegister(
		m.BuildInfo,
		m.ConfigProductsLoaded,
		m.ConfigLoadErrors,
		m.ConfigLastReload,
		m.LeaderElected,
		m.APIRequests,
		m.APILatency,
		m.APIQueries,
		m.APIDBSeconds,
		m.ActiveUsers,
		m.LogLines,
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
		m.QueueJobs,
		m.QueueJobsPaused,
		m.QueueOldestPending,
		m.QueueBytes,
		m.QueueTransfers,
		m.Workers,
		m.WorkerSlots,
		m.DatabaseBytes,
		m.QueueSampleFailures,
		m.QueueSampleDuration,
		m.JobsCompleted,
		m.JobDuration,
		m.JobBytes,
		m.JobErrors,
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

// ObserveLogShipping publishes what the log shipper has done.
//
// Counters rather than a collector, set from cumulative totals: the shipper
// counts with atomics on the logging path, where taking a Prometheus counter's
// lock per line would be the most expensive thing about logging.
//
// Set rather than added, because the shipper's numbers are already cumulative.
// A counter that only ever rises is what Prometheus needs, and re-deriving the
// delta here would be a second place to get it wrong.
func (m *Registry) ObserveLogShipping(sent, dropped, failed int64) {
	if m == nil {
		return
	}
	for outcome, n := range map[string]int64{
		"sent": sent, "dropped": dropped, "failed": failed,
	} {
		c := m.LogLines.WithLabelValues(outcome)
		// The counter is monotonic and so are the shipper's totals, so the
		// difference is what has happened since the last pass.
		if delta := n - m.logShipped[outcome]; delta > 0 {
			c.Add(float64(delta))
			m.logShipped[outcome] = n
		}
	}
}

// BindDatabase publishes the connection pool's own numbers.
//
// # Why a collector rather than a gauge somebody sets
//
// Because sql.DBStats is a SNAPSHOT the pool already keeps, and anything that
// copied it into a gauge on a timer would report the value as of the last tick
// - which is exactly wrong for saturation, the thing these exist to show. A
// collector reads them when Prometheus scrapes, so the numbers are the pool's
// own at that instant.
//
// # Why saturation is worth its own metrics
//
// A saturated pool is the difference between "one endpoint is slow" and
// "everything is slow". A request that cannot get a connection waits without
// doing any work, and that wait is charged to whatever route it happened to
// be serving - so a single expensive query elsewhere reads as every page
// being slow, and the route labels point at the victims rather than the
// cause. `db_connection_wait_seconds_total` climbing while `db_connections`
// sits at max is that, and nothing else looks like it.
//
// Safe to call with a nil registry or a nil stats function, which is what a
// Coordinator without a database does.
func (m *Registry) BindDatabase(stats func() sql.DBStats) {
	if m == nil || m.reg == nil || stats == nil {
		return
	}
	m.reg.MustRegister(&dbCollector{stats: stats})
}

type dbCollector struct{ stats func() sql.DBStats }

var (
	dbInUse = prometheus.NewDesc(
		namespace+"_db_connections",
		"Database pool connections by state.", []string{"state"}, nil)
	dbWaitCount = prometheus.NewDesc(
		namespace+"_db_connection_waits_total",
		"Times a caller had to wait for a database connection.", nil, nil)
	dbWaitSeconds = prometheus.NewDesc(
		namespace+"_db_connection_wait_seconds_total",
		"Time callers have spent waiting for a database connection.", nil, nil)
)

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- dbInUse
	ch <- dbWaitCount
	ch <- dbWaitSeconds
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stats()
	for state, v := range map[string]float64{
		"in_use": float64(s.InUse),
		"idle":   float64(s.Idle),
		"open":   float64(s.OpenConnections),
		"max":    float64(s.MaxOpenConnections),
	} {
		ch <- prometheus.MustNewConstMetric(dbInUse, prometheus.GaugeValue, v, state)
	}
	ch <- prometheus.MustNewConstMetric(dbWaitCount,
		prometheus.CounterValue, float64(s.WaitCount))
	ch <- prometheus.MustNewConstMetric(dbWaitSeconds,
		prometheus.CounterValue, s.WaitDuration.Seconds())
}
