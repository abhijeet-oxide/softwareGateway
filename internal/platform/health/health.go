// Package health implements the three-tier probe model.
//
// See docs/design/09-api.md section 9.1.
//
// The distinction this package exists to enforce:
//
//	Liveness  (/healthz)  - is this process wedged?     PROCESS-LOCAL ONLY.
//	Readiness (/readyz)   - should it receive traffic?  DB + config.
//	Deep      (:healthCheck) - diagnostics for a human. Everything.
//
// Liveness has no way to reach a dependency: Liveness accepts only func() error
// with no context and no arguments, so there is nothing to make a network call
// with. This is deliberate. A liveness probe that touched Postgres would
// restart every Coordinator during a brief database blip, converting a
// recoverable dependency hiccup into a fleet-wide crash-loop at exactly the
// moment the process needs to stay alive and retry.
package health

import (
	"context"
	"sync"
	"time"
)

// Status is a check outcome.
type Status string

const (
	StatusHealthy  Status = "HEALTHY"
	StatusDegraded Status = "DEGRADED"
	StatusDown     Status = "DOWN"
)

// Result is one dependency's outcome.
type Result struct {
	Name    string        `json:"name"`
	Status  Status        `json:"status"`
	Latency time.Duration `json:"-"`
	Detail  string        `json:"detail,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// LatencyMillis renders latency for JSON without exposing a Duration.
func (r Result) LatencyMillis() float64 { return float64(r.Latency.Microseconds()) / 1000.0 }

// Check is a dependency probe. Used for readiness and deep checks only.
type Check func(context.Context) Result

// livenessProbe deliberately takes no context and no arguments: there is
// nothing to perform I/O with, so a future edit cannot turn liveness into a
// dependency check without changing this type and failing review loudly.
type livenessProbe func() error

// Registry holds the checks for one process.
type Registry struct {
	mu        sync.RWMutex
	liveness  []namedLiveness
	readiness []namedCheck
	deep      []namedCheck
}

type namedLiveness struct {
	name  string
	probe livenessProbe
}

type namedCheck struct {
	name  string
	check Check
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

// AddLiveness registers a process-local probe. The probe signature makes
// network access impossible by construction.
func (r *Registry) AddLiveness(name string, p livenessProbe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liveness = append(r.liveness, namedLiveness{name, p})
}

// AddReadiness registers a check gating traffic: the database and
// configuration. Keep this set small - readiness runs on every probe interval.
func (r *Registry) AddReadiness(name string, c Check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readiness = append(r.readiness, namedCheck{name, c})
	// Everything that gates readiness also belongs in the deep check.
	r.deep = append(r.deep, namedCheck{name, c})
}

// AddDeep registers a diagnostic-only check: registries, SMTP, Teams. These
// may be slow and may fail without affecting whether we serve traffic, which
// is precisely why they live at a third endpoint.
func (r *Registry) AddDeep(name string, c Check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deep = append(r.deep, namedCheck{name, c})
}

// Live runs the liveness probes. Returns the first failure, or nil.
func (r *Registry) Live() error {
	r.mu.RLock()
	probes := append([]namedLiveness(nil), r.liveness...)
	r.mu.RUnlock()

	for _, p := range probes {
		if err := p.probe(); err != nil {
			return err
		}
	}
	return nil
}

// Report aggregates a set of checks.
type Report struct {
	Status  Status   `json:"status"`
	Results []Result `json:"checks"`
}

// Ready runs the readiness checks.
func (r *Registry) Ready(ctx context.Context) Report {
	r.mu.RLock()
	checks := append([]namedCheck(nil), r.readiness...)
	r.mu.RUnlock()
	return run(ctx, checks)
}

// Deep runs every check, including slow diagnostic ones. Backs
// `transferctl health`.
func (r *Registry) Deep(ctx context.Context) Report {
	r.mu.RLock()
	checks := append([]namedCheck(nil), r.deep...)
	r.mu.RUnlock()
	return run(ctx, checks)
}

// run executes checks concurrently - a deep check across a dozen registries
// should take as long as the slowest one, not the sum.
func run(ctx context.Context, checks []namedCheck) Report {
	results := make([]Result, len(checks))

	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c namedCheck) {
			defer wg.Done()
			start := time.Now()
			res := c.check(ctx)
			res.Name = c.name
			if res.Latency == 0 {
				res.Latency = time.Since(start)
			}
			results[i] = res
		}(i, c)
	}
	wg.Wait()

	return Report{Status: aggregate(results), Results: results}
}

// aggregate reduces results to the worst status present.
func aggregate(results []Result) Status {
	worst := StatusHealthy
	for _, r := range results {
		switch r.Status {
		case StatusDown:
			return StatusDown
		case StatusDegraded:
			worst = StatusDegraded
		case StatusHealthy:
		}
	}
	return worst
}

// OK is a convenience for a passing check.
func OK(detail string) Result { return Result{Status: StatusHealthy, Detail: detail} }

// Down is a convenience for a failing check.
func Down(err error) Result {
	r := Result{Status: StatusDown}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// Degraded is a convenience for a working-but-unhappy check.
func Degraded(detail string) Result { return Result{Status: StatusDegraded, Detail: detail} }
