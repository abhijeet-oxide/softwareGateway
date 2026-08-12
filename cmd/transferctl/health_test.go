package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// "Are the workers up, and what are they doing" is the first question of any
// incident, and `health` could not answer it: the workers table was created in
// the first migration and nothing ever wrote to it.

func healthWithFleet() *v1.HealthCheckResponse {
	now := time.Now()
	return &v1.HealthCheckResponse{
		Status: v1.HealthHealthy, Version: "1.4.0", Leader: true,
		Checks: []v1.HealthCheck{
			{Name: "database", Status: v1.HealthHealthy, LatencyMs: 1.2, Detail: "sqlite"},
		},
		Workers: []v1.Worker{
			{WorkerID: "INBLR1761", Version: "1.4.0", MaxConcurrency: 32,
				ActiveJobs: 1, LeasedJobs: 1, State: "ACTIVE",
				LastHeartbeat: now.Add(-4 * time.Second).Format(time.RFC3339Nano)},
			{WorkerID: "old-pod-7c9", Version: "1.3.0", MaxConcurrency: 16,
				ActiveJobs: 4, LeasedJobs: 0, State: "STALE",
				LastHeartbeat: now.Add(-31 * time.Minute).Format(time.RFC3339Nano)},
		},
	}
}

func TestHealthShowsTheFleetAndItsLoad(t *testing.T) {
	var out bytes.Buffer
	if err := renderHealthTable(&out, healthWithFleet()); err != nil {
		t.Fatal(err)
	}
	text := out.String()

	for _, want := range []string{
		"WORKER", "INBLR1761",
		"1/32",  // the load against the CONFIGURED ceiling, not a remainder
		"1.4.0", // the build each worker is running
		"STALE", // and the one that stopped talking
	} {
		if !strings.Contains(text, want) {
			t.Errorf("health does not show %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "returned to the queue") {
		t.Errorf("a stale worker does not say what follows from it:\n%s", text)
	}
}

// A Coordinator no worker has ever leased from says so, rather than printing an
// empty table that reads as "no data" — which is a different thing from "no
// workers".
func TestHealthSaysWhenNoWorkerHasEverLeased(t *testing.T) {
	resp := healthWithFleet()
	resp.Workers = nil

	var out bytes.Buffer
	if err := renderHealthTable(&out, resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no worker has ever leased") {
		t.Errorf("an empty fleet is not explained:\n%s", out.String())
	}
}

// The load column is the one somebody checks against their config file, so a
// worker whose ceiling was never recorded must not render as though it had one.
func TestWorkerLoadShowsTheConfiguredCeiling(t *testing.T) {
	if got := workerLoad(v1.Worker{LeasedJobs: 1, MaxConcurrency: 32}); got != "1/32" {
		t.Errorf("load = %q, want 1/32", got)
	}
	if got := workerLoad(v1.Worker{LeasedJobs: 2}); got != "2/?" {
		t.Errorf("load = %q, want the unknown ceiling shown as unknown", got)
	}
}

// The worker's own count against the Coordinator's. They diverge when leases
// were reaped underneath a worker that is still working on them, and that is
// worth a sentence rather than two numbers the reader has to compare.
func TestADisagreementAboutJobCountsIsCalledOut(t *testing.T) {
	got := workerDetail(v1.Worker{State: "ACTIVE", ActiveJobs: 4, LeasedJobs: 1})
	if !strings.Contains(got, "4 running") || !strings.Contains(got, "1 leased") {
		t.Errorf("detail = %q, want both counts named", got)
	}
}
