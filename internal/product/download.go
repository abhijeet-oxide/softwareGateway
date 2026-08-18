package product

import (
	"fmt"
)

// Downloads and auto-download: two things, and keeping them apart is the point.
//
//	download      WHAT happens — take this software, put it through these
//	              targets, with these gates. No patterns. Somebody chose the
//	              software, either by typing it or by matching a rule.
//	autoDownload  WHEN it happens by itself — a tag pattern, and nothing else.
//
// An auto-download rule does not do the downloading. It TRIGGERS a download,
// which is the same operation a person performs by hand. That is why a manual
// download consults no pattern: you already named the software, and a pattern
// asked at that point could only disagree with you.
//
// See docs/design/20.

// Download is one way software moves from a source into internal targets.
//
// Most products have exactly one, and then it is the default by being the only
// one. The list exists so a product that later needs a second — a different
// destination set for a different class of release — can have one without a
// schema change.
type Download struct {
	// Name is required only when a product declares more than one.
	Name string `json:"name,omitempty"`

	// Targets is where the software must end up. It is a SET, not a sequence:
	// the order comes from the targets' own `replication.mirror.from`, so
	// naming the end of a chain names the chain (docs/design/20 §3.5).
	Targets []string `json:"targets,omitempty"`

	Verify   *DownloadVerification `json:"verify,omitempty"`
	Priority *int                  `json:"priority,omitempty"`

	// Default marks the one `transferctl download <tag>` uses, and the one an
	// auto-download rule gets when it names none. Required when a product
	// declares several, meaningless when it declares one.
	Default bool `json:"default,omitempty"`
}

// EffectivePriority returns the download's priority or the default.
func (d Download) EffectivePriority() int {
	if d.Priority == nil {
		return DefaultRulePriority
	}
	return *d.Priority
}

// DownloadVerification says whether verification GATES the chain.
//
// It selects between behaviours the verification subsystem already has; it does
// not introduce a third trust configuration. Keys and identities come from the
// product and the target, because a trust configuration assembled from three
// places is one nobody can audit.
type DownloadVerification struct {
	// Before is source-side verification, before any bytes move.
	Before *bool `json:"before,omitempty"`
	// After is destination-side verification, after they land.
	After *bool `json:"after,omitempty"`
	// Policy is `enforce` or `warn`. Under `enforce` a destination that fails
	// verification stops every step that depends on it — which is the whole
	// security value of a chain.
	Policy VerificationPolicy `json:"policy,omitempty"`
}

// AutoDownload is when a download happens without being asked for.
type AutoDownload struct {
	// Enabled is the master switch over automatic firing. Downloads by hand
	// are unaffected — a switch that also disabled the manual path is one
	// nobody dares use during an incident.
	Enabled bool   `json:"enabled,omitempty"`
	Rules   []Rule `json:"rules,omitempty"`
}

// Rule selects software to download automatically.
//
// Rules are evaluated in order and the FIRST MATCH WINS — not all-match,
// because two rules matching one tag with different downloads has no sensible
// interpretation.
type Rule struct {
	Name string `json:"name"`

	// Enabled turns the rule off without deleting it. Configuration, in Git,
	// and the only way to turn a rule off — there is deliberately no runtime
	// override, because a second source of truth for "is this on" is exactly
	// what GitOps exists to prevent.
	Enabled *bool `json:"enabled,omitempty"`

	// TagPattern is RE2 (Go regexp): linear time, no backtracking. Stated
	// explicitly because a user-supplied pattern evaluated inside a polling
	// loop would, under a backtracking engine, be a denial-of-service vector.
	//
	// It lives HERE and nowhere else. A download has no pattern, because by
	// the time one runs the software has already been chosen.
	TagPattern string `json:"tagPattern"`

	// Sources narrows which sources the rule watches. Absent means all of
	// them. It is how one product carries a vendor's GA channel and its
	// early-access channel without two rules that differ only by a pattern
	// nobody can read.
	Sources []string `json:"sources,omitempty"`

	// Download names which download to trigger. Absent means the default,
	// which for a product with one download is that one.
	Download string `json:"download,omitempty"`

	// Targets, Priority and VerifyBeforeTransfer are the OLDER spelling, from
	// before downloads and auto-download were separate. A rule carrying them
	// describes its own download inline, and keeps working exactly as it did —
	// see Product.DownloadFor.
	Targets              []string `json:"targets,omitempty"`
	Priority             *int     `json:"priority,omitempty"`
	VerifyBeforeTransfer *bool    `json:"verifyBeforeTransfer,omitempty"`
}

