package product

import (
	"strings"
	"testing"
	"time"
)

// withDownloadBlock is the reference topology using the CURRENT spelling.
func withDownloadBlock() *Product {
	p := withQuayChain()
	rules := p.Spec.AutoDownload.Rules
	p.Spec.AutoDownload = AutoDownload{}
	p.Spec.Download = Download{Rules: rules}
	return p
}

func TestDownloadBlockValidates(t *testing.T) {
	if err := withDownloadBlock().Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The compatibility contract: every document valid before M9 stays valid and
// produces the same rules.
// ---------------------------------------------------------------------------

func TestAutoDownloadStillLoads(t *testing.T) {
	p := withQuayChain() // uses spec.autoDownload
	if err := p.Validate(nil); err != nil {
		t.Fatalf("the older spelling must keep working: %v", err)
	}
	if len(p.DownloadRules()) != 1 {
		t.Fatalf("rules = %d", len(p.DownloadRules()))
	}
	if !p.DownloadEnabled() {
		t.Fatal("autoDownload.enabled: true must read as enabled")
	}
	if got := p.DownloadBlockPath(); got != "spec.autoDownload" {
		t.Fatalf("errors must point at the block the reader wrote, got %q", got)
	}
}

// Errors have to name a line the reader actually has.
func TestErrorPathFollowsTheSpellingInUse(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].TagPattern = `^v(\d+`
	requireField(t, p.Validate(nil), "spec.download.rules[0].tagPattern")

	q := withQuayChain()
	q.Spec.AutoDownload.Rules[0].TagPattern = `^v(\d+`
	requireField(t, q.Validate(nil), "spec.autoDownload.rules[0].tagPattern")
}

