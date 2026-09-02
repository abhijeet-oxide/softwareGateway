package cel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
)

// deployment builds a workload with the containers given, as a decoded manifest
// would look.
func deployment(name string, containers ...map[string]any) map[string]any {
	list := make([]any, 0, len(containers))
	for _, c := range containers {
		list = append(list, c)
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": "ns"},
		"spec": map[string]any{
			"replicas": int64(3),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": name}},
				"spec":     map[string]any{"containers": list},
			},
		},
	}
}

func container(name string, limits map[string]any) map[string]any {
	c := map[string]any{"name": name, "image": "reg.example.com/x:1"}
	if limits != nil {
		c["resources"] = map[string]any{"limits": limits}
	}
	return c
}

func run(t *testing.T, check compliance.Check, objs ...map[string]any) []compliance.Result {
	t.Helper()
	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatalf("compiler: %v", err)
	}
	prog, err := comp.Compile(check)
	if err != nil {
		t.Fatalf("compiling %s: %v", check.ID, err)
	}
	cat := compliance.NewCatalog()
	if err := cat.Add(check, prog); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	rel := &compliance.Release{Product: "p", Tag: "t"}
	for _, o := range objs {
		rel.Resources = append(rel.Resources, compliance.Resource{Object: o})
	}
	eng := &compliance.Engine{Catalog: cat}
	res, err := eng.Run(context.Background(), rel)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

// A container-scoped check must produce one result per container, not one per
// workload. This is the difference between a report a vendor can work through
// and a single row that says the same thing after three of four are fixed.
func TestOneResultPerContainer(t *testing.T) {
	check := compliance.Check{
		ID: "RES-01", Title: "Containers declare memory limits", Severity: compliance.SeverityBlock,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"Deployment"}, Containers: compliance.ScopeAll},
		Assert:    compliance.Assert{Required: []string{"resources.limits.memory"}},
	}
	results := run(t, check,
		deployment("app",
			container("main", map[string]any{"memory": "1Gi"}),
			container("sidecar", nil),
		))

	if len(results) != 2 {
		t.Fatalf("want 2 results, one per container, got %d: %v", len(results), results)
	}
	var pass, fail int
	for _, r := range results {
		switch r.Outcome {
		case compliance.OutcomePass:
			pass++
			if r.Address.Container != "main" {
				t.Errorf("the passing container should be main, got %q", r.Address.Container)
			}
		case compliance.OutcomeFail:
			fail++
			if r.Address.Container != "sidecar" {
				t.Errorf("the failing container should be sidecar, got %q", r.Address.Container)
			}
			want := "spec.template.spec.containers[1].resources.limits.memory"
			if r.Address.Locus != want {
				t.Errorf("locus = %q, want %q - a vendor navigates by this", r.Address.Locus, want)
			}
			if !strings.Contains(r.Message, "sidecar") {
				t.Errorf("message does not name the container: %q", r.Message)
			}
		}
	}
	if pass != 1 || fail != 1 {
		t.Errorf("want one pass and one fail, got %d and %d", pass, fail)
	}
}

// The failure this package exists to prevent: seven of the sixteen inherited
// policies declare CronJob and then read spec.template.spec, so every scheduled
// job passes without being looked at.
func TestCronJobIsActuallyReached(t *testing.T) {
	cronjob := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": "backup", "namespace": "ns"},
		"spec": map[string]any{
			"jobTemplate": map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{container("dump", nil)},
						},
					},
				},
			},
		},
	}
	check := compliance.Check{
		ID: "RES-01", Title: "Containers declare memory limits", Severity: compliance.SeverityBlock,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"Deployment", "CronJob"}, Containers: compliance.ScopeAll},
		Assert:    compliance.Assert{Required: []string{"resources.limits.memory"}},
	}
	results := run(t, check, cronjob)
	if len(results) != 1 {
		t.Fatalf("the CronJob's container was not reached: got %d results", len(results))
	}
	if results[0].Outcome != compliance.OutcomeFail {
		t.Fatalf("want fail, got %s", results[0].Outcome)
	}
	want := "spec.jobTemplate.spec.template.spec.containers[0].resources.limits.memory"
	if results[0].Address.Locus != want {
		t.Errorf("locus = %q, want %q", results[0].Address.Locus, want)
	}
}

