package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func renderDownloads(t *testing.T, resp *v1.ListDownloadsResponse) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderDownloadList(&buf, resp); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func renderRules(t *testing.T, resp *v1.ListAutoDownloadRulesResponse) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRuleList(&buf, resp); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// A download is not a rule, and the listing must not read like one. There is
// no pattern to show, because by the time a download runs the software has
// already been chosen.
func TestDownloadListingShowsNoPattern(t *testing.T) {
	out := renderDownloads(t, &v1.ListDownloadsResponse{Downloads: []v1.DownloadView{{
		Product: "vendor-a", Name: "internal", Default: true,
		Targets: []string{"ocp-prod"}, Chain: []string{"lab", "ocp-prod"},
		ChainText: "lab → ocp-prod", Priority: 100,
		VerifyBefore: "true", VerifyAfter: "true", VerifyPolicy: "enforce",
	}}})

	for _, forbidden := range []string{"pattern", "PATTERN", "matches", "MATCHES"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("a download has no %q:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "(default)") {
		t.Fatalf("the default is which one a bare `download` runs, so it has to be marked:\n%s", out)
	}
}

// The chain is derived, not declared: a download naming one target that plans
// three steps has to say where the other two came from.
func TestDownloadListingExplainsATargetItWasNotGiven(t *testing.T) {
	out := renderDownloads(t, &v1.ListDownloadsResponse{Downloads: []v1.DownloadView{{
		Name: "internal", Targets: []string{"ocp-prod"},
		Chain: []string{"lab", "ocp-prod"}, ChainText: "lab → ocp-prod",
		VerifyBefore: "inherit", VerifyAfter: "inherit",
	}}})

	if !strings.Contains(out, "lab → ocp-prod") {
		t.Fatalf("chain missing:\n%s", out)
	}
	if !strings.Contains(out, "names ocp-prod") || !strings.Contains(out, "mirror from") {
		t.Fatalf("a step nobody asked for must be attributed:\n%s", out)
	}
}

// A download whose chain will not resolve is the one worth reading about, so
// it must survive the listing rather than blank it.
func TestABrokenChainIsShownAndDoesNotHideTheRest(t *testing.T) {
	out := renderDownloads(t, &v1.ListDownloadsResponse{Downloads: []v1.DownloadView{
		{Name: "broken", Targets: []string{"gone"}, ChainError: `unknown target "gone"`},
		{Name: "internal", Default: true, Targets: []string{"lab"}, ChainText: "lab"},
	}})

	if !strings.Contains(out, `unknown target "gone"`) {
		t.Fatalf("the reason must be printed:\n%s", out)
	}
	if !strings.Contains(out, "internal") {
		t.Fatalf("one broken download must not swallow the others:\n%s", out)
	}
}

// Verification is tri-state on the wire, and "said nothing" is not "turned it
// off" — rendering the two the same would misreport a product's trust posture.
func TestInheritedVerificationIsNotRenderedAsOff(t *testing.T) {
	out := renderDownloads(t, &v1.ListDownloadsResponse{Downloads: []v1.DownloadView{{
		Name: "internal", Targets: []string{"lab"}, ChainText: "lab",
		VerifyBefore: "inherit", VerifyAfter: "inherit",
	}}})

	if strings.Contains(out, "false") {
		t.Fatalf("nothing here was turned off:\n%s", out)
	}
	if !strings.Contains(out, "inherit") {
		t.Fatalf("inheritance has to be visible:\n%s", out)
	}
}

// A product with neither a download nor a default target can download nothing,
// and an empty listing that does not say so reads like "nothing configured yet".
func TestNoDownloadsSaysWhatThatMeans(t *testing.T) {
	out := renderDownloads(t, &v1.ListDownloadsResponse{})
	if !strings.Contains(out, "Nothing can be downloaded") {
		t.Fatalf("the consequence has to be stated:\n%s", out)
	}
}

// A rule's whole job is the pattern and which download it fires. Everything
// about where software goes belongs to the download it names.
func TestRuleListingNamesTheDownloadItTriggers(t *testing.T) {
	out := renderRules(t, &v1.ListAutoDownloadRulesResponse{Enabled: true, Rules: []v1.AutoDownloadRuleView{
		{Name: "ga", TagPattern: `^v\d+$`, Download: "internal", ChainText: "lab → ocp-prod", Enabled: true},
		{Name: "rc", TagPattern: `^v\d+-rc$`, Sources: []string{"primary"}, ChainText: "lab", Enabled: true},
		{Name: "old", TagPattern: `^orb_`, Inline: true, ChainText: "lab", Enabled: false},
	}})

	if !strings.Contains(out, "internal") {
		t.Fatalf("a named download must be shown:\n%s", out)
	}
	if !strings.Contains(out, "the default download") {
		t.Fatalf("a rule naming none triggers the default, and that is not nothing:\n%s", out)
	}
	if !strings.Contains(out, "(its own targets)") {
		t.Fatalf("the older inline spelling is why one rule's chain differs, so it must be named:\n%s", out)
	}
	if !strings.Contains(out, "every source") {
		t.Fatalf("an absent source list means all of them:\n%s", out)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("a disabled rule must not read as active:\n%s", out)
	}
}

// Three active-looking rules while nothing fires is the worst thing this
// listing can do, so the master switch is reported when it is off.
func TestTheMasterSwitchIsReportedWhenOff(t *testing.T) {
	resp := &v1.ListAutoDownloadRulesResponse{Rules: []v1.AutoDownloadRuleView{
		{Name: "ga", TagPattern: `^v\d+$`, Enabled: true, ChainText: "lab"},
	}}
	out := renderRules(t, resp)
	if !strings.Contains(out, "none of these fire on their own") {
		t.Fatalf("an off master switch has to be stated:\n%s", out)
	}
	if !strings.Contains(out, "by hand is unaffected") {
		t.Fatalf("the switch covers automatic firing only, and that is the reassuring half:\n%s", out)
	}

	resp.Enabled = true
	if strings.Contains(renderRules(t, resp), "none of these fire") {
		t.Fatal("an enabled block must not carry the warning")
	}
}

// A rule whose download will not resolve is broken now, not on the night it
// first matches.
func TestABrokenRuleIsCalledBroken(t *testing.T) {
	out := renderRules(t, &v1.ListAutoDownloadRulesResponse{Enabled: true, Rules: []v1.AutoDownloadRuleView{
		{Name: "ga", TagPattern: `^v\d+$`, Enabled: true, ChainError: `unknown download "nope"`},
	}})
	if !strings.Contains(out, "BROKEN") || !strings.Contains(out, `unknown download "nope"`) {
		t.Fatalf("state and reason both:\n%s", out)
	}
}

// Configuration comes from Git. A listing that offered a way to change it here
// would be advertising a second source of truth.
func TestListingsOfferNoRuntimeOverride(t *testing.T) {
	out := renderRules(t, &v1.ListAutoDownloadRulesResponse{Enabled: true, Rules: []v1.AutoDownloadRuleView{
		{Name: "ga", TagPattern: `^v\d+$`, Enabled: true, ChainText: "lab"},
	}})
	for _, forbidden := range []string{"suspend", "Suspend", "resume", "Resume"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("there is no runtime override, and %q implies one:\n%s", forbidden, out)
		}
	}
}