// Two blocks meaning the same thing, resolved by precedence, is a document
// nobody can read out loud.
func TestBothSpellingsIsAnError(t *testing.T) {
	p := withQuayChain()
	p.Spec.Download = Download{Rules: p.Spec.AutoDownload.Rules}

	e := requireField(t, p.Validate(nil), "spec.download")
	if !strings.Contains(e.Message, "both") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestVerifyBeforeTransferIsTheOlderSpelling(t *testing.T) {
	on := true
	r := Rule{VerifyBeforeTransfer: &on}
	if got := r.VerifiesBefore(); got == nil || !*got {
		t.Fatal("verifyBeforeTransfer must still be honoured")
	}

	off := false
	r.Verify = &RuleVerification{Before: &off}
	if got := r.VerifiesBefore(); got == nil || *got {
		t.Fatal("verify.before supersedes the older spelling")
	}
}

// Stating one thing twice is how a document eventually disagrees with itself.
func TestBothVerifySpellingsIsAnError(t *testing.T) {
	p := withDownloadBlock()
	on := true
	p.Spec.Download.Rules[0].VerifyBeforeTransfer = &on
	p.Spec.Download.Rules[0].Verify = &RuleVerification{Before: &on}

	requireField(t, p.Validate(nil), "spec.download.rules[0].verifyBeforeTransfer")
}

// ---------------------------------------------------------------------------
// Triggers
// ---------------------------------------------------------------------------

func TestTriggerDefaultsToDiscovery(t *testing.T) {
	r := Rule{}
	if !r.TriggeredBy(TriggerDiscovery) {
		t.Fatal("an absent trigger means discovery, which is what every rule meant before the field")
	}
	if r.TriggeredBy(TriggerManual) {
		t.Fatal("the default must not silently widen to manual")
	}
}

// A declared empty list says the rule can never run: a typo, not a config.
func TestEmptyTriggerListIsRejected(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Trigger = []Trigger{}

	e := requireField(t, p.Validate(nil), "spec.download.rules[0].trigger")
	if !strings.Contains(e.Message, "never run") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestUnknownTriggerIsRejected(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Trigger = []Trigger{"cron"}
	requireField(t, p.Validate(nil), "spec.download.rules[0].trigger[0]")
}

func TestManualOnlyRuleIsValid(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Trigger = []Trigger{TriggerManual}
	if err := p.Validate(nil); err != nil {
		t.Fatalf("a hotfix rule that only ever runs by hand is legitimate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Window
// ---------------------------------------------------------------------------

func TestWindowOpensAndCloses(t *testing.T) {
	w := Window{Start: "02:00", End: "06:00", TimeZone: "UTC"}

	inside := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	if open, _, err := w.Open(inside); err != nil || !open {
		t.Fatalf("03:00 is inside 02:00-06:00: (%v, %v)", open, err)
	}

	outside := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)
	open, next, err := w.Open(outside)
	if err != nil || open {
		t.Fatalf("14:00 is outside: (%v, %v)", open, err)
	}
	if next.Hour() != 2 || next.Day() != 2 {
		t.Fatalf("the next opening is tomorrow at 02:00, got %v", next)
	}
}

// 22:00-06:00 is ONE window, not two.
func TestWindowCrossingMidnight(t *testing.T) {
	w := Window{Start: "22:00", End: "06:00", TimeZone: "UTC"}
	for _, hour := range []int{23, 1, 5} {
		at := time.Date(2026, 9, 1, hour, 0, 0, 0, time.UTC)
		if open, _, err := w.Open(at); err != nil || !open {
			t.Fatalf("%02d:00 should be inside a window that crosses midnight: (%v, %v)", hour, open, err)
		}
	}
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if open, _, _ := w.Open(at); open {
		t.Fatal("12:00 is outside 22:00-06:00")
	}
}

// The next opening is built from the local calendar date, so a DST transition
// moves the wall-clock time rather than sliding it by an hour.
func TestWindowNextOpeningIsWallClock(t *testing.T) {
	w := Window{Start: "02:00", End: "06:00", TimeZone: "Europe/London"}
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	// The afternoon before the spring-forward Sunday.
	at := time.Date(2027, 3, 27, 14, 0, 0, 0, loc)
	open, next, err := w.Open(at)
	if err != nil || open {
		t.Fatalf("(%v, %v)", open, err)
	}
	if next.Hour() != 2 {
		t.Fatalf("the window opens at 02:00 wall-clock whatever the offset, got %v", next)
	}
}

func TestZeroLengthWindowIsRejected(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Window = &Window{Start: "02:00", End: "02:00"}

	e := requireField(t, p.Validate(nil), "spec.download.rules[0].window")
	if !strings.Contains(e.Hint, "crosses midnight") {
		t.Fatalf("the hint should pre-empt the obvious misreading, got %q", e.Hint)
	}
}

func TestMalformedWindowIsRejected(t *testing.T) {
	for _, w := range []*Window{
		{Start: "2am", End: "06:00"},
		{Start: "02:00", End: "6"},
		{Start: "02:00", End: "06:00", TimeZone: "Mars/Olympus"},
	} {
		p := withDownloadBlock()
		p.Spec.Download.Rules[0].Window = w
		requireField(t, p.Validate(nil), "spec.download.rules[0].window")
	}
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

func TestRuleSourcesMustExist(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Sources = []string{"nowhere"}
	requireField(t, p.Validate(nil), "spec.download.rules[0].sources[0]")
}

func TestRuleSourcesMustBeEnabled(t *testing.T) {
	p := withDownloadBlock()
	off := false
	p.Spec.Sources[0].Enabled = &off
	p.Spec.Download.Rules[0].Sources = []string{p.Spec.Sources[0].Name}

	e := requireField(t, p.Validate(nil), "spec.download.rules[0].sources[0]")
	if !strings.Contains(e.Message, "never match") {
		t.Fatalf("got %q", e.Message)
	}
}

// ---------------------------------------------------------------------------
// The chain, closed over mirror.from
// ---------------------------------------------------------------------------

// Naming the tail names the chain: the rule needs only the destination it
// cares about.
func TestRuleMayNameOnlyTheTailOfAChain(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Targets = []string{"ocp-prod"}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("a rule naming only the mirror target is valid: %v", err)
	}
	if got := p.chainOf("ocp-prod", targetsByName(p)); len(got) != 2 || got[1] != "jfrog-store" {
		t.Fatalf("the chain must close over mirror.from, got %v", got)
	}
}

// A hop the closure reaches is as load-bearing as a named destination, so a
// disabled one is as fatal.
func TestDisabledHopIsRejected(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Download.Rules[0].Targets = []string{"ocp-prod"}
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
	p := withDownloadBlock()
	p.Spec.Targets[0].Type = RegistryQuay
	p.Spec.Targets[0].Quay = &QuaySettings{APITokenRef: &SecretRef{SecretName: "quay-api", Key: "token"}}
	p.Spec.Targets[0].Repository = "platform-prod/store"
	p.Spec.Targets[0].Replication = &Replication{Mode: ReplicationMirror, Mirror: &MirrorConfig{
		From: "ocp-prod", Tags: []string{"v3.*"}, Robot: "platform-prod+swgw",
	}}

	// The cycle itself is reported by validateMirrorChain; what matters here is
	// that walking it does not hang.
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

// A rule that verifies at the destination, over a mirror whose globs cannot
// match a signature tag, has stated a requirement its own configuration makes
// impossible.
func TestVerifyAfterOverASignatureBlindMirrorIsAnError(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	on := true
	p.Spec.Download.Rules[0].Targets = []string{"ocp-prod"}
	p.Spec.Download.Rules[0].Verify = &RuleVerification{After: &on, Policy: PolicyEnforce}

	e := requireField(t, p.Validate(nil), "spec.download.rules[0].targets[0]")
	if !strings.Contains(e.Hint, "sha256-") {
		t.Fatalf("the hint must name the tag form, got %q", e.Hint)
	}
	if !strings.Contains(e.Hint, "report success") {
		t.Fatalf("the hint must say the sync SUCCEEDS, got %q", e.Hint)
	}
}

func TestNoErrorWhenSignaturesAreMirrored(t *testing.T) {
	p := withDownloadBlock() // tags already carry 'sha256-*.sig'
	on := true
	p.Spec.Download.Rules[0].Targets = []string{"ocp-prod"}
	p.Spec.Download.Rules[0].Verify = &RuleVerification{After: &on, Policy: PolicyEnforce}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// Without verify.after the mirror is nobody's problem: it warns (warnings.go)
// rather than failing, because a product that does not verify at the
// destination has no reason to mirror signatures.
func TestNoErrorWithoutVerifyAfter(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	p.Spec.Download.Rules[0].Targets = []string{"ocp-prod"}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// The check follows the chain, so a mirror reached as a HOP is checked too.
func TestSignatureCheckFollowsTheChain(t *testing.T) {
	p := withDownloadBlock()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	on := true
	// Name the mirror explicitly AND rely on the closure; both must report.
	p.Spec.Download.Rules[0].Targets = []string{"jfrog-store", "ocp-prod"}
	p.Spec.Download.Rules[0].Verify = &RuleVerification{After: &on}

	requireField(t, p.Validate(nil), "spec.download.rules[0].targets[1]")
}
