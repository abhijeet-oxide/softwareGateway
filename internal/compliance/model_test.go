package compliance

import "testing"

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
		{Outcome: OutcomePass, Severity: SeverityBlock},
		{Outcome: OutcomeFail, Severity: SeverityBlock},
		{Outcome: OutcomeFail, Severity: SeverityWarn},
		{Outcome: OutcomeSkip, Severity: SeverityBlock},
		{Outcome: OutcomeError, Severity: SeverityBlock},
		{Outcome: OutcomeWaived, Severity: SeverityBlock},
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
		mk(OutcomePass, SeverityBlock, "a", "x", "C-01"),
		mk(OutcomeFail, SeverityWarn, "b", "y", "B-01"),
		mk(OutcomeError, SeverityBlock, "a", "z", "A-01"),
		mk(OutcomeFail, SeverityBlock, "c", "w", "D-01"),
		mk(OutcomeSkip, SeverityInfo, "a", "v", "E-01"),
	}
	Sort(in)
	want := []Outcome{OutcomeFail, OutcomeFail, OutcomeError, OutcomePass, OutcomeSkip}
	for i, w := range want {
		if in[i].Outcome != w {
			t.Fatalf("position %d is %s, want %s (order: %v)", i, in[i].Outcome, w, outcomes(in))
		}
	}
	// Within failures, blocking comes before warning.
	if in[0].Severity != SeverityBlock {
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
