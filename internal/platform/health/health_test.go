package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLivenessCannotReachDependencies(t *testing.T) {
	// This test documents a compile-time property rather than a runtime one.
	// AddLiveness accepts func() error - no context, no arguments - so a
	// liveness probe has nothing to make a network call with. If someone
	// widens that signature, this test's comment is the reason not to.
	//
	// A liveness probe that checked Postgres would restart every Coordinator
	// during a database blip. See docs/design/09-api.md section 9.1.
	r := New()
	r.AddLiveness("loop", func() error { return nil })
	if err := r.Live(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestLiveReturnsFirstFailure(t *testing.T) {
	sentinel := errors.New("main loop stalled")
	r := New()
	r.AddLiveness("ok", func() error { return nil })
	r.AddLiveness("stalled", func() error { return sentinel })
	r.AddLiveness("never-reached", func() error { t.Fatal("should not run"); return nil })

	if err := r.Live(); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %v", err, sentinel)
	}
}

func TestReadinessDoesNotIncludeDeepChecks(t *testing.T) {
	// A slow registry probe must not be able to fail readiness and pull the
	// Coordinator out of rotation.
	r := New()
	r.AddReadiness("database", func(context.Context) Result { return OK("connected") })
	r.AddDeep("vendor-registry", func(context.Context) Result { return Down(errors.New("timeout")) })

	ready := r.Ready(context.Background())
	if ready.Status != StatusHealthy {
		t.Fatalf("readiness must ignore deep checks, got %s", ready.Status)
	}
	if len(ready.Results) != 1 {
		t.Fatalf("expected 1 readiness check, got %d", len(ready.Results))
	}

	deep := r.Deep(context.Background())
	if deep.Status != StatusDown {
		t.Fatalf("deep check must surface the failure, got %s", deep.Status)
	}
	if len(deep.Results) != 2 {
		t.Fatalf("deep must include readiness checks too, got %d", len(deep.Results))
	}
}

func TestAggregateWorstWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Result
		want Status
	}{
		{"empty is healthy", nil, StatusHealthy},
		{"all healthy", []Result{{Status: StatusHealthy}, {Status: StatusHealthy}}, StatusHealthy},
		{"one degraded", []Result{{Status: StatusHealthy}, {Status: StatusDegraded}}, StatusDegraded},
		{"down beats degraded", []Result{{Status: StatusDegraded}, {Status: StatusDown}}, StatusDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregate(tc.in); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestChecksRunConcurrently(t *testing.T) {
	// A deep check across many registries should cost the slowest, not the sum.
	const n = 8
	const delay = 50 * time.Millisecond

	r := New()
	for i := 0; i < n; i++ {
		r.AddDeep("slow", func(context.Context) Result {
			time.Sleep(delay)
			return OK("")
		})
	}

	start := time.Now()
	r.Deep(context.Background())
	elapsed := time.Since(start)

	if elapsed > delay*3 {
		t.Fatalf("checks appear serial: %v elapsed for %d checks of %v", elapsed, n, delay)
	}
}

func TestResultNameIsSetFromRegistration(t *testing.T) {
	r := New()
	r.AddReadiness("database", func(context.Context) Result { return OK("connected") })

	rep := r.Ready(context.Background())
	if rep.Results[0].Name != "database" {
		t.Fatalf("got name %q, want database", rep.Results[0].Name)
	}
	if rep.Results[0].Latency == 0 {
		t.Fatal("latency should be measured when the check does not set it")
	}
}
