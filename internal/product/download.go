package product

import (
	"fmt"
	"strings"
	"time"
)

// Download rules: one declared operation from vendor to cluster.
//
// The block is `download`, and `autoDownload` is its older spelling. The
// rename is not cosmetic: a rule can now be triggered by hand, so a field
// called `autoDownload` would be describing something it no longer is. Every
// document written against the old name keeps working — see DownloadRules.
//
// See docs/design/20.

// Download is the product's download rules and the master switch over them.
type Download struct {
	// Enabled governs AUTOMATIC firing only. `false` stops discovery from
	// triggering any rule and leaves every one of them runnable by hand, which
	// is what you want during an incident — a master switch that also disabled
	// the recovery path is a switch nobody dares use.
	Enabled *bool  `json:"enabled,omitempty"`
	Rules   []Rule `json:"rules,omitempty"`
}

// IsEnabled reports whether rules fire automatically. Defaults to true when
// any rule is declared, matching what `autoDownload.enabled` meant.
func (d Download) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// Trigger is how a rule can be started.
type Trigger string

const (
	// TriggerDiscovery fires the rule when a matching package is discovered.
	TriggerDiscovery Trigger = "discovery"
	// TriggerManual allows a person, or the UI, to run it on demand.
	TriggerManual Trigger = "manual"
)

// ValidTriggers is the closed set.
var ValidTriggers = []Trigger{TriggerDiscovery, TriggerManual}

// RuleVerification says whether verification GATES this rule's chain.
//
// It selects between behaviours the verification subsystem already has; it
// does not introduce a third trust configuration. Keys and identities come
// from the product and the target, because a trust configuration assembled
// from three places is one nobody can audit.
type RuleVerification struct {
	// Before is source-side verification, before any bytes move.
	Before *bool `json:"before,omitempty"`
	// After is destination-side verification, after they land.
	After *bool `json:"after,omitempty"`
	// Policy is `enforce` or `warn`. Under `enforce` a destination that fails
	// verification stops every step that depends on it — which is the whole
	// security value of a chain.
	Policy VerificationPolicy `json:"policy,omitempty"`
}

// Window is a time of day a rule may start work in.
//
// The one field here beyond the stated requirement, included because a rule
// that fires on discovery at 14:00 and saturates a vendor link for two hours
// is a real way to be paged — and because it costs nothing structurally: the
// request is created with `scheduleAt` set to the next opening, using the
// scheduling that already exists rather than a second scheduler.
type Window struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	TimeZone string `json:"timeZone,omitempty"`
}

// Rule matches a discovered tag and creates a transfer request.
//
// Rules are evaluated in order and the FIRST MATCH WINS — not all-match,
// because two rules matching one tag with different priorities has no sensible
// interpretation.
type Rule struct {
	Name string `json:"name"`

	// Enabled is the CONFIGURED intent, and lives in Git. Turning a rule off
	// operationally is a suspension, which is a different thing on purpose —
	// see docs/design/20 §9.
	Enabled *bool `json:"enabled,omitempty"`

	// TagPattern is RE2 (Go regexp): linear time, no backtracking. Stated
	// explicitly because a user-supplied pattern evaluated inside a polling
	// loop would, under a backtracking engine, be a denial-of-service vector.
	TagPattern string `json:"tagPattern"`

	// Sources narrows which sources a rule watches. Absent means all of them.
	// It is how one product carries a vendor's GA channel and its early-access
	// channel without two rules that differ only by a tag pattern nobody can
	// read.
	Sources []string `json:"sources,omitempty"`

	// Targets names the DESTINATIONS this rule cares about — not every hop.
	// The set is closed over `replication.mirror.from` and then ordered, so a
	// rule naming a Quay target that mirrors from JFrog plans both steps.
	// Naming every hop explicitly still works and is a no-op.
	Targets []string `json:"targets,omitempty"`

	Trigger []Trigger `json:"trigger,omitempty"`
	Window  *Window   `json:"window,omitempty"`

	Priority *int              `json:"priority,omitempty"`
	Verify   *RuleVerification `json:"verify,omitempty"`

	// VerifyBeforeTransfer is the older spelling of `verify.before`.
	VerifyBeforeTransfer *bool `json:"verifyBeforeTransfer,omitempty"`
}

