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
	Title    string `json:"title"`
	Filename string `json:"filename"`
	Rows     []struct {
		Title  string `json:"title"`
		Panels []struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Expr        []string `json:"expr"`
			Alias       []string `json:"alias"`
			Width       int      `json:"width"`
		} `json:"panels"`
	} `json:"rows"`
}

// dashboardsDir is the directory the chart globs and compose mounts. Read
// rather than listed, so a dashboard added without being added here is not a
// dashboard that goes unchecked.
var dashboardsDir = filepath.Join(repoRoot, "deploy", "observability", "dashboards")

func readDashboards(t *testing.T) map[string]dashboard {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(dashboardsDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatalf("no dashboards in %s; the chart and compose would serve an "+
			"empty vmui", dashboardsDir)
	}

	out := make(map[string]dashboard, len(names))
	for _, path := range names {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var d dashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s is not valid JSON, so vmui will serve it as an empty "+
				"dashboard: %v", path, err)
		}
		out[filepath.Base(path)] = d
	}
	return out
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
	for file, d := range readDashboards(t) {
		for _, row := range d.Rows {
			for _, panel := range row.Panels {
				for _, expr := range panel.Expr {
					for _, name := range metricsIn(expr) {
						if known[name] {
							continue
						}
						have := make([]string, 0, len(known))
						for n := range known {
							if strings.HasPrefix(n, "softwaregateway_") {
								have = append(have, n)
							}
						}
						sort.Strings(have)
						t.Errorf("%s: panel %q plots %s, which no metric exports.\n"+
							"Either the metric was renamed and this expression was not, or the\n"+
							"expression has a typo. Exported names:\n  %s\n",
							file, panel.Title, name, strings.Join(have, "\n  "))
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
	for file, d := range readDashboards(t) {
		if d.Title == "" {
			t.Errorf("%s has no title, so vmui labels its tab with the filename", file)
		}
		// vmui resolves a dashboard by the `filename` INSIDE it, not by the
		// name on disk. A copied file that kept the original's value serves
		// one dashboard under two tabs.
		if d.Filename != file {
			t.Errorf("%s declares filename %q. vmui identifies a dashboard by "+
				"this field, so two files that share one value collapse into a "+
				"single tab.", file, d.Filename)
		}
		if len(d.Rows) == 0 {
			t.Errorf("%s has no rows", file)
		}
		for _, row := range d.Rows {
			if len(row.Panels) == 0 {
				t.Errorf("%s: row %q has no panels", file, row.Title)
			}
			for _, panel := range row.Panels {
				switch {
				case panel.Title == "":
					t.Errorf("%s: row %q has a panel with no title", file, row.Title)
				case len(panel.Expr) == 0:
					t.Errorf("%s: panel %q has no expression, so it draws nothing",
						file, panel.Title)
				case len(panel.Description) < 40:
					t.Errorf("%s: panel %q has no description worth reading; say what "+
						"the panel is for and what a wrong-looking value means.",
						file, panel.Title)
				case panel.Width < 0 || panel.Width > 12:
					t.Errorf("%s: panel %q has width %d. vmui lays a row out on a "+
						"twelve-column grid; anything outside 1..12 (or 0 for the "+
						"full width) renders wrong.", file, panel.Title, panel.Width)
				case len(panel.Alias) > 0 && len(panel.Alias) != len(panel.Expr):
					t.Errorf("%s: panel %q has %d aliases for %d expressions. vmui "+
						"pairs them by position, so the extras are silently ignored "+
						"and the legend shows raw label sets instead.",
						file, panel.Title, len(panel.Alias), len(panel.Expr))
				}
			}
		}
	}
}
