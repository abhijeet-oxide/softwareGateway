package pipeline_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/pipeline"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"

	"sigs.k8s.io/yaml"
)

// The shipped example must be a document this code accepts.
//
// docs/design/examples/config.yaml is what a reader copies. A design document
// whose example does not load is worse than no example: it teaches a shape the
// software rejects, and nobody finds out until they deploy it.
//
// So the file is a fixture. If the schema changes and the example is not
// updated, this fails - which is the only reliable way documentation and code
// stay in step.

// exampleStage reads just the `stage:` block, which is the part of the example
// this package owns. The rest of the file covers scanners, storage and process
// settings that other packages are responsible for.
func exampleStage(t *testing.T) config.StageConfig {
	t.Helper()

	path := filepath.Join("..", "..", "docs", "design", "examples", "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped example: %v", err)
	}

	var doc struct {
		Stage config.StageConfig `json:"stage"`
	}
	// sigs.k8s.io/yaml routes through encoding/json, so `koanf` tags do not
	// apply and the field names below are the JSON ones. They are the same
	// strings the YAML uses, which is the point.
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the shipped example: %v", err)
	}
	return doc.Stage
}

func TestShippedExampleTaskListValidates(t *testing.T) {
	stage := exampleStage(t)

	if len(stage.Tasks) == 0 {
		t.Fatal("the example declares no tasks; either the file moved or the " +
			"field names no longer match")
	}
	if err := config.ValidateTasks(stage.Tasks); err != nil {
		t.Fatalf("the shipped example does not validate: %v", err)
	}
}

// The example's route has to work on the product the example ships beside it.
func TestShippedExampleResolvesTheDocumentedRoute(t *testing.T) {
	stage := exampleStage(t)

	p := productWith(
		target("external", "external"),
		target("lab", "lab"),
		target("quay-eu", "lab"),
		target("quay-us", "lab"),
		target("gold", "prod"),
		target("quay-eu-prod", "prod"),
	)

	pl := pipeline.Resolve(stage.Tasks, p)

	if got, want := pl.Describe(), "source → external → lab → prod"; got != want {
		t.Errorf("route = %q, want %q", got, want)
	}

	// The fan-out the example is written to demonstrate.
	onboard, ok := pl.Step("onboard")
	if !ok {
		t.Fatal("the example has no onboard task")
	}
	if got, want := len(onboard.Targets), 3; got != want {
		t.Errorf("onboard writes %d targets, want %d", got, want)
	}
	if !onboard.Purge {
		t.Error("the example's onboard task should purge the external copy")
	}

	promote, ok := pl.Step("promote")
	if !ok {
		t.Fatal("the example has no promote task")
	}
	if got, want := len(promote.Targets), 2; got != want {
		t.Errorf("promote writes %d targets, want %d - promotion fans out too", got, want)
	}
}

// `verify: enforce` in the example is inert today and must stay parseable, so
// the day signature verification ships nothing in the file has to change.
func TestShippedExampleAsksForVerificationItCannotYetRun(t *testing.T) {
	stage := exampleStage(t)

	download, ok := stage.Task("download")
	if !ok {
		t.Fatal("the example has no download task")
	}
	if got := download.Verify.Normalize(); got != config.CheckEnforce {
		t.Errorf("download.verify = %q, want %q", got, config.CheckEnforce)
	}
	if !download.Verify.Blocks() {
		t.Error("enforce should block; the skip happens because the check is " +
			"unavailable, not because the mode is weak")
	}
}
