package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Choosing WHAT to measure, and being able to disagree with the choice.

func samplePlan() calibrationPlan {
	return calibrationPlan{
		product: "cfx-5000-product",
		source: v1.Repository{Name: "cfx-near",
			Registry: "cfx-5000-product-orb-docker.swdp-us.support.nokia.com"},
		target: v1.Repository{Name: "cfx-jfrog-lab",
			Registry: "artifact.it.att.com", Repository: "apm0014228-oci-stage"},
		repository:        "cfx-5000-product/admin",
		why:               "holds the largest discovered package (63.7 GiB)",
		otherRepositories: 41,
	}
}

// The plan is shown BEFORE anything expensive happens, and it names the one
// repository out of forty-two that will actually be measured. A calibration
// that silently picked one and reported a number for "the source" is how
// `cfx-5000-product/aaa` got measured and believed.
func TestThePlanSaysWhichRepositoryAndWhy(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirm(strings.NewReader(""), &out, samplePlan(), calibrateOptions{},
		[]int{1, 2, 4}, "42s", true)
	if err != nil || !ok {
		t.Fatalf("assumeYes did not proceed: ok=%v err=%v", ok, err)
	}

	text := out.String()
	for _, want := range []string{
		"cfx-5000-product/admin",     // what will be measured
		"largest discovered package", // why it was chosen
		"41 other(s) not measured",   // what will not be
		"apm0014228-oci-stage/",      // where the write probe goes
		"1, 2, 4 streams",            // the sweep it will run
		"never committed",            // what the write probe leaves
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the plan does not mention %q:\n%s", want, text)
		}
	}
}

// Answering anything but yes cancels, and nothing is measured.
func TestAnAnswerOtherThanYesCancels(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "no\n", "later\n"} {
		var out bytes.Buffer
		ok, err := confirm(strings.NewReader(answer), &out, samplePlan(),
			calibrateOptions{}, nil, "1m", false)
		if err != nil {
			t.Fatalf("%q: %v", answer, err)
		}
		// Not a terminal, so it does not actually ask — the guard is that a
		// non-interactive stream proceeds rather than blocking forever on a
		// question nobody can answer.
		if !ok {
			t.Errorf("%q: a non-interactive run refused to proceed", answer)
		}
	}
}

// A product with several sources and no --from must not be guessed at when
// there is nobody to ask.
func TestAmbiguityWithoutATerminalIsAUsageError(t *testing.T) {
	candidates := []v1.Repository{
		{Name: "cfx-near", Enabled: true}, {Name: "cfx-mirror", Enabled: true},
	}

	var out bytes.Buffer
	_, err := chooseRepository(strings.NewReader(""), &out, "source", candidates, "",
		func(v1.Repository) bool { return false })
	if err == nil {
		t.Fatal("two sources and no flag resolved to one anyway")
	}
	for _, want := range []string{"--from", "cfx-near", "cfx-mirror"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A named source that does not exist names the ones that do.
func TestANamedSourceThatIsNotThereListsWhatIs(t *testing.T) {
	candidates := []v1.Repository{{Name: "cfx-near", Enabled: true}}

	var out bytes.Buffer
	_, err := chooseRepository(strings.NewReader(""), &out, "source", candidates, "typo",
		func(v1.Repository) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "cfx-near") {
		t.Errorf("error = %v, want it to name the source that exists", err)
	}
}

// One candidate needs no question, and the default target needs none either.
func TestNoQuestionWhenThereIsNothingToDecide(t *testing.T) {
	var out bytes.Buffer

	only := []v1.Repository{{Name: "cfx-near", Enabled: true}}
	got, err := chooseRepository(strings.NewReader(""), &out, "source", only, "",
		func(v1.Repository) bool { return false })
	if err != nil || got.Name != "cfx-near" {
		t.Errorf("single candidate = %v, %v", got.Name, err)
	}

	targets := []v1.Repository{
		{Name: "lab", Enabled: true}, {Name: "prod", Enabled: true, Default: true},
	}
	got, err = chooseRepository(strings.NewReader(""), &out, "target", targets, "",
		func(r v1.Repository) bool { return r.Default })
	if err != nil || got.Name != "prod" {
		t.Errorf("default target = %v, %v", got.Name, err)
	}
	if out.Len() != 0 {
		t.Errorf("a decided choice still printed a question:\n%s", out.String())
	}
}

// An explicit --source-repository is the whole answer, and says so.
func TestAnExplicitRepositoryIsNotSecondGuessed(t *testing.T) {
	repo, why, others := chooseSourceRepository(t.Context(), nil, "p",
		v1.Repository{Repositories: []string{"a", "b"}}, "chosen/one")

	if repo != "chosen/one" {
		t.Errorf("repository = %q, want the one that was named", repo)
	}
	if !strings.Contains(why, "--source-repository") {
		t.Errorf("why = %q, want it to say the choice was explicit", why)
	}
	if others != 0 {
		t.Errorf("others = %d, want 0 for an explicit choice", others)
	}
}
