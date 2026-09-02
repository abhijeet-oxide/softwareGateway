package baseline_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// runFixture evaluates the whole shipped baseline against a rendered manifest
// stream, the way a real release is evaluated.
//
// Every fixture runs against the WHOLE pack, not just the check it was written
// for. A new check that makes an unrelated one fire is caught here, and this is
// the only place that interaction is ever visible.
func runFixture(t *testing.T, name string) []compliance.Result {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	base := compliance.Address{
		Product: "fixtures", Release: "v1", Chart: strings.TrimSuffix(name, ".yaml"),
		ChartVersion: "1.0.0",
	}
	resources, parseErrs := compliance.ParseManifests(body, base)
	if len(parseErrs) > 0 {
		t.Fatalf("fixture %s does not parse: %v", name, parseErrs)
	}
	if len(resources) == 0 {
		t.Fatalf("fixture %s produced no resources", name)
	}

	rel := &compliance.Release{
		Product: "fixtures", Tag: "v1", Resources: resources,
		Config: map[string]any{
			"approvedRegistries": []any{"registry.acme.example"},
		},
	}
	eng := &compliance.Engine{Catalog: loadShipped(t)}
	results, err := eng.Run(context.Background(), rel)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return results
}

// failures returns the check IDs that failed or could not be decided, with the
// subject, so a diff names what is wrong rather than how many.
func failures(results []compliance.Result) []string {
	var out []string
	for _, r := range results {
		switch r.Outcome {
		case compliance.OutcomeFail, compliance.OutcomeError:
			out = append(out, r.CheckID+" @ "+r.Address.Where()+" - "+r.Message)
		}
	}
	sort.Strings(out)
	return out
}

// The merge gate. A compliant release must produce no failures across the
// entire baseline, so "my new check has no false positives" is asserted by CI
// rather than by whoever wrote it.
func TestGoodFixtureIsClean(t *testing.T) {
	results := runFixture(t, "good-app.yaml")
	if bad := failures(results); len(bad) > 0 {
		t.Errorf("the good fixture produced %d findings; every one is a false positive:", len(bad))
		for _, b := range bad {
			t.Errorf("    %s", b)
		}
	}

	counts := compliance.Tally(results)
	if counts.Pass == 0 {
		t.Error("no check passed; the fixture is not being reached at all")
	}
	if v := compliance.Decide(counts); v != compliance.VerdictPass {
		t.Errorf("verdict = %s, want pass", v)
	}
	t.Logf("good-app: %d pass, %d skip, %d fail, %d error", counts.Pass, counts.Skip, counts.Fail, counts.Error)
}

// Two runs of the same release must be byte-identical. This is what makes a
// finding reproducible and a comparison between releases meaningful.
func TestRunsAreDeterministic(t *testing.T) {
	a := runFixture(t, "good-app.yaml")
	b := runFixture(t, "good-app.yaml")
	if len(a) != len(b) {
		t.Fatalf("two runs produced %d and %d results", len(a), len(b))
	}
	for i := range a {
		if a[i].CheckID != b[i].CheckID || a[i].Outcome != b[i].Outcome ||
			a[i].Address.Where() != b[i].Address.Where() || a[i].Message != b[i].Message {
			t.Fatalf("result %d differs between runs:\n  %+v\n  %+v", i, a[i], b[i])
		}
	}
}

// expectation is one finding a fixture must produce.
type expectation struct {
	Check     string `json:"check"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
}

type expectations struct {
	// Findings is an EXACT set for the checks it names, not a minimum. A check
	// that also fires on the deliberately-correct object in the same fixture
	// fails here, which is how a false positive is caught at the point it is
	// introduced.
	Findings []expectation `json:"findings"`
}

func (e expectation) String() string {
	s := e.Check + " " + e.Kind + "/" + e.Name
	if e.Container != "" {
		s += " container " + e.Container
	}
	return s
}

// assertFixture runs a fixture and compares the findings for the checks its
// expectations name against the exact expected set.
//
// Scoping the comparison to the named checks is deliberate: a fixture written
// for PDB is not required to be otherwise compliant - the bad-pdb workloads
// have no resource limits either - and demanding that would make every fixture
// a copy of good-app with one thing changed, which is a corpus nobody
// maintains.
func assertFixture(t *testing.T, fixture string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", strings.TrimSuffix(fixture, ".yaml")+".expected.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var want expectations
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want.Findings) == 0 {
		t.Fatalf("%s expects nothing, so it asserts nothing", fixture)
	}

	inScope := map[string]bool{}
	for _, f := range want.Findings {
		inScope[f.Check] = true
	}

	got := map[string]bool{}
	for _, r := range runFixture(t, fixture) {
		if !inScope[r.CheckID] {
			continue
		}
		if r.Outcome == compliance.OutcomeError {
			t.Errorf("%s could not be decided on %s: %s", r.CheckID, r.Address.Where(), r.Error)
			continue
		}
		if r.Outcome != compliance.OutcomeFail {
			continue
		}
		got[expectation{
			Check: r.CheckID, Kind: r.Address.Kind,
			Name: r.Address.Name, Container: r.Address.Container,
		}.String()] = true
	}

	wanted := map[string]bool{}
	for _, f := range want.Findings {
		wanted[f.String()] = true
		if !got[f.String()] {
			t.Errorf("MISSING: %s did not fire", f)
		}
	}
	for g := range got {
		if !wanted[g] {
			t.Errorf("UNEXPECTED: %s fired and was not expected; this is a false positive", g)
		}
	}
}

func TestBadPDB(t *testing.T)       { assertFixture(t, "bad-pdb.yaml") }
func TestBadCronJob(t *testing.T)   { assertFixture(t, "bad-cronjob.yaml") }
func TestBadResources(t *testing.T) { assertFixture(t, "bad-resources.yaml") }
func TestBadSecurity(t *testing.T)  { assertFixture(t, "bad-security.yaml") }
func TestBadNetwork(t *testing.T)   { assertFixture(t, "bad-network.yaml") }
func TestBadSupply(t *testing.T)    { assertFixture(t, "bad-supply.yaml") }
func TestBadRBAC(t *testing.T)      { assertFixture(t, "bad-rbac.yaml") }

func TestBadScheduling(t *testing.T)    { assertFixture(t, "bad-scheduling.yaml") }
func TestBadProbes(t *testing.T)        { assertFixture(t, "bad-probes.yaml") }
func TestBadStorage(t *testing.T)       { assertFixture(t, "bad-storage.yaml") }
func TestBadConfig(t *testing.T)        { assertFixture(t, "bad-config.yaml") }
func TestBadMetadata(t *testing.T)      { assertFixture(t, "bad-metadata.yaml") }
func TestBadObservability(t *testing.T) { assertFixture(t, "bad-observability.yaml") }
