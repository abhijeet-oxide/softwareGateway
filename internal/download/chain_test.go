package download

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// estate is the topology this feature exists for, plus a DR target that pulls
// from the vendor independently.
func estate() *product.Product {
	return &product.Product{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec: product.Spec{Targets: []product.Target{
			{Name: "jfrog-store", Registry: "artifactory.example.com",
				Repository: "docker-local/vendor-a", Default: true},
			{Name: "dr-store", Registry: "dr.example.com", Repository: "vendor-a"},
			{Name: "ocp-prod", Registry: "quay.example.com",
				Repository: "platform-prod/vendor-a", Type: product.RegistryQuay,
				Replication: &product.Replication{Mode: product.ReplicationMirror,
					Mirror: &product.MirrorConfig{From: "jfrog-store",
						Tags: []string{"v3.*"}, Robot: "platform-prod+swgw"}}},
		}},
	}
}

func derive(t *testing.T, targets ...string) Chain {
	t.Helper()
	c, err := Derive(estate(), targets)
	if err != nil {
		t.Fatalf("Derive(%v): %v", targets, err)
	}
	return c
}

// THE behaviour: naming the tail names the chain.
func TestNamingTheTailPlansTheWholeChain(t *testing.T) {
	c := derive(t, "ocp-prod")

	if len(c.Steps) != 2 {
		t.Fatalf("expected two steps, got %v", c.Targets())
	}
	if c.Steps[0].Target != "jfrog-store" || c.Steps[0].Index != 0 {
		t.Fatalf("the hop must come first: %+v", c.Steps[0])
	}
	if c.Steps[1].Target != "ocp-prod" || c.Steps[1].Index != 1 {
		t.Fatalf("the named destination waits on it: %+v", c.Steps[1])
	}
	if c.Steps[1].From != "jfrog-store" {
		t.Fatalf("the edge must be recorded: %+v", c.Steps[1])
	}
}

// Naming every hop is a no-op, which is what makes the change backward
// compatible with every rule already written.
func TestNamingEveryHopIsIdentical(t *testing.T) {
	tail := derive(t, "ocp-prod")
	both := derive(t, "jfrog-store", "ocp-prod")

	if strings.Join(tail.Targets(), ",") != strings.Join(both.Targets(), ",") {
		t.Fatalf("%v != %v", tail.Targets(), both.Targets())
	}
	for i := range tail.Steps {
		if tail.Steps[i].Index != both.Steps[i].Index {
			t.Fatalf("step %d index differs: %d vs %d", i, tail.Steps[i].Index, both.Steps[i].Index)
		}
	}
}

// A hop pulled in by the closure is distinguishable from one somebody typed,
// because an error about it has to say which target dragged it in.
func TestNamedByRuleDistinguishesAHopFromADestination(t *testing.T) {
	c := derive(t, "ocp-prod")
	if c.Steps[0].NamedByRule {
		t.Fatal("jfrog-store was pulled in, not named")
	}
	if !c.Steps[1].NamedByRule {
		t.Fatal("ocp-prod was named")
	}
}

// Fan-out and chaining are the same feature: independent destinations share an
// index and nothing in the rule distinguishes them.
func TestFanOutSharesAStepIndex(t *testing.T) {
	c := derive(t, "jfrog-store", "dr-store")

	for _, s := range c.Steps {
		if s.Index != 0 {
			t.Fatalf("independent destinations run together: %+v", s)
		}
		if s.From != "" {
			t.Fatalf("neither pulls from the other: %+v", s)
		}
	}
	if c.MaxIndex() != 0 {
		t.Fatalf("MaxIndex = %d", c.MaxIndex())
	}
}

func TestFanOutAndChainTogether(t *testing.T) {
	c := derive(t, "dr-store", "ocp-prod")

	if len(c.Steps) != 3 {
		t.Fatalf("expected dr-store, jfrog-store and ocp-prod, got %v", c.Targets())
	}
	if c.Describe() != "dr-store + jfrog-store → ocp-prod" {
		t.Fatalf("Describe() = %q", c.Describe())
	}
}

