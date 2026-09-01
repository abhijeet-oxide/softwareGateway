package baseline_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The coverage gate.
//
// # Why this test exists rather than a convention
//
// A check that has never been run against a chart that violates it is a
// hypothesis. Half of the sixteen policies this platform inherited have a
// defect that a single negative fixture would have caught - seven silently skip
// CronJob, one catches one of four deadlock spellings, one fires on a selector
// spelling it does not understand - and every one of them was reviewed by
// somebody who believed it worked.
//
// So the bar is mechanical and it is enforced here: a registered check needs a
// case where it fires and a case where it does not, or CI fails and the
// coverage table below is the list of what is missing.

// negativeCoverage reads every expectation file and returns the checks that
// have a case where they fire.
func negativeCoverage(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.expected.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var e expectations
		if err := yaml.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, f := range e.Findings {
			covered[f.Check] = true
		}
	}
	return covered
}

// positiveCoverage returns the checks that pass on the shared good fixture.
//
// A check that only ever fires has not been shown to distinguish anything: it
// might fire on everything. The good fixture is where "and it passes when it
// should" is asserted, which is also why every new check runs against it.
func positiveCoverage(t *testing.T) map[string]bool {
	t.Helper()
	passing := map[string]bool{}
	for _, r := range runFixture(t, "good-app.yaml") {
		if r.Outcome == compliance.OutcomePass {
			passing[r.CheckID] = true
		}
	}
	return passing
}

func TestEveryCheckHasAFixture(t *testing.T) {
	cat := loadShipped(t)
	fires := negativeCoverage(t)
	passes := positiveCoverage(t)

	var missingNegative, missingPositive []string
	for _, c := range cat.Checks() {
		if c.Deprecated {
			continue
		}
		if !fires[c.ID] {
			missingNegative = append(missingNegative, c.ID+" ("+c.Title+")")
		}
		if !passes[c.ID] {
			missingPositive = append(missingPositive, c.ID+" ("+c.Title+")")
		}
	}
	sort.Strings(missingNegative)
	sort.Strings(missingPositive)

	if len(missingNegative) > 0 {
		t.Errorf("%d checks have never been shown to fire. Each needs a fixture that violates it:", len(missingNegative))
		for _, m := range missingNegative {
			t.Errorf("    %s", m)
		}
	}
	if len(missingPositive) > 0 {
		t.Errorf("%d checks do not pass on good-app, so they have never been shown to accept anything:", len(missingPositive))
		for _, m := range missingPositive {
			t.Errorf("    %s", m)
		}
	}
	t.Logf("%d checks, %d with a firing case, %d with a passing case",
		cat.Len(), len(fires), len(passes))
}

// Every fixture must be reachable from a test, or it is a file nobody runs.
func TestEveryFixtureIsAsserted(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "bad-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if strings.HasSuffix(f, ".expected.yaml") {
			continue
		}
		want := strings.TrimSuffix(f, ".yaml") + ".expected.yaml"
		if _, err := os.Stat(want); err != nil {
			t.Errorf("%s has no expectation file, so nothing checks what it produces", filepath.Base(f))
		}
	}
}
