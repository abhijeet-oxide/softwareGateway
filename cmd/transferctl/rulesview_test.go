package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func rule() v1.DownloadRuleView {
	return v1.DownloadRuleView{
		Product: "vendor-a", Name: "ga-releases",
		TagPattern: `^v\d+\.\d+\.\d+$`,
		Targets:    []string{"ocp-prod"},
		Chain:      []string{"jfrog-store", "ocp-prod"},
		ChainText:  "jfrog-store → ocp-prod",
		Trigger:    []string{"discovery", "manual"},
		Priority:   100, Enabled: true, Runnable: true,
		VerifyBefore: "true", VerifyAfter: "true", VerifyPolicy: "enforce",
		Revision: "abc123",
	}
}

func renderRule(t *testing.T, r v1.DownloadRuleView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRuleDetail(&buf, &r); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func renderRules(t *testing.T, rules ...v1.DownloadRuleView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRuleList(&buf, &v1.ListDownloadRulesResponse{Rules: rules}); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// The chain is implicit to type and never implicit to read: a rule naming one
// target that plans two steps has to say so.
func TestDescribeShowsTheDerivedChain(t *testing.T) {
	out := renderRule(t, rule())

	if !strings.Contains(out, "jfrog-store → ocp-prod") {
		t.Fatalf("the chain must be rendered:\n%s", out)
	}
	if !strings.Contains(out, "pulled in by what those targets mirror from") {
		t.Fatalf("a hop the rule did not name must be explained:\n%s", out)
	}
}

func TestDescribeIsQuietWhenTheRuleNamedEveryHop(t *testing.T) {
	r := rule()
	r.Targets = []string{"jfrog-store", "ocp-prod"}

	if strings.Contains(renderRule(t, r), "pulled in by") {
		t.Fatal("nothing was pulled in; the rule named both")
	}
}

// Under enforce, the security property is the thing a reader most needs, and
// it should not have to be inferred from two fields.
func TestDescribeStatesTheGate(t *testing.T) {
	out := renderRule(t, rule())
	if !strings.Contains(out, "stops every") || !strings.Contains(out, "Nothing downstream is configured") {
		t.Fatalf("expected the gate to be stated:\n%s", out)
	}
}

func TestDescribeOmitsTheGateWhenItIsNotInForce(t *testing.T) {
	r := rule()
	r.VerifyPolicy = "warn"
	if strings.Contains(renderRule(t, r), "stops every") {
		t.Fatal("under warn the chain proceeds; saying otherwise would be false")
	}
}

// Inherited is not the same as off, and rendering both as false would tell the
// reader the opposite of the truth.
func TestInheritedVerificationRendersAsInherit(t *testing.T) {
	r := rule()
	r.VerifyBefore, r.VerifyAfter, r.VerifyPolicy = "inherit", "inherit", ""

	out := renderRule(t, r)
	if !strings.Contains(out, "before the transfer   inherit") {
		t.Fatalf("expected inherit:\n%s", out)
	}
	if strings.Contains(out, "false") {
		t.Fatalf("inherited must not render as off:\n%s", out)
	}
}

// A suspension reports BOTH facts, because both are true.
func TestSuspendedRuleShowsConfigurationAndOverride(t *testing.T) {
	r := rule()
	r.Suspended = true
	r.Runnable = false
	r.SuspendedBy = "alice@example.com"
	r.SuspendedReason = "vendor shipped a broken build"

	out := renderRule(t, r)
	if !strings.Contains(out, "configuration says    enabled") {
		t.Fatalf("Git's answer must still be shown:\n%s", out)
	}
	if !strings.Contains(out, "SUSPENDED by          alice@example.com") {
		t.Fatalf("the override must name who:\n%s", out)
	}
	if !strings.Contains(out, "Configuration was not changed") {
		t.Fatalf("the reader must be told nothing was committed:\n%s", out)
	}
}

// An indefinite override is the failure mode; say so rather than leaving a
// blank.
func TestIndefiniteSuspensionSaysSo(t *testing.T) {
	r := rule()
	r.Suspended = true
	r.SuspendedReason = "incident"

	if !strings.Contains(renderRule(t, r), "nothing will lift this on its own") {
		t.Fatalf("expected the standing complaint:\n%s", renderRule(t, r))
	}
}

// The state column reports the situation, not the flag.
func TestRuleStatePrecedence(t *testing.T) {
	cases := []struct {
		name string
		view v1.DownloadRuleView
		want string
	}{
		{"broken", v1.DownloadRuleView{ChainError: "cycle", Enabled: true}, "BROKEN"},
		{"both", v1.DownloadRuleView{Suspended: true}, "suspended (also disabled in Git)"},
		{"suspended", v1.DownloadRuleView{Suspended: true, Enabled: true}, "SUSPENDED"},
		{"disabled", v1.DownloadRuleView{}, "disabled in Git"},
		{"active", v1.DownloadRuleView{Enabled: true}, "active"},
	}
	for _, c := range cases {
		if got := ruleState(c.view); got != c.want {
			t.Fatalf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestListExplainsASuspensionOnce(t *testing.T) {
	r := rule()
	r.Suspended = true

	out := renderRules(t, r)
	if !strings.Contains(out, "not a configuration change") {
		t.Fatalf("expected the explanation:\n%s", out)
	}
	if !strings.Contains(out, "rules describe") {
		t.Fatalf("expected the next step:\n%s", out)
	}
}

func TestListSaysNothingAboutSuspensionWhenNoneIsInForce(t *testing.T) {
	if strings.Contains(renderRules(t, rule()), "not a configuration change") {
		t.Fatal("no suspension, no explanation")
	}
}

// A broken chain must not blank the listing.
func TestBrokenRuleStillListsWithItsReason(t *testing.T) {
	r := rule()
	r.ChainError = "targets form a mirror.from cycle through \"ocp-prod\""
	r.ChainText = ""

	out := renderRules(t, r)
	if !strings.Contains(out, "cycle") || !strings.Contains(out, "BROKEN") {
		t.Fatalf("expected the reason on the row:\n%s", out)
	}
}

// Asking twice for the same work is the normal result of a re-run.
func TestRunReportsAlreadyRequestedAsNormal(t *testing.T) {
	var buf bytes.Buffer
	err := renderRuleRun(&buf, &v1.RunDownloadRuleResponse{
		Product: "vendor-a", Rule: "ga-releases",
		Chain:   []string{"jfrog-store", "ocp-prod"},
		Matched: []string{"v3.2.1"}, AlreadyRequested: []string{"v3.2.1"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "already requested") {
		t.Fatalf("expected it to be reported:\n%s", out)
	}
	for _, alarming := range []string{"error", "failed", "Error"} {
		if strings.Contains(out, alarming) {
			t.Fatalf("this is a normal outcome and must not read as a failure: %q in\n%s", alarming, out)
		}
	}
}

func TestRunDryRunSaysNothingWasCreated(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRuleRun(&buf, &v1.RunDownloadRuleResponse{
		Product: "vendor-a", Rule: "ga-releases",
		Chain: []string{"jfrog-store"}, Matched: []string{"v3.2.1"}, ValidateOnly: true,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing was created") {
		t.Fatalf("a dry run must say so:\n%s", buf.String())
	}
}

func TestRunWithNoMatchesSaysWhy(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRuleRun(&buf, &v1.RunDownloadRuleResponse{
		Product: "vendor-a", Rule: "ga-releases", Chain: []string{"jfrog-store"},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "Nothing matched") {
		t.Fatalf("an empty run must explain itself:\n%s", buf.String())
	}
}
