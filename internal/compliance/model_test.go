package compliance

import (
	"encoding/json"
	"testing"
)

// The verdict ordering is the whole of Rule 2. A run that could not decide
// something is not a run that found nothing wrong.
func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		in   Counts
		want Verdict
	}{
		{"clean", Counts{Pass: 10}, VerdictPass},
		{"warnings only", Counts{Pass: 10, Fail: 2, Warning: 2}, VerdictConditional},
		{"a blocking failure", Counts{Pass: 10, Fail: 1, Blocking: 1}, VerdictFail},
		// The single most damaging thing this package could report: a release
		// whose charts would not render, shown as green.
		{"undecided", Counts{Pass: 10, Error: 3}, VerdictInconclusive},
		{"undecided with warnings", Counts{Error: 1, Fail: 1, Warning: 1}, VerdictInconclusive},
		// A definite blocking failure outranks "some of it could not be read":
		// something is known to be wrong and it is actionable now.
		{"undecided with a blocking failure", Counts{Error: 5, Fail: 1, Blocking: 1}, VerdictFail},
		{"nothing applied", Counts{Skip: 8}, VerdictPass},
	}
	for _, c := range cases {
		if got := Decide(c.in); got != c.want {
			t.Errorf("%s: Decide = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestTallyCountsSeverityOfFailuresOnly(t *testing.T) {
	got := Tally([]Result{
		{Outcome: OutcomePass, Severity: SeverityCritical},
		{Outcome: OutcomeFail, Severity: SeverityCritical},
		{Outcome: OutcomeFail, Severity: SeverityWarning},
		{Outcome: OutcomeSkip, Severity: SeverityCritical},
		{Outcome: OutcomeError, Severity: SeverityCritical},
		{Outcome: OutcomeWaived, Severity: SeverityCritical},
	})
	want := Counts{Pass: 1, Fail: 2, Skip: 1, Error: 1, Waived: 1, Blocking: 1, Warning: 1}
	if got != want {
		t.Errorf("Tally = %+v, want %+v", got, want)
	}
	if got.Total() != 6 {
		t.Errorf("Total = %d, want 6", got.Total())
	}
}

// A fingerprint that included the chart version would report every unfixed
// finding as "fixed, and a new one appeared" on the next release, which makes
// a comparison useless.
func TestFingerprintIgnoresVersionAndTag(t *testing.T) {
	a := Result{CheckID: "RES-01", Address: Address{
		Product: "p", Release: "orb_23.8", Chart: "mysvc", ChartVersion: "4.2.1",
		SourceFile: "templates/deploy.yaml", Kind: "Deployment", Name: "app",
		Container: "main", Locus: "resources.limits.memory", RenderedLine: 12,
	}}
	b := a
	b.Address.Release = "orb_23.9"
	b.Address.ChartVersion = "4.3.0"
	b.Address.RenderedLine = 48
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("the same unfixed defect in two releases produced two fingerprints")
	}

	c := a
	c.Address.Container = "sidecar"
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("two containers of one workload share a fingerprint; fixing one would look like fixing both")
	}
}

// Two runs of the same release must produce byte-identical output, which is a
// merge gate. Sorting is where a map iteration order would leak in.
func TestSortIsTotalAndStable(t *testing.T) {
	mk := func(outcome Outcome, sev Severity, chart, name, check string) Result {
		return Result{CheckID: check, Outcome: outcome, Severity: sev,
			Address: Address{Chart: chart, Kind: "Deployment", Name: name}}
	}
	in := []Result{
		mk(OutcomePass, SeverityCritical, "a", "x", "C-01"),
		mk(OutcomeFail, SeverityWarning, "b", "y", "B-01"),
		mk(OutcomeError, SeverityCritical, "a", "z", "A-01"),
		mk(OutcomeFail, SeverityCritical, "c", "w", "D-01"),
		mk(OutcomeSkip, SeverityInform, "a", "v", "E-01"),
	}
	Sort(in)
	want := []Outcome{OutcomeFail, OutcomeFail, OutcomeError, OutcomePass, OutcomeSkip}
	for i, w := range want {
		if in[i].Outcome != w {
			t.Fatalf("position %d is %s, want %s (order: %v)", i, in[i].Outcome, w, outcomes(in))
		}
	}
	// Within failures, blocking comes before warning.
	if in[0].Severity != SeverityCritical {
		t.Errorf("the first failure is %s, want the blocking one first", in[0].Severity)
	}
}

func outcomes(rs []Result) []Outcome {
	out := make([]Outcome, len(rs))
	for i, r := range rs {
		out[i] = r.Outcome
	}
	return out
}

// The old spellings still read, and never write.
//
// `block`, `warn` and `info` are in every stored result, every exported
// spreadsheet, every policy pack on disk - including ones this repository does
// not contain - and in the query string of any filter somebody bookmarked.
// Renaming the value without reading the old one turns a third-party pack into
// a load error and a saved link into an empty table, over a change that is
// entirely about the word.
func TestLegacySeveritySpellingsAreRead(t *testing.T) {
	for old, want := range map[string]Severity{
		"block":    SeverityCritical,
		"warn":     SeverityWarning,
		"info":     SeverityInform,
		"critical": SeverityCritical,
		"warning":  SeverityWarning,
		"inform":   SeverityInform,
		"  Block ": SeverityCritical,
	} {
		if got := ParseSeverity(old); got != want {
			t.Errorf("ParseSeverity(%q) = %q, want %q", old, got, want)
		}
	}
	// Anything else is returned as itself, so an unknown value is visible as
	// what it is rather than quietly becoming one of the three.
	if got := ParseSeverity("catastrophic"); got != Severity("catastrophic") || got.Valid() {
		t.Errorf("ParseSeverity(catastrophic) = %q, valid = %v", got, got.Valid())
	}

	// A pack written against the old vocabulary loads as the new one, because
	// that is the only place the translation happens.
	var c struct {
		Severity Severity `json:"severity"`
	}
	if err := json.Unmarshal([]byte(`{"severity":"block"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Severity != SeverityCritical {
		t.Errorf("a pack saying block loaded as %q, want %q", c.Severity, SeverityCritical)
	}

	// And the value written back out is the current one, so the old vocabulary
	// drains rather than persisting.
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"severity":"critical"}` {
		t.Errorf("marshalled as %s, want the current spelling", out)
	}
}
