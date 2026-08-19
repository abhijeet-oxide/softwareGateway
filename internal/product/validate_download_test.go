package product

import (
	"strings"
	"testing"
)

// withSplitBlocks is the reference topology written the new way: a download
// that says where things go, and a rule that says which tags.
func withSplitBlocks() *Product {
	p := withQuayChain()
	rule := p.Spec.AutoDownload.Rules[0]
	prio := 100

	p.Spec.Download = []Download{{
		Targets:  []string{"ocp-prod"},
		Priority: &prio,
	}}
	p.Spec.AutoDownload = AutoDownload{
		Enabled: true,
		Rules:   []Rule{{Name: rule.Name, TagPattern: rule.TagPattern}},
	}
	return p
}

func TestSplitBlocksValidate(t *testing.T) {
	if err := withSplitBlocks().Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A download holds no pattern, and a rule holds nothing but one
// ---------------------------------------------------------------------------

// A rule with no pattern selects nothing, and the message has to say that
// downloading by hand needs no rule at all - otherwise the obvious fix is to
// invent a pattern that matches everything.
func TestRuleWithoutAPatternIsRejected(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.AutoDownload.Rules[0].TagPattern = ""

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].tagPattern")
	if !strings.Contains(e.Hint, "by hand") {
		t.Fatalf("the hint must say a manual download needs no rule, got %q", e.Hint)
	}
}

func TestRuleNamingAnUnknownDownloadIsRejected(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download[0].Name = "to-production"
	p.Spec.AutoDownload.Rules[0].Download = "nowhere"

	requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].download")
}

// Naming a download AND carrying inline targets is two answers to one
// question.
func TestRuleCannotBothNameADownloadAndCarryTargets(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download[0].Name = "to-production"
	p.Spec.AutoDownload.Rules[0].Download = "to-production"
	p.Spec.AutoDownload.Rules[0].Targets = []string{"jfrog-store"}

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0]")
	if !strings.Contains(e.Hint, "older spelling") {
		t.Fatalf("got %q", e.Hint)
	}
}

// ---------------------------------------------------------------------------
// One download is the default by being the only one
// ---------------------------------------------------------------------------

func TestSingleDownloadNeedsNoNameOrDefault(t *testing.T) {
	p := withSplitBlocks()
	if err := p.Validate(nil); err != nil {
		t.Fatalf("one unnamed download is complete on its own: %v", err)
	}
	d, ok := p.DefaultDownload()
	if !ok || d.Name != "" {
		t.Fatalf("DefaultDownload = (%+v, %v)", d, ok)
	}
}

// Picking by list order would be a decision no reader could see.
func TestSeveralDownloadsNeedOneMarkedDefault(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download = []Download{
		{Name: "to-production", Targets: []string{"ocp-prod"}},
		{Name: "to-storage", Targets: []string{"jfrog-store"}},
	}

	e := requireField(t, p.Validate(nil), "spec.download")
	if !strings.Contains(e.Hint, "default: true") {
		t.Fatalf("the hint must name the fix, got %q", e.Hint)
	}

	p.Spec.Download[0].Default = true
	if err := p.Validate(nil); err != nil {
		t.Fatalf("marking one default resolves it: %v", err)
	}
	d, _ := p.DefaultDownload()
	if d.Name != "to-production" {
		t.Fatalf("DefaultDownload = %q", d.Name)
	}
}

func TestTwoDefaultsAreRejected(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download = []Download{
		{Name: "a", Targets: []string{"jfrog-store"}, Default: true},
		{Name: "b", Targets: []string{"jfrog-store"}, Default: true},
	}
	requireField(t, p.Validate(nil), "spec.download")
}

func TestSeveralDownloadsNeedNames(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download = []Download{
		{Targets: []string{"jfrog-store"}, Default: true},
		{Name: "to-production", Targets: []string{"ocp-prod"}},
	}
	requireField(t, p.Validate(nil), "spec.download[0].name")
}

func TestDuplicateDownloadNamesAreRejected(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download = []Download{
		{Name: "a", Targets: []string{"jfrog-store"}, Default: true},
		{Name: "a", Targets: []string{"ocp-prod"}},
	}
	requireField(t, p.Validate(nil), "spec.download[1].name")
}

// ---------------------------------------------------------------------------
// The compatibility contract: every document written before the split works
// ---------------------------------------------------------------------------

// A rule carrying its own targets describes a download inline, which is what
// every document written before this split does.
func TestLegacyRuleWithInlineTargetsStillWorks(t *testing.T) {
	p := withQuayChain() // rules carry targets, no spec.download at all
	if err := p.Validate(nil); err != nil {
		t.Fatalf("the older shape must keep working: %v", err)
	}

	d, err := p.DownloadFor(p.Spec.AutoDownload.Rules[0])
	if err != nil {
		t.Fatalf("DownloadFor: %v", err)
	}
	if strings.Join(d.Targets, ",") != "jfrog-store,ocp-prod" {
		t.Fatalf("the inline download must carry the rule's targets, got %v", d.Targets)
	}
	if d.EffectivePriority() != 100 {
		t.Fatalf("and its priority, got %d", d.EffectivePriority())
	}
}

