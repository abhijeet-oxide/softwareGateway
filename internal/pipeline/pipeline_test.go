package pipeline_test

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/pipeline"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// siteTasks is the vocabulary from docs/design/examples/config.yaml.
func siteTasks() []config.Task {
	return []config.Task{
		{
			Name: "download", DisplayName: "Download",
			From: config.SourceStage, To: "external",
			Verify: config.CheckEnforce, Compliance: config.CheckEnforce,
			Scanners: []string{"xray", "anchore"},
		},
		{
			Name: "onboard", DisplayName: "Onboard to Lab",
			From: "external", To: "lab", Purge: true,
			Compliance: config.CheckEnabled,
			Scanners:   []string{"xray", "anchore"},
		},
		{
			Name: "promote", DisplayName: "Promote to Prod",
			From: "lab", To: "prod",
			Scanners: []string{"xray"},
		},
	}
}

func target(name, stage string) product.Target {
	return product.Target{Name: name, Stage: stage, Registry: "r.example.com", Repository: "p/" + name}
}

func productWith(targets ...product.Target) *product.Product {
	return &product.Product{
		APIVersion: product.APIVersion, Kind: product.Kind,
		Metadata: product.Metadata{Name: "p"},
		Spec:     product.Spec{Targets: targets},
	}
}

func TestResolveFullRoute(t *testing.T) {
	p := productWith(target("external", "external"), target("lab", "lab"), target("gold", "prod"))

	pl := pipeline.Resolve(siteTasks(), p)

	if got, want := len(pl.Steps), 3; got != want {
		t.Fatalf("steps = %d, want %d", got, want)
	}
	if got, want := pl.Describe(), "source → external → lab → prod"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
	if len(pl.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none", pl.Skipped)
	}
}

// THE COLLAPSE. A team with no landing zone should get auto-download straight
// into lab, with nothing in either document saying so.
func TestResolveSkipsAStageTheProductDoesNotHave(t *testing.T) {
	p := productWith(target("lab", "lab"), target("gold", "prod"))

	pl := pipeline.Resolve(siteTasks(), p)

	if got, want := len(pl.Steps), 2; got != want {
		t.Fatalf("steps = %d, want %d: %s", got, want, pl.Describe())
	}
	if got, want := pl.Describe(), "source → lab → prod"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}

	entry, ok := pl.Entry()
	if !ok {
		t.Fatal("no entry step; a discovered release would land nowhere")
	}
	if entry.Name != "onboard" {
		t.Errorf("entry task = %q, want onboard", entry.Name)
	}
	if !entry.Repointed || entry.ConfiguredFrom != "external" {
		t.Errorf("entry should record that it was repointed from external, got %+v",
			struct {
				R bool
				C string
			}{entry.Repointed, entry.ConfiguredFrom})
	}

	if len(pl.Skipped) != 1 || pl.Skipped[0].Task != "download" {
		t.Errorf("Skipped = %+v, want the download task", pl.Skipped)
	}
}

// One stage, one task, no promotion anywhere - and no configuration saying so.
func TestResolveSingleStageProduct(t *testing.T) {
	p := productWith(target("internal", "external"))

	pl := pipeline.Resolve(siteTasks(), p)

	if got, want := len(pl.Steps), 1; got != want {
		t.Fatalf("steps = %d, want %d", got, want)
	}
	if got, want := pl.Describe(), "source → external"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

// THE FAN-OUT. Several targets in one stage are one step, and the order is the
// order somebody wrote them in.
func TestResolveFansOutToEveryTargetInAStage(t *testing.T) {
	p := productWith(
		target("external", "external"),
		target("lab", "lab"),
		target("quay-eu", "lab"),
		target("quay-us", "lab"),
		target("gold", "prod"),
		target("quay-eu-prod", "prod"),
	)

	pl := pipeline.Resolve(siteTasks(), p)

	onboard, ok := pl.Step("onboard")
	if !ok {
		t.Fatal("no onboard step")
	}
	if got, want := len(onboard.Targets), 3; got != want {
		t.Fatalf("onboard writes %d targets, want %d", got, want)
	}
	if got, want := onboard.Targets[0].Name, "lab"; got != want {
		t.Errorf("first target = %q, want %q (declaration order)", got, want)
	}

	// Promotion is the same mechanism pointed at a different stage.
	promote, ok := pl.Step("promote")
	if !ok {
		t.Fatal("no promote step")
	}
	if got, want := len(promote.Targets), 2; got != want {
		t.Errorf("promote writes %d targets, want %d", got, want)
	}
}

func TestResolveIgnoresDisabledTargets(t *testing.T) {
	off := false
	labOff := target("lab", "lab")
	labOff.Enabled = &off

	p := productWith(target("external", "external"), labOff, target("gold", "prod"))
	pl := pipeline.Resolve(siteTasks(), p)

	if got, want := pl.Describe(), "source → external → prod"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

// `environment` is the older spelling and has to keep working unchanged.
func TestResolveHonoursTheSupersededEnvironmentField(t *testing.T) {
	legacy := product.Target{Name: "lab", Environment: "lab", Registry: "r", Repository: "p/lab"}
	p := productWith(target("external", "external"), legacy)

	pl := pipeline.Resolve(siteTasks(), p)

	if got, want := pl.Describe(), "source → external → lab"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

func TestResolveProductWithNoStagedTargets(t *testing.T) {
	p := productWith(product.Target{Name: "somewhere", Registry: "r", Repository: "p/x"})

	pl := pipeline.Resolve(siteTasks(), p)

	if len(pl.Steps) != 0 {
		t.Fatalf("steps = %d, want 0", len(pl.Steps))
	}
	if !strings.Contains(pl.Describe(), "no route") {
		t.Errorf("Describe() = %q, want it to say there is no route", pl.Describe())
	}
	if len(pl.Skipped) != 3 {
		t.Errorf("Skipped = %d, want all three tasks", len(pl.Skipped))
	}
}

func TestResolveNilProduct(t *testing.T) {
	if pl := pipeline.Resolve(siteTasks(), nil); len(pl.Steps) != 0 {
		t.Fatalf("steps = %d, want 0", len(pl.Steps))
	}
}
