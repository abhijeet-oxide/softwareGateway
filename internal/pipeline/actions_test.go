package pipeline_test

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/pipeline"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

func fullRoute() pipeline.Pipeline {
	return pipeline.Resolve(siteTasks(), productWith(
		target("external", "external"),
		target("lab", "lab"),
		target("quay-eu", "lab"),
		target("gold", "prod"),
	))
}

func available(t *testing.T, pl pipeline.Pipeline, stage string) pipeline.Action {
	t.Helper()
	a, ok := pl.Available(pipeline.Location{Stage: stage})
	if !ok {
		t.Fatalf("no action available at stage %q", stage)
	}
	return a
}

// EXACTLY ONE action is available at a time - the property the release page's
// single primary button depends on.
func TestActionsOfferExactlyOneAtEachPosition(t *testing.T) {
	pl := fullRoute()

	for _, stage := range []string{"", "external", "lab"} {
		count := 0
		for _, a := range pl.Actions(pipeline.Location{Stage: stage}) {
			if a.Available {
				count++
			}
		}
		if count != 1 {
			t.Errorf("stage %q: %d actions available, want exactly 1", stage, count)
		}
	}
}

func TestActionsWalkTheRoute(t *testing.T) {
	pl := fullRoute()

	if got, want := available(t, pl, "").Name, "download"; got != want {
		t.Errorf("undiscovered release offers %q, want %q", got, want)
	}
	if got, want := available(t, pl, "external").Name, "onboard"; got != want {
		t.Errorf("release in external offers %q, want %q", got, want)
	}
	if got, want := available(t, pl, "lab").Name, "promote"; got != want {
		t.Errorf("release in lab offers %q, want %q", got, want)
	}
}

// The end of the route offers nothing, and that is not an error.
func TestActionsAtTheEndOfTheRoute(t *testing.T) {
	pl := fullRoute()
	if _, ok := pl.Available(pipeline.Location{Stage: "prod"}); ok {
		t.Error("a release in the last stage should have no action available")
	}
}

// The label a button carries comes from configuration, not from code.
func TestActionCarriesTheConfiguredLabel(t *testing.T) {
	pl := fullRoute()
	if got, want := available(t, pl, "external").Label, "Onboard to Lab"; got != want {
		t.Errorf("label = %q, want %q", got, want)
	}
}

// An unavailable action must explain itself well enough to render as help text.
func TestUnavailableActionsNameTheBlockingStage(t *testing.T) {
	pl := fullRoute()

	for _, a := range pl.Actions(pipeline.Location{Stage: "external"}) {
		if a.Available {
			continue
		}
		if a.Reason == "" {
			t.Errorf("action %q is unavailable with no reason", a.Name)
			continue
		}
		if !strings.HasSuffix(a.Reason, ".") {
			t.Errorf("action %q reason is not a sentence: %q", a.Name, a.Reason)
		}
	}

	actions := pl.Actions(pipeline.Location{Stage: "external"})
	if got, want := actions[2].Reason, "This release is in external. It has to reach lab first."; got != want {
		t.Errorf("promote reason = %q, want %q", got, want)
	}
	// The entry task, on a release that has already landed in its destination:
	// "already in external" is a better answer than "already brought in",
	// because it names where the release is rather than what happened to it.
	if got, want := actions[0].Reason, "This release is already in external."; got != want {
		t.Errorf("download reason = %q, want %q", got, want)
	}
}

// The action names what it would write, so the confirmation dialog can list the
// Quays without re-deriving the fan-out.
func TestActionListsEveryTargetItWouldWrite(t *testing.T) {
	pl := fullRoute()
	onboard := available(t, pl, "external")

	if got, want := len(onboard.Targets), 2; got != want {
		t.Fatalf("onboard writes %v, want %d targets", onboard.Targets, want)
	}
	if onboard.Targets[0] != "lab" || onboard.Targets[1] != "quay-eu" {
		t.Errorf("targets = %v, want [lab quay-eu]", onboard.Targets)
	}
}

// A product whose route collapsed still offers a sensible first action.
func TestActionsOnACollapsedRoute(t *testing.T) {
	pl := pipeline.Resolve(siteTasks(), productWith(target("lab", "lab"), target("gold", "prod")))

	a := available(t, pl, "")
	if a.Name != "onboard" {
		t.Errorf("entry action = %q, want onboard", a.Name)
	}
	if a.Label != "Onboard to Lab" {
		t.Errorf("label = %q, want the configured display name", a.Label)
	}
}

func TestActionsOnAnEmptyRoute(t *testing.T) {
	pl := pipeline.Resolve(siteTasks(), productWith(product.Target{Name: "x", Registry: "r", Repository: "p/x"}))

	if got := pl.Actions(pipeline.Location{}); len(got) != 0 {
		t.Errorf("actions = %v, want none", got)
	}
	if _, ok := pl.Available(pipeline.Location{}); ok {
		t.Error("an empty route should offer nothing")
	}
}