func TestLegacyVerifyBeforeTransferBecomesTheDownloadsGate(t *testing.T) {
	p := withQuayChain()
	on := true
	p.Spec.AutoDownload.Rules[0].VerifyBeforeTransfer = &on

	d, err := p.DownloadFor(p.Spec.AutoDownload.Rules[0])
	if err != nil {
		t.Fatalf("DownloadFor: %v", err)
	}
	if got := d.VerifiesBefore(); got == nil || !*got {
		t.Fatal("verifyBeforeTransfer must survive as the download's before-gate")
	}
}

// A rule naming nothing gets the default download, and with no downloads
// declared, the product's default target - which is what it has always meant.
func TestRuleNamingNothingFallsBackTwice(t *testing.T) {
	p := withQuayChain()
	p.Spec.AutoDownload.Rules[0].Targets = nil
	p.Spec.AutoDownload.Rules[0].Priority = nil

	d, err := p.DownloadFor(p.Spec.AutoDownload.Rules[0])
	if err != nil {
		t.Fatalf("DownloadFor: %v", err)
	}
	if strings.Join(d.Targets, ",") != "jfrog-store" {
		t.Fatalf("expected the default target, got %v", d.Targets)
	}

	p.Spec.Download = []Download{{Targets: []string{"ocp-prod"}}}
	d, err = p.DownloadFor(p.Spec.AutoDownload.Rules[0])
	if err != nil {
		t.Fatalf("DownloadFor: %v", err)
	}
	if strings.Join(d.Targets, ",") != "ocp-prod" {
		t.Fatalf("a declared default download wins over the default target, got %v", d.Targets)
	}
}

// ---------------------------------------------------------------------------
// A download's targets
// ---------------------------------------------------------------------------

func TestDownloadTargetsMustExistAndBeUsable(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Download[0].Targets = []string{"nowhere"}
	requireField(t, p.Validate(nil), "spec.download[0].targets[0]")

	p = withSplitBlocks()
	p.Spec.Targets[1].PromotionOnly = true
	p.Spec.Targets[1].Environment = ""
	requireField(t, p.Validate(nil), "spec.download[0].targets[0]")
}

// A hop the closure reaches is as load-bearing as a named destination.
func TestDisabledHopIsRejected(t *testing.T) {
	p := withSplitBlocks()
	off := false
	p.Spec.Targets[0].Enabled = &off
	p.Spec.Targets[0].Default = false

	err := p.Validate(nil)
	errs, _ := AsErrors(err)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "pulls from") && strings.Contains(e.Message, "disabled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the hop to be reported, got %v", fieldsOf(t, err))
	}
}

func TestChainTerminatesOnACycle(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Targets[0].Type = RegistryQuay
	p.Spec.Targets[0].Quay = &QuaySettings{APITokenRef: &SecretRef{SecretName: "quay-api", Key: "token"}}
	p.Spec.Targets[0].Repository = "platform-prod/store"
	p.Spec.Targets[0].Replication = &Replication{Mode: ReplicationMirror, Mirror: &MirrorConfig{
		From: "ocp-prod", Tags: []string{"v3.*"}, Robot: "platform-prod+swgw",
	}}

	got := p.chainOf("ocp-prod", targetsByName(p))
	if len(got) != 2 {
		t.Fatalf("a cycle must terminate after visiting each target once, got %v", got)
	}
}

func targetsByName(p *Product) map[string]Target {
	out := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		out[t.Name] = t
	}
	return out
}

// ---------------------------------------------------------------------------
// The cross-block check neither half can make alone
// ---------------------------------------------------------------------------

func TestVerifyAfterOverASignatureBlindMirrorIsAnError(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	on := true
	p.Spec.Download[0].Verify = &DownloadVerification{After: &on, Policy: PolicyEnforce}

	e := requireField(t, p.Validate(nil), "spec.download[0].targets[0]")
	if !strings.Contains(e.Hint, "sha256-") {
		t.Fatalf("the hint must name the tag form, got %q", e.Hint)
	}
	if !strings.Contains(e.Hint, "report success") {
		t.Fatalf("the hint must say the sync SUCCEEDS, got %q", e.Hint)
	}
}

func TestNoErrorWhenSignaturesAreMirrored(t *testing.T) {
	p := withSplitBlocks() // tags already carry 'sha256-*.sig'
	on := true
	p.Spec.Download[0].Verify = &DownloadVerification{After: &on, Policy: PolicyEnforce}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// Without verify.after the mirror is nobody's problem: it warns rather than
// failing, because a product that does not verify at the destination has no
// reason to mirror signatures.
func TestNoErrorWithoutVerifyAfter(t *testing.T) {
	p := withSplitBlocks()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// The check follows the legacy inline shape too, because that is a download.
func TestSignatureCheckCoversAnInlineDownload(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	on := true
	p.Spec.AutoDownload.Rules[0].Targets = []string{"ocp-prod"}
	p.Spec.AutoDownload.Rules[0].VerifyBeforeTransfer = nil
	// The inline shape has no `after`, so this is expressed the only way it
	// can be: through a declared download the rule names.
	p.Spec.Download = []Download{{
		Name: "to-production", Targets: []string{"ocp-prod"},
		Verify: &DownloadVerification{After: &on},
	}}
	p.Spec.AutoDownload.Rules[0].Targets = nil
	p.Spec.AutoDownload.Rules[0].Priority = nil
	p.Spec.AutoDownload.Rules[0].Download = "to-production"

	requireField(t, p.Validate(nil), "spec.download[0].targets[0]")
}
