package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// TestQueueGaugesDropLabelsThatHaveEmptied is the bug a gauge vector invites.
//
// prometheus.GaugeVec REMEMBERS every label combination it has ever been given.
// Set workers{state="active"} to 1 and then stop setting it, and it keeps
// reporting 1 forever - so the last worker to drain leaves a dashboard showing
// a worker that no longer exists, and an alert on "no active workers" never
// fires. Reset before the writes is what makes a gauge read the present.
func TestQueueGaugesDropLabelsThatHaveEmptied(t *testing.T) {
	m := metrics.New("coordinator")

	observeQueue(m, store.QueueSnapshot{
		Jobs:      map[string]int64{"pending": 7, "leased": 2},
		Workers:   map[string]int64{"active": 3, "stale": 1},
		Transfers: map[string]int64{"running": 1},
	})
	if body := gather(t, m); !strings.Contains(body, `workers{state="active"} 3`) {
		t.Fatalf("the first sample did not reach the gauges:\n%s", body)
	}

	// The fleet drains and the queue empties.
	observeQueue(m, store.QueueSnapshot{
		Jobs:      map[string]int64{"pending": 0},
		Workers:   map[string]int64{},
		Transfers: map[string]int64{},
	})

	body := gather(t, m)
	for _, stale := range []string{
		`workers{state="active"}`,
		`workers{state="stale"}`,
		`queue_jobs{state="leased"}`,
		`queue_transfers{state="running"}`,
	} {
		if strings.Contains(body, stale) {
			t.Errorf("%s is still being reported after the sample that no longer "+
				"contains it.\nA GaugeVec keeps every label it has ever seen, so a\n"+
				"drained fleet goes on reporting workers that do not exist. The write\n"+
				"path has to Reset before it Sets.", stale)
		}
	}
	if !strings.Contains(body, `queue_jobs{state="pending"} 0`) {
		t.Error("a state still present in the sample should be reported, even at zero")
	}
}

// TestQueueGaugesReportEveryDimensionOfASnapshot guards against a field being
// added to the snapshot and never reaching a gauge - which looks like an empty
// panel rather than like a bug.
func TestQueueGaugesReportEveryDimensionOfASnapshot(t *testing.T) {
	m := metrics.New("coordinator")
	observeQueue(m, store.QueueSnapshot{
		Jobs:                 map[string]int64{"pending": 5, "blocked": 1, "leased": 2},
		JobsPaused:           1,
		OldestPendingSeconds: 42,
		PendingBytes:         1 << 30,
		InFlightBytes:        1 << 20,
		Transfers:            map[string]int64{"running": 3},
		Workers:              map[string]int64{"active": 2},
		SlotsActive:          2,
		SlotsGranted:         6,
		SlotsMax:             16,
		DatabaseBytes:        123456,
	})

	body := gather(t, m)
	for _, want := range []string{
		`queue_jobs{state="pending"} 5`,
		`queue_jobs{state="blocked"} 1`,
		`queue_jobs{state="leased"} 2`,
		`queue_jobs_paused 1`,
		`queue_oldest_pending_seconds 42`,
		`queue_bytes{kind="pending"} 1.073741824e+09`,
		`queue_bytes{kind="in_flight"} 1.048576e+06`,
		`queue_transfers{state="running"} 3`,
		`workers{state="active"} 2`,
		`worker_slots{kind="active"} 2`,
		`worker_slots{kind="granted"} 6`,
		`worker_slots{kind="max"} 16`,
		`database_bytes 123456`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not contain %q.\n"+
				"A snapshot field that reaches no gauge is an empty dashboard panel,\n"+
				"which reads as 'nothing is happening' rather than as a missing wire.", want)
		}
	}
}

func gather(t *testing.T, m *metrics.Registry) string {
	t.Helper()
	families, err := m.Prometheus().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "softwaregateway_") {
			continue
		}
		for _, mm := range f.GetMetric() {
			var labels []string
			for _, l := range mm.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			name := strings.TrimPrefix(f.GetName(), "softwaregateway_")
			if len(labels) > 0 {
				name += "{" + strings.Join(labels, ",") + "}"
			}
			var v float64
			switch {
			case mm.Gauge != nil:
				v = mm.Gauge.GetValue()
			case mm.Counter != nil:
				v = mm.Counter.GetValue()
			default:
				continue
			}
			b.WriteString(name + " " + trimFloat(v) + "\n")
		}
	}
	return b.String()
}

// trimFloat renders a value the way the Prometheus text format does, so the
// assertions above can be written as the lines an operator would see.
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// TestASamplerTimeoutIsLogged is the bug a running deployment found.
//
// The sampler suppressed its warning when the context was done, meaning to
// stay quiet during shutdown. But the context it asked was the TIMEOUT it had
// just derived, so the one failure most worth knowing about - the sample could
// not get a database connection inside its budget - was the one it never
// mentioned. Eight of them happened before anybody noticed, and only the
// counter showed it.
func TestASamplerTimeoutIsLogged(t *testing.T) {
	var buf bytes.Buffer
	s := &queueSampler{
		metrics:  metrics.New("coordinator"),
		logger:   slog.New(slog.NewTextHandler(&buf, nil)),
		packages: nil, // never reached: the snapshot is stubbed below
		interval: time.Second,
	}

	// A sample whose context is already past its deadline, which is what a
	// contended pool produces.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s.observeFailure(expired, context.Background(), errors.New("context deadline exceeded"))

	if !strings.Contains(buf.String(), "queue sample failed") {
		t.Errorf("a sample that timed out logged nothing:\n%q\n\n"+
			"A timeout is the failure worth a line - it means the gauges are stale\n"+
			"because the sample could not get a connection. Only a shutdown should\n"+
			"be quiet.", buf.String())
	}

	// A shutdown stays quiet.
	buf.Reset()
	stopped, stop := context.WithCancel(context.Background())
	stop()
	s.observeFailure(stopped, stopped, errors.New("context canceled"))
	if buf.Len() != 0 {
		t.Errorf("shutting down logged a failure:\n%q", buf.String())
	}
}