// IsEnabled reports whether the rule fires. Defaults to true.
func (r Rule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// UsesLegacyShape reports whether a rule describes its own download inline,
// which every document written before downloads were their own block does.
func (r Rule) UsesLegacyShape() bool {
	return len(r.Targets) > 0 || r.Priority != nil || r.VerifyBeforeTransfer != nil
}

// Downloads returns the product's declared downloads.
func (p Product) Downloads() []Download { return p.Spec.Download }

// DefaultDownload returns the download a manual `transferctl download` runs.
//
// One declared download IS the default, without having to say so. Several
// require exactly one to be marked, which validation enforces — so reaching
// here with none marked means the document did not validate.
func (p Product) DefaultDownload() (Download, bool) {
	switch len(p.Spec.Download) {
	case 0:
		return Download{}, false
	case 1:
		return p.Spec.Download[0], true
	}
	for _, d := range p.Spec.Download {
		if d.Default {
			return d, true
		}
	}
	return Download{}, false
}

// DownloadNamed looks one up.
func (p Product) DownloadNamed(name string) (Download, bool) {
	for _, d := range p.Spec.Download {
		if d.Name == name {
			return d, true
		}
	}
	return Download{}, false
}

// DownloadFor resolves which download an auto-download rule triggers.
//
// Three cases, in the order they are checked, and the middle one is what keeps
// every document written before this split working unchanged:
//
//  1. The rule names a download          — use that one.
//  2. The rule carries its own targets   — it describes a download inline.
//  3. Neither                            — the product's default.
func (p Product) DownloadFor(r Rule) (Download, error) {
	if r.Download != "" {
		d, ok := p.DownloadNamed(r.Download)
		if !ok {
			return Download{}, fmt.Errorf(
				"rule %q names download %q, which is not declared", r.Name, r.Download)
		}
		return d, nil
	}

	if r.UsesLegacyShape() {
		d := Download{Targets: r.Targets, Priority: r.Priority}
		if r.VerifyBeforeTransfer != nil {
			d.Verify = &DownloadVerification{Before: r.VerifyBeforeTransfer}
		}
		return d, nil
	}

	if d, ok := p.DefaultDownload(); ok {
		return d, nil
	}
	// No downloads declared and no inline targets: fall back to the product's
	// default TARGET, which is what a rule naming nothing has always meant.
	t, ok := p.DefaultTarget()
	if !ok {
		return Download{}, fmt.Errorf(
			"rule %q names no download and product %q declares none", r.Name, p.Metadata.Name)
	}
	return Download{Targets: []string{t.Name}}, nil
}

// Rule looks up an auto-download rule by name.
func (p Product) Rule(name string) (Rule, bool) {
	for _, r := range p.Spec.AutoDownload.Rules {
		if r.Name == name {
			return r, true
		}
	}
	return Rule{}, false
}

// AutoDownloadEnabled reports whether rules fire automatically.
func (p Product) AutoDownloadEnabled() bool { return p.Spec.AutoDownload.Enabled }

// VerifiesBefore reports source-side verification. Nil means inherit whatever
// the product and target say.
func (d Download) VerifiesBefore() *bool {
	if d.Verify == nil {
		return nil
	}
	return d.Verify.Before
}

// VerifiesAfter reports destination-side verification. Nil means inherit.
func (d Download) VerifiesAfter() *bool {
	if d.Verify == nil {
		return nil
	}
	return d.Verify.After
}

// VerificationPolicy returns the download's policy override, or empty to
// inherit.
func (d Download) VerificationPolicy() VerificationPolicy {
	if d.Verify == nil {
		return ""
	}
	return d.Verify.Policy
}
