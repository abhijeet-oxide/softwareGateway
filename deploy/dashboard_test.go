package deploy

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
)

// dashboard is the shape vmui reads. Only the parts an assertion needs.
type dashboard struct {
	Title string `json:"title"`
	Rows  []struct {
		Title  string `json:"title"`
		Panels []struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Expr        []string `json:"expr"`
		} `json:"panels"`
	} `json:"rows"`
}

func readDashboard(t *testing.T) dashboard {
	t.Helper()
	path := filepath.Join(repoRoot, "deploy", "observability", "dashboard-api.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("%s is not valid JSON, so vmui will serve an empty dashboard: %v", path, err)
	}
	return d
}

// exported is every metric name the coordinator's registry declares.
//
// Describe rather than Gather on purpose: a *Vec with no children yet has no
// series to gather, so gathering would report most of the catalog as missing
// in a process that had simply not served a request.
func exported(t *testing.T) map[string]bool {
	t.Helper()
	reg := metrics.New("coordinator")
	reg.BindDatabase(func() sql.DBStats { return sql.DBStats{} })

	descs := make(chan *prometheus.Desc)
	go func() {
		reg.Prometheus().Describe(descs)
		close(descs)
	}()

	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	names := map[string]bool{}
	for d := range descs {
		if m := fqName.FindStringSubmatch(d.String()); m != nil {
			names[m[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("the registry described no metrics at all; this test cannot be trusted")
	}
	return names
}

// metricsIn pulls the metric names out of a PromQL expression.
//
// A bare word followed by `(` is a function, and a word inside a label matcher
// or after `by`/`on` is a label; everything else that looks like an identifier
// is a metric.
var promIdent = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

func metricsIn(expr string) []string {
	// Strip label matchers so status_class and route are not read as metrics.
	expr = regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(expr, "")
	var out []string
	for _, loc := range promIdent.FindAllStringIndex(expr, -1) {
		word := expr[loc[0]:loc[1]]
		if !strings.HasPrefix(word, "softwaregateway_") {
			continue
		}
		// Trim the suffixes Prometheus derives from a histogram, which are
		// series names rather than declared metric names.
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			word = strings.TrimSuffix(word, suffix)
		}
		if !slices.Contains(out, word) {
			out = append(out, word)
		}
	}
	return out
}

// TestTheDashboardOnlyPlotsMetricsWeExport is the one that fails when a metric
// is renamed.
//
// The dashboard is a JSON file with PromQL strings in it. Nothing compiles it,
// so renaming a metric in internal/platform/metrics leaves a panel that draws
// an empty graph - and an empty graph is indistinguishable from a system doing
// nothing, which is exactly the reading an operator must not get from a panel
// called "Error rate by route". The fix when this fails is to update the
// expression in deploy/observability/dashboard-api.json to the new name.
func TestTheDashboardOnlyPlotsMetricsWeExport(t *testing.T) {
	known := exported(t)
	for _, row := range readDashboard(t).Rows {
		for _, panel := range row.Panels {
			for _, expr := range panel.Expr {
				for _, name := range metricsIn(expr) {
					if !known[name] {
						have := make([]string, 0, len(known))
						for n := range known {
							if strings.HasPrefix(n, "softwaregateway_") {
								have = append(have, n)
							}
						}
						sort.Strings(have)
						t.Errorf("panel %q plots %s, which no metric exports.\n"+
							"Either the metric was renamed and this expression was not, or the\n"+
							"expression has a typo. Exported names:\n  %s\n",
							panel.Title, name, strings.Join(have, "\n  "))
					}
				}
			}
		}
	}
}

// TestEveryPanelSaysWhatItIsFor keeps the dashboard readable by somebody who
// did not write it.
//
// A panel titled "p99" tells an operator nothing about whether the number in
// front of them is bad. The description is where the panel says what it is for
// and what a wrong-looking value means, and vmui shows it on the info icon.
func TestEveryPanelSaysWhatItIsFor(t *testing.T) {
	d := readDashboard(t)
	if len(d.Rows) == 0 {
		t.Fatal("the dashboard has no rows")
	}
	for _, row := range d.Rows {
		if len(row.Panels) == 0 {
			t.Errorf("row %q has no panels", row.Title)
		}
		for _, panel := range row.Panels {
			switch {
			case panel.Title == "":
				t.Errorf("row %q has a panel with no title", row.Title)
			case len(panel.Expr) == 0:
				t.Errorf("panel %q has no expression, so it draws nothing", panel.Title)
			case len(panel.Description) < 40:
				t.Errorf("panel %q has no description worth reading; say what the panel is\n"+
					"for and what a wrong-looking value means.", panel.Title)
			}
		}
	}
}
