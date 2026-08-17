package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func renderList(t *testing.T, resp *v1.ListReplicationResponse) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderTargetList(&buf, resp); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func renderDetail(t *testing.T, v *v1.ReplicationView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderTargetDetail(&buf, v); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// The rule from docs/design/18 §6.1, enforced where a reader would notice it
// breaking: no speed, no percentage, no ETA for a target whose bytes we do not
// move.
func TestListNeverShowsProgressForADelegatedTarget(t *testing.T) {
	out := renderList(t, &v1.ListReplicationResponse{Targets: []v1.ReplicationView{{
		Target: "ocp-prod", Mode: "mirror",
		Desired:  &v1.ReplicationDesired{Upstream: "artifactory.example.com/x", Interval: "6h0m0s"},
		Observed: &v1.ReplicationObserved{Configured: true, SyncStatus: "SYNCING"},
	}}})

	// Checked against the TABLE only. The prose below it mentions elapsed time
	// precisely to say that nothing is derived from it, and asserting over the
	// whole output would fail on the sentence that states the rule.
	table, _, _ := strings.Cut(out, "\n\n")
	for _, forbidden := range []string{"%", "MB/s", "ETA", "elapsed"} {
		if strings.Contains(table, forbidden) {
			t.Fatalf("the table must not imply progress, found %q in:\n%s", forbidden, table)
		}
	}
	if !strings.Contains(out, "SYNCING") {
		t.Fatalf("the state is what we DO have, and it must be shown:\n%s", out)
	}
	if !strings.Contains(out, "STATE, not a speed") {
		t.Fatalf("the absence of byte columns should be explained once:\n%s", out)
	}
}

// A copy target has nothing delegated, so the explanation would be noise.
func TestListSaysNothingAboutDelegationWhenNothingIsDelegated(t *testing.T) {
	out := renderList(t, &v1.ListReplicationResponse{Targets: []v1.ReplicationView{
		{Target: "lab", Mode: "copy"},
	}})
	if strings.Contains(out, "STATE, not a speed") {
		t.Fatalf("no delegated target, so no explanation:\n%s", out)
	}
	if !strings.Contains(out, "n/a") {
		t.Fatalf("a copy target's replication state is n/a, not blank:\n%s", out)
	}
}

// An unrecognised registry status must reach the reader with its raw value, so
// it can be looked up rather than dismissed.
func TestUnknownSyncStatusCarriesTheRegistrysOwnWord(t *testing.T) {
	out := renderList(t, &v1.ListReplicationResponse{Targets: []v1.ReplicationView{{
		Target: "ocp-prod", Mode: "mirror",
		Observed: &v1.ReplicationObserved{Configured: true, SyncStatus: "UNKNOWN", SyncStatusRaw: "QUIESCING"},
	}}})
	if !strings.Contains(out, "QUIESCING") {
		t.Fatalf("the raw status must survive to the screen:\n%s", out)
	}
}

// The state column reports the thing to act on, worst first.
func TestStatePrecedence(t *testing.T) {
	cases := []struct {
		name string
		view v1.ReplicationView
		want string
	}{
		{"copy", v1.ReplicationView{Mode: "copy"}, "n/a"},
		{"unreachable", v1.ReplicationView{Mode: "mirror", Unreachable: "connection refused",
			PendingApply: true}, "unreachable"},
		{"absent", v1.ReplicationView{Mode: "mirror",
			Drift: &v1.ReplicationDrift{Detected: true, Absent: true}}, "not applied"},
		{"drifted", v1.ReplicationView{Mode: "mirror",
			Drift: &v1.ReplicationDrift{Detected: true}, PendingApply: true}, "DRIFTED"},
		{"pending", v1.ReplicationView{Mode: "mirror", PendingApply: true,
			Drift: &v1.ReplicationDrift{}}, "pending apply"},
		{"in sync", v1.ReplicationView{Mode: "mirror", Drift: &v1.ReplicationDrift{}}, "in sync"},
	}
	for _, c := range cases {
		if got := replicationState(c.view); got != c.want {
			t.Fatalf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}
}

// Defence three of three against the signature trap: visible on the screen
// rather than inferred from a field nobody reads.
func TestDescribeWarnsWhenGlobsCannotMatchSignatures(t *testing.T) {
	out := renderDetail(t, &v1.ReplicationView{
		Product: "vendor-a", Target: "ocp-prod", Mode: "mirror",
		Desired: &v1.ReplicationDesired{Tags: []string{"v3.*"}},
	})
	if !strings.Contains(out, "sha256-*.sig") {
		t.Fatalf("the warning must name the tag form being missed:\n%s", out)
	}
	if !strings.Contains(out, "signatures will not") {
		t.Fatalf("the warning must say what the consequence is:\n%s", out)
	}
}

func TestDescribeIsQuietWhenSignaturesAreCovered(t *testing.T) {
	for _, tags := range [][]string{{"v3.*", "sha256-*.sig"}, {"*"}} {
		out := renderDetail(t, &v1.ReplicationView{
			Product: "vendor-a", Target: "ocp-prod", Mode: "mirror",
			Desired: &v1.ReplicationDesired{Tags: tags},
		})
		if strings.Contains(out, "signatures will not") {
			t.Fatalf("tags %v cover signatures; no warning expected:\n%s", tags, out)
		}
	}
}

// "Never applied" is a state with an obvious next step, and the output should
// say what it is.
func TestDescribeNamesTheNextStepWhenNeverApplied(t *testing.T) {
	out := renderDetail(t, &v1.ReplicationView{
		Product: "vendor-a", Target: "ocp-prod", Mode: "mirror",
		Desired: &v1.ReplicationDesired{Tags: []string{"*"}},
	})
	if !strings.Contains(out, "never applied") || !strings.Contains(out, "targets apply") {
		t.Fatalf("expected the next step to be named:\n%s", out)
	}
}

// Drift is never closed behind your back, and the output says so where
// somebody would otherwise wait for it to fix itself.
func TestDescribeSaysDriftIsNotSelfHealing(t *testing.T) {
	out := renderDetail(t, &v1.ReplicationView{
		Product: "vendor-a", Target: "ocp-prod", Mode: "mirror",
		Desired: &v1.ReplicationDesired{Tags: []string{"*"}},
		Drift: &v1.ReplicationDrift{
			Detected: true, Summary: "tags differ",
			Fields: []v1.ReplicationField{{Field: "tags", Want: "[*]", Got: "[v3.*]"}},
		},
	})
	if !strings.Contains(out, "never closed automatically") {
		t.Fatalf("expected the non-self-healing rule to be stated:\n%s", out)
	}
	if !strings.Contains(out, "want [*], got [v3.*]") {
		t.Fatalf("the field diff must be rendered:\n%s", out)
	}
}

// A copy target should not be given a section it cannot have.
func TestDescribeCopyTargetSaysWhatItIsInstead(t *testing.T) {
	out := renderDetail(t, &v1.ReplicationView{Product: "vendor-a", Target: "lab", Mode: "copy"})
	if strings.Contains(out, "CONFIGURED") || strings.Contains(out, "OBSERVED") {
		t.Fatalf("a copy target has no registry-side configuration:\n%s", out)
	}
	if !strings.Contains(out, "nothing to apply or sync") {
		t.Fatalf("expected the explanation:\n%s", out)
	}
}

// A destructive plan that was not applied must say both things: what it would
// do, and that it did not do it.
func TestApplyPlanSaysWhatWasNotDone(t *testing.T) {
	var buf bytes.Buffer
	err := renderApplyPlan(&buf, &v1.ApplyReplicationResponse{
		Product: "vendor-a", Target: "ocp-prod", Mode: "mirror",
		Steps:       []string{"set repository platform-prod/x state NORMAL -> MIRROR, which makes it READ-ONLY to everyone but platform-prod+swgw"},
		Destructive: true,
	}, false)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "READ-ONLY") {
		t.Fatalf("the step must survive to the screen:\n%s", out)
	}
	if !strings.Contains(out, "destructive") {
		t.Fatalf("the warning must be shown:\n%s", out)
	}
	if strings.Contains(out, "Applied.") {
		t.Fatalf("nothing was applied and the output must not say otherwise:\n%s", out)
	}
}

func TestApplyPlanDryRunSaysNothingChanged(t *testing.T) {
	var buf bytes.Buffer
	if err := renderApplyPlan(&buf, &v1.ApplyReplicationResponse{
		Product: "vendor-a", Target: "ocp-prod", Mode: "mirror",
		Steps: []string{"update the mirror configuration:"},
	}, true); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing was changed") {
		t.Fatalf("a dry run must say so:\n%s", buf.String())
	}
}
