package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/querycount"
)

// THE ROUND TRIPS A REQUEST MADE REACH THE METRIC.
//
// This is the number that makes an N+1 visible in a running deployment, where
// latency cannot: on a small database twenty-five extra round trips cost
// forty milliseconds and nothing looks wrong. Two listings in this application
// shipped that way.
//
// The counter is installed by the LOGGING middleware, which wraps the metrics
// one, so both report the same request - and a wiring change that left the
// metrics middleware installing its own would silently report zero for every
// route. Hence a test that drives the pair together rather than either alone.
func TestQueriesPerRequestReachTheMetric(t *testing.T) {
	reg := metrics.New("test")

	const queries = 7
	handler := chainForTest(reg, http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			// Stand in for the driver wrapper, which records against whatever
			// counter the request's context carries.
			for range queries {
				querycount.Record(r.Context())
			}
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/things", nil))

	got := histogramSum(t, reg, "softwaregateway_api_request_queries")
	if got != queries {
		t.Errorf("the metric recorded %v round trips, want %d - the counter the "+
			"logging middleware installs is not the one the metrics middleware "+
			"reads", got, queries)
	}
}

// A request that makes no queries records a zero rather than nothing.
//
// A route absent from the histogram and a route that queries nothing look the
// same on a dashboard, and they are not the same thing: the first is a route
// nobody has exercised and the second is a route that is doing no database
// work at all.
func TestARequestThatQueriesNothingIsStillRecorded(t *testing.T) {
	reg := metrics.New("test")
	handler := chainForTest(reg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/things", nil))

	if count := histogramCount(t, reg, "softwaregateway_api_request_queries"); count != 1 {
		t.Errorf("observations = %v, want 1 - a request that queries nothing "+
			"must still appear", count)
	}
}

// THE LATENCY BUCKETS REACH FAR ENOUGH TO SEE THE FAILURES WE ACTUALLY HAD.
//
// prometheus.DefBuckets stops at ten seconds. A deployment reported a listing
// taking five MINUTES and the histogram could say only "over ten seconds":
// every one of those requests fell in +Inf, so every quantile above that
// bucket was extrapolation rather than measurement.
func TestTheLatencyBucketsCoverASlowRequest(t *testing.T) {
	reg := metrics.New("test")
	handler := chainForTest(reg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/v1/things", nil))

	families, err := reg.Prometheus().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var top float64
	for _, f := range families {
		if f.GetName() != "softwaregateway_api_request_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				top = max(top, b.GetUpperBound())
			}
		}
	}
	// Five minutes, because that is the length of the slowest request anybody
	// has reported against this service.
	if top < 300 {
		t.Errorf("the slowest latency bucket is %vs; a request slower than that "+
			"lands in +Inf and its quantile is extrapolation, not measurement", top)
	}
}

// chainForTest is the logging and metrics pair in the order the router uses -
// logging OUTSIDE metrics, which is what makes one counter serve both.
func chainForTest(reg *metrics.Registry, h http.Handler) http.Handler {
	// A chi router, because the metrics middleware labels by ROUTE TEMPLATE
	// and there is no template without one.
	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(Logging(slog.New(slog.DiscardHandler)))
	r.Use(Metrics(reg))
	r.Method(http.MethodGet, "/api/v1/things", h)
	return r
}

func histogramSum(t *testing.T, reg *metrics.Registry, name string) float64 {
	t.Helper()
	return histogramField(t, reg, name, func(h *histogramView) float64 { return h.sum })
}

func histogramCount(t *testing.T, reg *metrics.Registry, name string) float64 {
	t.Helper()
	return histogramField(t, reg, name, func(h *histogramView) float64 { return h.count })
}

type histogramView struct{ sum, count float64 }

func histogramField(
	t *testing.T, reg *metrics.Registry, name string, pick func(*histogramView) float64,
) float64 {
	t.Helper()
	families, err := reg.Prometheus().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			h := m.GetHistogram()
			return pick(&histogramView{sum: h.GetSampleSum(), count: float64(h.GetSampleCount())})
		}
	}
	t.Fatalf("no metric named %s; gathered %s", name, strings.Join(familyNames(families), ", "))
	return 0
}

func familyNames(families []*io_prometheus_client.MetricFamily) []string {
	out := make([]string, 0, len(families))
	for _, f := range families {
		out = append(out, f.GetName())
	}
	return out
}