// A check that applies to nothing says so. Silence and compliance must not look
// the same.
func TestNoSubjectsProducesASkip(t *testing.T) {
	check := compliance.Check{
		ID: "PDB-01", Title: "Workloads are covered by a PodDisruptionBudget", Severity: compliance.SeverityWarn,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"StatefulSet"}},
		Assert:    compliance.Assert{Expr: `pdbFor(self).spec.minAvailable >= 1`},
	}
	results := run(t, check, deployment("app", container("main", nil)))
	if len(results) != 1 || results[0].Outcome != compliance.OutcomeSkip {
		t.Fatalf("want a single skip, got %v", results)
	}
}

// Cross-resource work goes through an engine function, and it must understand
// matchExpressions - the spelling the inherited pdb.rego ignores entirely.
func TestPDBForUnderstandsMatchExpressions(t *testing.T) {
	sts := map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": "etcd", "namespace": "ns"},
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "etcd"}},
				"spec":     map[string]any{"containers": []any{container("etcd", nil)}},
			},
		},
	}
	pdb := map[string]any{
		"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
		"metadata": map[string]any{"name": "etcd", "namespace": "ns"},
		"spec": map[string]any{
			"minAvailable": int64(2),
			"selector": map[string]any{
				"matchExpressions": []any{
					map[string]any{"key": "app", "operator": "In", "values": []any{"etcd"}},
				},
			},
		},
	}
	check := compliance.Check{
		ID: "PDB-01", Title: "Quorum workloads keep a majority available", Severity: compliance.SeverityBlock,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"StatefulSet"}},
		Assert: compliance.Assert{
			Expr:  `int(value(pdbFor(self), "spec.minAvailable")) >= 2`,
			Locus: "spec.minAvailable",
		},
	}
	results := run(t, check, sts, pdb)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Outcome != compliance.OutcomePass {
		t.Fatalf("the PDB selects this workload via matchExpressions and should pass: %+v", results[0])
	}
}

// An assertion that is not a condition is refused at load, not accepted as
// truthy at run time.
func TestNonBooleanAssertionIsALoadError(t *testing.T) {
	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	_, err = comp.Compile(compliance.Check{
		ID: "BAD-01", Title: "x", Severity: compliance.SeverityInfo,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"Pod"}},
		Assert:    compliance.Assert{Expr: `text(self, "metadata.name")`},
	})
	if err == nil {
		t.Fatal("a string-valued assertion compiled; it would have passed every subject")
	}
}

// A typo is a load error naming the check, not a fault on release 47.
func TestUnknownFunctionIsALoadError(t *testing.T) {
	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	_, err = comp.Compile(compliance.Check{
		ID: "BAD-02", Title: "x", Severity: compliance.SeverityInfo,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"Pod"}},
		Assert:    compliance.Assert{Expr: `pdbForr(self) != null`},
	})
	if err == nil {
		t.Fatal("an unknown function compiled")
	}
}

// An expression that faults produces an error result, never a pass and never a
// fail: it has shown nothing about the subject.
func TestFaultingExpressionIsUndecidable(t *testing.T) {
	check := compliance.Check{
		ID: "ERR-01", Title: "x", Severity: compliance.SeverityBlock,
		AppliesTo: compliance.AppliesTo{Kinds: []string{"Deployment"}},
		Assert:    compliance.Assert{Expr: `int(text(self, "metadata.name")) > 0`},
	}
	results := run(t, check, deployment("app", container("main", nil)))
	if len(results) != 1 || results[0].Outcome != compliance.OutcomeError {
		t.Fatalf("want one error result, got %v", results)
	}
	if results[0].Error == "" {
		t.Error("an error result with no reason is a dead end")
	}
}
