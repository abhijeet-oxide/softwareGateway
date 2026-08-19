package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A calibration report is read once, under time pressure, by somebody deciding
// whether to change a production setting. These assert the two things that
// decision needs: the recommended level is findable in the table, and the
// reason for every suggestion is next to it.

func sampleCalibration() *v1.CalibrateResponse {
	return &v1.CalibrateResponse{
		Product: "cfx-5000-product", MeasuredFrom: "ops-laptop", DurationSec: 96,
		Source: v1.CalibrationSide{
			Role: "source", Name: "near", Registry: "near.registry.example.net",
			Repository: "orbs/cfx-5000-k8s",
			Route: v1.CalibrationRoute{
				Configured: "environment http://proxy.corp:8080 (nothing is configured for this registry)",
				ProxyInUse: true, DirectTested: true, DirectReachable: true,
				ProxiedRate: 2.1 * (1 << 20), DirectRate: 18 * (1 << 20),
			},
			RTTMs: 42,
			Levels: []v1.CalibrationLevel{
				{Concurrency: 1, Bytes: "10485760", Rate: 5 << 20, PerStream: 5 << 20, Requests: 12, TTFBMs: 180},
				{Concurrency: 2, Bytes: "20971520", Rate: 10 << 20, PerStream: 5 << 20, Requests: 24, TTFBMs: 190},
				{Concurrency: 4, Bytes: "37748736", Rate: 18 << 20, PerStream: 4.5 * (1 << 20), Requests: 44, TTFBMs: 210},
				{Concurrency: 8, Bytes: "39845888", Rate: 19 << 20, PerStream: 2.4 * (1 << 20), Requests: 48, TTFBMs: 460, Errors: 2, Throttled: 2},
			},
			Knee: 4,
		},
		Target: v1.CalibrationSide{
			Role: "target", Name: "jfrog-lab", Registry: "artifactory.example.com",
			Repository: "apm-oci-stage",
			Route:      v1.CalibrationRoute{Configured: "direct (proxy.direct is set)"},
			RTTMs:      3,
			Levels: []v1.CalibrationLevel{
				{Concurrency: 1, Bytes: "31457280", Rate: 15 << 20, PerStream: 15 << 20, Requests: 9, TTFBMs: 40},
				{Concurrency: 2, Bytes: "50331648", Rate: 24 << 20, PerStream: 12 << 20, Requests: 15, TTFBMs: 55},
			},
			Knee: 2,
		},
		Suggestions: []v1.CalibrationSuggestion{
			{
				Severity: "critical", Setting: "network.proxy.direct", Scope: "source near",
				Current: "unset (traffic goes through the proxy)", Suggested: "true",
				Evidence: "direct moved 18.0 MiB/s against 2.1 MiB/s through the proxy - 757% faster. " +
					"The proxy in force is: environment http://proxy.corp:8080",
			},
			{
				Severity: "recommended", Setting: "worker.maxConcurrentJobs", Scope: "each worker",
				Suggested: "2",
				Evidence:  "the source knee is 4 streams and the target's is 2; a job holds one of each at once",
			},
			{
				Severity: "info", Scope: "this path",
				Suggested: "24.0 MiB/s is the ceiling measured today",
				Evidence:  "read 19.0 MiB/s from the source, wrote 24.0 MiB/s to the target; the read side governs",
			},
		},
		Notes: []string{"measured from ops-laptop. If workers run elsewhere, their path is not this one"},
	}
}

func TestCalibrationTableMarksTheLevelToConfigure(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCalibration(&buf, sampleCalibration()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// The knee is the number somebody types into a config file. It has to be
	// findable in the table, not only restated in the prose below it.
	var marked []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			marked = append(marked, line)
		}
	}
	if len(marked) != 2 {
		t.Errorf("%d level(s) marked as the knee, want one per side:\n%s", len(marked), out)
	}

	// Throttling must not read as a generic error: it is the registry stating
	// a limit, and it outranks every rate in the table.
	if !strings.Contains(out, "throttled") {
		t.Error("a level with 429s did not say so")
	}
	// The proxy comparison belongs in the header of the side it applies to.
	if !strings.Contains(out, "direct against") {
		t.Errorf("the direct-route comparison is missing:\n%s", out)
	}
}

func TestEverySuggestionIsPrintedWithItsEvidence(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCalibration(&buf, sampleCalibration()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, s := range sampleCalibration().Suggestions {
		if s.Setting != "" && !strings.Contains(out, s.Setting) {
			t.Errorf("setting %q is missing from the output", s.Setting)
		}
		// Evidence is wrapped, so look for a distinctive fragment rather than
		// the whole sentence.
		fragment := strings.Fields(s.Evidence)[0]
		if !strings.Contains(out, fragment) {
			t.Errorf("evidence for %q is missing (looked for %q)", s.Setting, fragment)
		}
	}
	if !strings.Contains(out, "!!") {
		t.Error("the critical suggestion is not marked as one")
	}
}

func TestEmptySuggestionsSayNothingToChange(t *testing.T) {
	resp := sampleCalibration()
	resp.Suggestions = nil

	var buf bytes.Buffer
	if err := renderCalibration(&buf, resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No suggestions") {
		t.Error("an empty suggestion list rendered as nothing at all")
	}
}