// The rendering is what makes the chain implicit to type and never implicit to
// read.
func TestDescribeRendersTheChain(t *testing.T) {
	if got := derive(t, "ocp-prod").Describe(); got != "jfrog-store → ocp-prod" {
		t.Fatalf("Describe() = %q", got)
	}
	if got := (Chain{}).Describe(); got != "nothing" {
		t.Fatalf("an empty chain = %q", got)
	}
}

func TestPredecessorFindsTheStepWaitedOn(t *testing.T) {
	c := derive(t, "ocp-prod")

	pred, ok := c.Predecessor("ocp-prod")
	if !ok || pred.Target != "jfrog-store" {
		t.Fatalf("Predecessor = (%+v, %v)", pred, ok)
	}
	if _, ok := c.Predecessor("jfrog-store"); ok {
		t.Fatal("the head of a chain waits for nothing")
	}
}

// No targets means the default, which is what every rule that omits the field
// has always meant.
func TestNoTargetsUsesTheDefault(t *testing.T) {
	c := derive(t)
	if len(c.Steps) != 1 || c.Steps[0].Target != "jfrog-store" {
		t.Fatalf("got %v", c.Targets())
	}
}

func TestNoTargetsAndNoDefaultIsAnError(t *testing.T) {
	p := estate()
	p.Spec.Targets[0].Default = false

	if _, err := Derive(p, nil); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "no default target") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownTargetIsAnError(t *testing.T) {
	if _, err := Derive(estate(), []string{"nowhere"}); err == nil {
		t.Fatal("expected an error")
	}
}

// An upstream outside the chain — an externalReference, or a hop another rule
// fills — means nothing in THIS chain precedes it.
func TestUpstreamOutsideTheProductIsIndexZero(t *testing.T) {
	p := estate()
	p.Spec.Targets[2].Replication.Mirror.From = ""
	p.Spec.Targets[2].Replication.Mirror.ExternalReference = "elsewhere.example.com/path"

	c, err := Derive(p, []string{"ocp-prod"})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(c.Steps) != 1 || c.Steps[0].Index != 0 || c.Steps[0].From != "" {
		t.Fatalf("a chain cannot wait on something it does not contain: %+v", c.Steps)
	}
}

// A cycle must be reported rather than hung on. It is rejected at validation
// too; this is the belt to that braces, because Derive also runs against
// configuration that arrived some other way.
func TestCycleIsReportedNotHung(t *testing.T) {
	p := estate()
	p.Spec.Targets[0].Type = product.RegistryQuay
	p.Spec.Targets[0].Replication = &product.Replication{
		Mode: product.ReplicationMirror,
		Mirror: &product.MirrorConfig{From: "ocp-prod",
			Tags: []string{"v3.*"}, Robot: "docker-local+swgw"},
	}

	_, err := Derive(p, []string{"ocp-prod"})
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("got %v", err)
	}
}

// The delegated flag rides along, because the step that configures a mirror is
// rendered differently from the one that copies bytes.
func TestStepsCarryWhetherTheyAreDelegated(t *testing.T) {
	c := derive(t, "ocp-prod")
	if c.Steps[0].Delegated {
		t.Fatal("jfrog-store is a copy")
	}
	if !c.Steps[1].Delegated {
		t.Fatal("ocp-prod is a mirror")
	}
}

// A dry run that reordered between two identical invocations would be
// unreviewable.
func TestOrderIsStable(t *testing.T) {
	first := derive(t, "ocp-prod", "dr-store").Targets()
	for i := 0; i < 20; i++ {
		got := derive(t, "ocp-prod", "dr-store").Targets()
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("order changed: %v vs %v", got, first)
		}
	}
}