// EffectivePriority returns the rule's priority or the default.
func (r Rule) EffectivePriority() int {
	if r.Priority == nil {
		return DefaultRulePriority
	}
	return *r.Priority
}

// IsEnabled reports the configured intent. Defaults to true.
func (r Rule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Triggers returns the ways this rule can start. Defaults to discovery only,
// which is what every rule written before this field meant.
func (r Rule) Triggers() []Trigger {
	if len(r.Trigger) == 0 {
		return []Trigger{TriggerDiscovery}
	}
	return r.Trigger
}

// TriggeredBy reports whether a rule accepts one way of starting.
func (r Rule) TriggeredBy(t Trigger) bool {
	for _, have := range r.Triggers() {
		if have == t {
			return true
		}
	}
	return false
}

// VerifiesBefore reports source-side verification, honouring the older
// spelling. Nil means "inherit whatever the product and target say".
func (r Rule) VerifiesBefore() *bool {
	if r.Verify != nil && r.Verify.Before != nil {
		return r.Verify.Before
	}
	return r.VerifyBeforeTransfer
}

// VerifiesAfter reports destination-side verification. Nil means inherit.
func (r Rule) VerifiesAfter() *bool {
	if r.Verify == nil {
		return nil
	}
	return r.Verify.After
}

// VerificationPolicy returns the rule's policy override, or empty to inherit.
func (r Rule) VerificationPolicy() VerificationPolicy {
	if r.Verify == nil {
		return ""
	}
	return r.Verify.Policy
}

// DownloadRules returns the product's rules, from whichever spelling the
// document uses.
//
// The compatibility contract, stated so it can be tested: every product
// document valid before M9 is valid after it and produces the same transfers.
func (p Product) DownloadRules() []Rule {
	if len(p.Spec.Download.Rules) > 0 {
		return p.Spec.Download.Rules
	}
	return p.Spec.AutoDownload.Rules
}

// DownloadEnabled reports whether rules fire automatically, from whichever
// spelling the document uses.
func (p Product) DownloadEnabled() bool {
	if len(p.Spec.Download.Rules) > 0 || p.Spec.Download.Enabled != nil {
		return p.Spec.Download.IsEnabled()
	}
	return p.Spec.AutoDownload.Enabled
}

// DownloadBlockPath is the field path of whichever spelling is in use, so an
// error message points at a line the reader actually wrote.
func (p Product) DownloadBlockPath() string {
	if len(p.Spec.Download.Rules) > 0 || p.Spec.Download.Enabled != nil {
		return "spec.download"
	}
	return "spec.autoDownload"
}

// Rule looks up a rule by name.
func (p Product) Rule(name string) (Rule, bool) {
	for _, r := range p.DownloadRules() {
		if r.Name == name {
			return r, true
		}
	}
	return Rule{}, false
}

// ParseWindow returns the window's start and end as minutes past midnight in
// its own zone.
func (w Window) Parse() (start, end int, loc *time.Location, err error) {
	zone := w.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	loc, err = time.LoadLocation(zone)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("unknown time zone %q: %w", zone, err)
	}
	start, err = parseClock(w.Start)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("start: %w", err)
	}
	end, err = parseClock(w.End)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("end: %w", err)
	}
	return start, end, loc, nil
}

func parseClock(v string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%q is not a HH:MM time", v)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Open reports whether the window is open at a moment, and when it next opens.
//
// A window that crosses midnight is open on both sides of it, which is the
// common case: 22:00–06:00 is one window, not two.
func (w Window) Open(at time.Time) (open bool, nextOpening time.Time, err error) {
	start, end, loc, err := w.Parse()
	if err != nil {
		return false, time.Time{}, err
	}
	local := at.In(loc)
	minutes := local.Hour()*60 + local.Minute()

	if start < end {
		open = minutes >= start && minutes < end
	} else {
		// Crosses midnight.
		open = minutes >= start || minutes < end
	}
	if open {
		return true, local, nil
	}

	// The next opening, in the window's own zone. Constructed from the local
	// calendar date rather than by adding a duration, so a DST transition moves
	// the wall-clock time rather than sliding it by an hour — the window is
	// stated in wall-clock terms and should stay there.
	todays := time.Date(local.Year(), local.Month(), local.Day(), start/60, start%60, 0, 0, loc)
	if !todays.After(local) {
		todays = todays.AddDate(0, 0, 1)
	}
	return false, todays, nil
}
