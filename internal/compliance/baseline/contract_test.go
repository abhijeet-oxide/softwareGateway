package baseline_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The output contract.
//
// # Why these are assertions and not review notes
//
// Both of the failures below happened, shipped, and were found by a person
// reading a report rather than by CI:
//
//  1. A pack stopped compiling and its checks silently vanished from the run.
//     Nothing failed - the good fixture was still clean, because a check that
//     does not exist cannot fire - and the report simply had a category missing
//     from it. Absence and compliance are indistinguishable on the screen a
//     release manager reads, which is the one thing this package exists to
//     prevent.
//
//  2. Two checks emitted findings with an empty observed value, one of them
//     with an unsubstituted fragment where the value should have been. Both
//     were blocking, so they were among the first rows a reviewer opened, and
//     neither told the reader what was actually wrong.
//
// Each is one assertion, and each would have caught its own defect before
// release.

// A pack that fails to load takes its checks with it. That must fail the build,
// not the report.
func TestEveryShippedPackLoads(t *testing.T) {
	cat := loadShipped(t)
	total := 0
	for _, p := range cat.Packs() {
		if !p.OK() {
			t.Errorf("pack %s did not load cleanly, so the checks it owns are missing from every run:", p.Name)
			for _, e := range p.Errors {
				t.Errorf("    %s", e)
			}
		}
		if p.Checks == 0 {
			t.Errorf("pack %s registered no checks", p.Name)
		}
		total += p.Checks
	}
	if total != cat.Len() {
		t.Errorf("packs report %d checks but the catalogue holds %d", total, cat.Len())
	}
	t.Logf("%d packs, %d checks", len(cat.Packs()), cat.Len())
}

// Every finding has to say what was actually seen.
//
// A row naming a field and leaving the value blank makes the reader open the
// chart to learn what the tool already knew, and a row containing a template
// fragment tells them the tool is broken. Neither is worth emitting.
func TestFindingsCarryTheValueTheyJudged(t *testing.T) {
	fixtures := []string{
		"bad-config.yaml", "bad-cronjob.yaml", "bad-metadata.yaml", "bad-network.yaml",
		"bad-observability.yaml", "bad-pdb.yaml", "bad-probes.yaml", "bad-rbac.yaml",
		"bad-resources.yaml", "bad-scheduling.yaml", "bad-security.yaml",
		"bad-storage.yaml", "bad-supply.yaml", "bad-upgrade.yaml",
	}
	// Fragments that mean a value never got substituted, or that a list was
	// rendered through a conversion that produces nothing.
	broken := []string{"{{", "}}", "<no value>", "%!", "(MISSING)"}

	var problems []string
	for _, f := range fixtures {
		for _, r := range runFixture(t, f) {
			if r.Outcome != compliance.OutcomeFail {
				continue
			}
			where := r.CheckID + " @ " + f + " " + r.Address.Kind + "/" + r.Address.Name
			if r.Address.Locus != "" && strings.TrimSpace(r.Observed) == "" {
				problems = append(problems, where+": names "+r.Address.Locus+" and reports no value for it")
			}
			for _, frag := range broken {
				if strings.Contains(r.Observed, frag) {
					problems = append(problems, where+": observed value contains "+frag+" - "+r.Observed)
				}
				if strings.Contains(r.Message, frag) {
					problems = append(problems, where+": message contains "+frag+" - "+r.Message)
				}
			}
			if strings.TrimSpace(r.Message) == "" {
				problems = append(problems, where+": no message")
			}
			if strings.TrimSpace(r.Remediation) == "" {
				problems = append(problems, where+": no remediation, so the finding states a defect with no fix")
			}
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// A check that exists to REPORT something says it on a pass.
//
// The alternative is what the audit found: an inventory check has to fail on
// every subject in order to say anything, so three checks produced roughly a
// third of a report's rows and close to none of its defects. `observeOnPass`
// is the mechanism that separates the two, and this asserts it works - because
// a silent pass row is indistinguishable from the check not existing.
func TestInventoryChecksSayWhatTheySawOnAPass(t *testing.T) {
	want := map[string]string{
		// The tasks Helm runs outside the ordinary install. Nothing else in a
		// compliance report shows them: hooks do not appear among the deployed
		// objects.
		"UPG-08": "Helm runs this Job at: pre-install, pre-upgrade",
	}
	seen := map[string]bool{}
	for _, r := range runFixture(t, "good-app.yaml") {
		if r.Outcome != compliance.OutcomePass {
			continue
		}
		expect, ok := want[r.CheckID]
		if !ok {
			continue
		}
		seen[r.CheckID] = true
		if r.Observed != expect {
			t.Errorf("%s passed and recorded %q, want %q", r.CheckID, r.Observed, expect)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s produced no passing result on the good fixture", id)
		}
	}
}

// Every check has to carry the fields a non-specialist reader needs, and the
// severity rubric has to hold. Both are cheap to state and impossible to keep
// by convention across a hundred checks in thirteen files.
func TestEveryCheckIsTriageable(t *testing.T) {
	for _, c := range loadShipped(t).Checks() {
		if c.Deprecated {
			continue
		}
		miss := func(field string) { t.Errorf("%s (%s): no %s", c.ID, c.Title, field) }
		if c.Confidence == "" {
			miss("confidence")
		}
		if c.WhenItBites == "" {
			miss("whenItBites - a reader cannot prioritise a finding with no timing")
		}
		if c.FixOwner == "" {
			miss("fixOwner - the finding cannot be routed to anybody")
		}
		if c.FixEffort == "" {
			miss("fixEffort")
		}
		if strings.TrimSpace(c.Remediation) == "" {
			miss("remediation")
		}
		if strings.TrimSpace(c.Rationale) == "" {
			miss("rationale - the finding says what is wrong and not why it matters")
		}
		if strings.TrimSpace(c.Reference) == "" {
			miss("reference to the clause of the standard it enforces")
		}
		if c.Severity == compliance.SeverityBlock && c.Confidence == compliance.ConfidenceNeedsReview {
			t.Errorf("%s: blocks a release on a finding it says needs a human to judge", c.ID)
		}
	}
}
