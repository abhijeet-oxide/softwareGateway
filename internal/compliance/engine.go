package compliance

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Judgement is what one evaluation concluded about one subject.
//
// It is not a Result: the check's identity, severity, address and determinacy
// are the engine's to attach, and an evaluator that could supply them could
// also get them wrong. An evaluator answers one question - does this subject
// satisfy the rule - and describes what it saw.
type Judgement struct {
	// Compliant is the answer. Ignored when Err is set.
	Compliant bool
	// Observed is the value that decided it, in the subject's own units, and
	// Expected is what the rule required. Both are for the vendor: a report
	// saying only "failed" makes them reproduce the analysis to learn which
	// value offended.
	Observed string
	Expected string
	// Locus is the field judged, relative to the subject. The engine prefixes
	// it with the container path when the subject is a container, so an author
	// writes "resources.limits.memory" and the report says
	// "spec.template.spec.containers[2].resources.limits.memory".
	Locus string
	// Message is one sentence for somebody who does not have this screen open.
	Message string
	// Err makes the result undecidable rather than failed. An expression that
	// faulted on a field a custom resource does not have has not shown
	// non-compliance, and recording it as a failure would send a vendor after a
	// defect in their chart that is really a defect in the check.
	Err error
}

// Program is a compiled check: its applicability predicate and its assertion.
//
// Both are compiled, because both can be wrong and both should be wrong at load
// time. The interface exists so the engine does not import an expression
// language - the declarative compiler and the Go built-ins satisfy it
// identically, and a run cannot tell which produced a result.
type Program interface {
	// Applies decides membership of the denominator for subjects the
	// declarative selectors could not settle. It is asked only when the check
	// declares a `where`.
	Applies(ctx context.Context, subj Subject, idx *Index) (bool, error)
	// Evaluate judges one subject.
	Evaluate(ctx context.Context, subj Subject, idx *Index) (Judgement, error)
}

// Compiler turns a declared check into a Program, reporting a usable error when
// it cannot.
//
// The error is the point. An expression compiled at load fails in front of the
// person editing the pack, naming the check and the column; the alternative is
// a fault on release 47, in a branch nobody took until a vendor shipped a
// StatefulSet with no annotations.
type Compiler interface {
	Compile(check Check) (Program, error)
}

// Determiner answers how firmly a value is established - see Determinacy.
//
// It is an interface so the engine does not depend on the renderer: a run over
// plain manifests with nothing to override has no determiner and every result
// is `na`, which is the honest answer rather than a default that reads as a
// measurement.
type Determiner interface {
	Determinacy(subj Subject, locus string) Determinacy
}

// Catalog is the loaded checks, by ID, with their compiled programs.
type Catalog struct {
	checks   map[string]Check
	programs map[string]Program
	order    []string

	// packs records what loaded and what did not. A pack that failed is kept,
	// with its reason: the checks it owns must report `error` rather than
	// vanishing, because a check that disappears looks exactly like a check
	// that passed.
	packs []PackStatus

	// BundleDigest identifies this catalogue's contents, recorded on every run
	// so "which rulebook produced this report" has an answer a year later.
	BundleDigest string
}