// A run says where the software is going before it says what it created —
// the chain is the part an operator has to check, and it is derived rather
// than typed.
func TestARunPrintsTheChainItResolved(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRunDownload(&buf, &v1.RunDownloadResponse{
		Product: "vendor-a", Download: "internal",
		Chain:   []string{"lab", "ocp-prod"},
		Created: []string{"req_8f2c"},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "lab → ocp-prod") {
		t.Fatalf("chain missing:\n%s", out)
	}
	if !strings.Contains(out, "req_8f2c") {
		t.Fatalf("the request id is what everything else is looked up by:\n%s", out)
	}
}

// Re-running after a timeout must not read like an error. The work is already
// happening, which is the answer the operator wanted.
func TestAlreadyRequestedIsNotAnError(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRunDownload(&buf, &v1.RunDownloadResponse{
		Product: "vendor-a", Chain: []string{"lab"},
		AlreadyRequested: []string{"v3.2.1"},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "already requested  v3.2.1") {
		t.Fatalf("the outcome has to be stated plainly:\n%s", out)
	}
	for _, forbidden := range []string{"error", "Error", "failed", "!"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("%q makes a normal outcome look like a problem:\n%s", forbidden, out)
		}
	}
}

// A dry run that printed request ids would be lying about what it did.
func TestADryRunSaysItCreatedNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRunDownload(&buf, &v1.RunDownloadResponse{
		Product: "vendor-a", Chain: []string{"lab", "ocp-prod"},
		Requested: []string{"v3.2.1"}, ValidateOnly: true,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "would download  v3.2.1") {
		t.Fatalf("a dry run reports intent:\n%s", out)
	}
	if !strings.Contains(out, "nothing was created") {
		t.Fatalf("and says that it is only intent:\n%s", out)
	}
}
