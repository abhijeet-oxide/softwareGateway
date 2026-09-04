package source

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The run log names the chart, the kind and the cause.
//
// # The defect this exists for
//
// An operator watching a real run saw thirteen consecutive lines reading
// "<chart>: Template error", with no cause on any of them. Two things were
// wrong and only one was visible:
//
//  1. The line carried the classification and dropped helm's message, which
//     had been captured and stored the whole time.
//  2. It read the kind off Charts[0] - the FIRST chart of the artifact. For an
//     umbrella whose fourth subchart is broken, Charts[0] is the umbrella,
//     which rendered perfectly, so the line reported the classification of a
//     chart that had no failure at all.
//
// The log is the screen that is up while a run is still going. A line on it
// that names a problem and not its cause is a line that costs somebody a
// re-run.
func TestTheRunLogSaysWhyAChartFailed(t *testing.T) {
	cases := []struct {
		name   string
		chart  string
		failed []*compliance.Chart
		want   []string
		absent []string
	}{
		{
			name:  "a single chart names its cause and its template",
			chart: "cfx-adrf",
			failed: []*compliance.Chart{{
				Name:            "cfx-adrf-chart",
				RenderErrorKind: "needs_values",
				RenderError: "helm template failed for cfx-adrf-chart: Error: execution error at " +
					"(cfx-adrf-chart/templates/chart-check.yaml:2:4): global.registry must be specified",
			}},
			want: []string{
				"cfx-adrf",
				"Requires values",
				"global.registry must be specified",
				"cfx-adrf-chart/templates/chart-check.yaml",
			},
			// The artifact was already named at the front of the line; the
			// chart's own name is not a second fact when it is not a subchart.
			absent: []string{"(cfx-adrf-chart)"},
		},
		{
			name:  "a broken subchart is reported as the subchart, not the umbrella",
			chart: "orb-umbrella",
			failed: []*compliance.Chart{{
				Name:            "inner",
				SubchartPath:    "charts/inner",
				RenderErrorKind: "template",
				RenderError: "helm template failed for inner: Error: template: inner/templates/d.yaml:14:22: " +
					`executing "inner/templates/d.yaml" at <.Values.image.tag>: ` +
					"nil pointer evaluating interface {}.tag",
			}},
			want: []string{
				"orb-umbrella",
				"charts/inner",
				"Template error",
				"nil pointer evaluating interface {}.tag",
			},
		},
		{
			name:  "several failures give the first in full and count the rest",
			chart: "big-umbrella",
			failed: []*compliance.Chart{
				{Name: "a", SubchartPath: "charts/a", RenderErrorKind: "template",
					RenderError: "helm template failed for a: Error: first cause"},
				{Name: "b", SubchartPath: "charts/b", RenderErrorKind: "invalid_yaml",
					RenderError: "helm template failed for b: Error: second cause"},
				{Name: "c", SubchartPath: "charts/c", RenderErrorKind: "template",
					RenderError: "helm template failed for c: Error: third cause"},
			},
			want:   []string{"charts/a", "first cause", "and 2 more chart(s)"},
			absent: []string{"second cause", "third cause"},
		},
		{
			// A chart with a kind and no message still gets a usable line
			// rather than a dangling separator.
			name:   "a failure with no message still names the kind",
			chart:  "quiet",
			failed: []*compliance.Chart{{Name: "quiet", RenderErrorKind: "renderer"}},
			want:   []string{"quiet", "Renderer unavailable"},
			absent: []string{" - "},
		},
		{
			name:   "no failing chart at all is still a sentence",
			chart:  "empty",
			failed: nil,
			want:   []string{"empty", "render failed"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderFailureLine(c.chart, c.failed)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("line does not contain %q:\n  %s", w, got)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(got, a) {
					t.Errorf("line should not contain %q:\n  %s", a, got)
				}
			}
			if strings.Contains(got, "\n") {
				t.Errorf("a log line must be one line:\n  %s", got)
			}
		})
	}
}