// PackStatus is what happened to one pack at load.
type PackStatus struct {
	Name        string   `json:"name"`
	Prefixes    []string `json:"prefixes,omitempty"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Maintainer  string   `json:"maintainer,omitempty"`
	Reference   string   `json:"reference,omitempty"`
	Path        string   `json:"path,omitempty"`
	// Builtin marks the pack compiled into the binary. It is always present and
	// cannot be removed by deleting a directory, which is what stops a
	// misconfigured mount turning every release green.
	Builtin bool `json:"builtin,omitempty"`
	Checks  int  `json:"checks"`
	// Errors is why a pack did not load, or the problems found in one that did.
	// Surfaced in the API and on the policy page, never only in a log.
	Errors []string `json:"errors,omitempty"`
}

// OK reports whether the pack loaded cleanly.
func (p PackStatus) OK() bool { return len(p.Errors) == 0 }

// NewCatalog builds an empty catalogue.
func NewCatalog() *Catalog {
	return &Catalog{
		checks:   make(map[string]Check, 128),
		programs: make(map[string]Program, 128),
	}
}

// Add registers one compiled check. It reports an error rather than overwriting
// on a duplicate ID: two checks with one ID means one of them silently stops
// existing, and which one depends on load order.
func (c *Catalog) Add(check Check, prog Program) error {
	if _, dup := c.checks[check.ID]; dup {
		return fmt.Errorf("check %s is already registered by pack %q", check.ID, c.checks[check.ID].Pack)
	}
	c.checks[check.ID] = check
	if prog != nil {
		c.programs[check.ID] = prog
	}
	c.order = append(c.order, check.ID)
	sort.Strings(c.order)
	return nil
}

// AddPackStatus records a pack's load outcome.
func (c *Catalog) AddPackStatus(s PackStatus) { c.packs = append(c.packs, s) }

// Packs is every pack seen, loaded or broken, in a stable order.
func (c *Catalog) Packs() []PackStatus {
	out := append([]PackStatus(nil), c.packs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Checks is every registered check, ordered by ID.
func (c *Catalog) Checks() []Check {
	out := make([]Check, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.checks[id])
	}
	return out
}

// Check returns one check by ID.
func (c *Catalog) Check(id string) (Check, bool) {
	ch, ok := c.checks[id]
	return ch, ok
}

// Len is the number of registered checks.
func (c *Catalog) Len() int { return len(c.checks) }

// Engine evaluates a catalogue against a release.
type Engine struct {
	Catalog *Catalog
	// Determiner is optional; without one every result is determinacy `na`.
	Determiner Determiner
	// Waivers is optional; without it nothing is waived.
	Waivers WaiverSet
	// MaxResults truncates rather than exhausting memory on a pathological
	// release. A truncated run says so - a silently shortened report is worse
	// than a failed one, because it looks complete.
	MaxResults int
}

// WaiverSet decides whether an accepted exception covers a result.
type WaiverSet interface {
	Waive(r *Result) bool
}

// ErrTruncated is returned alongside results when MaxResults was reached.
var ErrTruncated = errors.New("compliance: result limit reached, report is incomplete")

// Run evaluates every check against every subject it applies to.
//
// # The shape of a run
//
// For each check the engine computes the applicable subjects FIRST, from the
// declared AppliesTo, and only then evaluates. That order is what makes passes
// derivable: a subject that was applicable and produced no failure is a pass,
// and a check that applied to nothing produces one skip rather than silence.
//
// The alternative - letting checks emit their own results - is what the
// organization's existing policies do, and it is why they can report
// "compliant", "not applicable" and "the traversal never got there" with the
// same empty list.
func (e *Engine) Run(ctx context.Context, rel *Release) ([]Result, error) {
	idx := BuildIndex(rel)
	results := make([]Result, 0, 1024)
	truncated := false

	appendResult := func(r Result) {
		if truncated {
			return
		}
		if e.MaxResults > 0 && len(results) >= e.MaxResults {
			truncated = true
			return
		}
		if e.Waivers != nil && r.Outcome == OutcomeFail {
			e.Waivers.Waive(&r)
		}
		results = append(results, r)
	}

	for _, check := range e.Catalog.Checks() {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if check.Deprecated {
			continue
		}

		prog, ok := e.Catalog.programs[check.ID]
		if !ok {
			// A check with no program is one whose pack failed to compile. It
			// reports error, once, rather than being absent: absence is
			// indistinguishable from a pass on the screen a release manager
			// reads.
			appendResult(e.result(check, Subject{Address: Address{Product: rel.Product, Release: rel.Tag, PackageDigest: rel.PackageDigest}},
				Judgement{Err: errors.New("check did not load; its pack is reported broken in the policy catalogue")}))
			continue
		}

		subjects := e.subjects(ctx, check, rel, idx, prog)
		if len(subjects) == 0 {
			// Nothing to judge. Recorded, because "this release has no
			// PodDisruptionBudgets" and "the PDB checks did not run" must not
			// look the same.
			appendResult(Result{
				CheckID:     check.ID,
				CheckTitle:  check.Title,
				Severity:    check.Severity,
				Tier:        check.Tier,
				Category:    check.Category,
				Pack:        check.Pack,
				Outcome:     OutcomeSkip,
				Determinacy: DeterminacyNA,
				Address:     Address{Product: rel.Product, Release: rel.Tag, PackageDigest: rel.PackageDigest},
				Message:     "no resources in this release are in scope for this check",
			})
			continue
		}

		for _, subj := range subjects {
			if err := ctx.Err(); err != nil {
				return results, err
			}
			j, err := prog.Evaluate(ctx, subj, idx)
			if err != nil {
				j = Judgement{Err: err}
			}
			appendResult(e.result(check, subj, j))
		}
	}

	Sort(results)
	if truncated {
		return results, ErrTruncated
	}
	return results, nil
}

// subjects computes a check's denominator.
func (e *Engine) subjects(ctx context.Context, check Check, rel *Release, idx *Index, prog Program) []Subject {
	var out []Subject
	at := check.AppliesTo

	for i := range rel.Resources {
		res := &rel.Resources[i]
		if !at.MatchesKind(res.APIVersion(), res.Kind()) {
			continue
		}
		if !at.MatchesMeta(res.Labels(), res.Annotations()) {
			continue
		}
		if len(at.Charts) > 0 && !matchesAnyGlob(at.Charts, res.Address.Chart) {
			continue
		}

		if !at.Containers.SelectsContainers() {
			subj := Subject{Resource: res, Address: res.Address}
			if e.included(ctx, check, prog, subj, idx) {
				out = append(out, subj)
			}
			continue
		}
		for _, c := range res.Containers(at.Containers) {
			container := c
			addr := res.Address
			addr.Container = container.Name
			addr.ContainerType = container.Type
			subj := Subject{Resource: res, Container: &container, Address: addr}
			if e.included(ctx, check, prog, subj, idx) {
				out = append(out, subj)
			}
		}
	}
	return out
}

// included applies the check's `where` predicate, when it has one.
//
// A predicate that faults excludes the subject and is not silently swallowed:
// the check still reports, because the subject is then judged by nothing and a
// denominator that shrinks for an unexplained reason is how a check quietly
// stops applying to anything.
func (e *Engine) included(ctx context.Context, check Check, prog Program, subj Subject, idx *Index) bool {
	if check.AppliesTo.Where == "" {
		return true
	}
	ok, err := prog.Applies(ctx, subj, idx)
	if err != nil {
		return false
	}
	return ok
}

// result assembles the Result from the check, the subject and the judgement.
// This is the only place a Result is constructed from an evaluation, so the
// address, the severity and the determinacy cannot be got wrong per check.
func (e *Engine) result(check Check, subj Subject, j Judgement) Result {
	addr := subj.Address
	addr.Locus = e.locus(subj, j.Locus)

	r := Result{
		CheckID:     check.ID,
		CheckTitle:  check.Title,
		Severity:    check.Severity,
		Tier:        check.Tier,
		Category:    check.Category,
		Pack:        check.Pack,
		Remediation: check.Remediation,
		Reference:   check.Reference,
		Address:     addr,
		Observed:    j.Observed,
		Expected:    j.Expected,
		Message:     j.Message,
		Determinacy: DeterminacyNA,
	}

	switch {
	case j.Err != nil:
		r.Outcome = OutcomeError
		r.Error = j.Err.Error()
		if r.Message == "" {
			r.Message = "this check could not be decided for " + subj.Describe()
		}
	case j.Compliant:
		r.Outcome = OutcomePass
	default:
		r.Outcome = OutcomeFail
		if e.Determiner != nil {
			r.Determinacy = e.Determiner.Determinacy(subj, addr.Locus)
		} else {
			r.Determinacy = DeterminacyUnknown
		}
		if r.Message == "" {
			r.Message = check.Title + " is not satisfied by " + subj.Describe()
		}
	}
	return r
}

// locus prefixes a container-relative locus with the container's path, so a
// vendor navigates to it without knowing which container was subject 3.
func (e *Engine) locus(subj Subject, locus string) string {
	if locus == "" {
		return subj.Address.Locus
	}
	if subj.Container != nil {
		return subj.Container.Path() + "." + locus
	}
	return locus
}

// matchesAnyGlob reports whether s matches any of the patterns, where "*"
// matches any run of characters. Deliberately not full filepath globbing: a
// chart name is not a path, and "*" is the only wildcard anybody writing a
// chart selector needs.
func matchesAnyGlob(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	parts := splitOnStar(pattern)
	if len(parts) == 1 {
		return pattern == s
	}
	rest := s
	for i, part := range parts {
		if part == "" {
			continue
		}
		switch {
		case i == 0:
			if len(rest) < len(part) || rest[:len(part)] != part {
				return false
			}
			rest = rest[len(part):]
		case i == len(parts)-1:
			if len(rest) < len(part) || rest[len(rest)-len(part):] != part {
				return false
			}
			rest = rest[:len(rest)-len(part)]
		default:
			k := indexOf(rest, part)
			if k < 0 {
				return false
			}
			rest = rest[k+len(part):]
		}
	}
	return true
}

func splitOnStar(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '*' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return append(out, p[start:])
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
