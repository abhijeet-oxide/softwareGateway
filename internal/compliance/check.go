package compliance

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Check is one rule, as its author declared it.
//
// # Why the metadata is data rather than code
//
// The catalogue page lists every check and explains it without evaluating
// anything; the vendor report prints the rule beside the finding; a reviewer
// sees a severity change as a one-line diff. None of that is possible when the
// description is a string literal inside the rule that produces it, which is
// where it lives in the organization's existing policies - and which is why the
// same category is spelled two different ways in two of those files.
//
// # Why AppliesTo is separate from the assertion
//
// It is the denominator. The engine computes the applicable set BEFORE any
// expression runs, so a check that reports only violations still produces a
// complete result set: every applicable subject it did not report on is a pass,
// derived rather than emitted. Without a declared denominator "40 workloads,
// all compliant" and "the traversal never reached them" are the same empty
// list.
type Check struct {
	// ID is permanent and globally unique - "PDB-02". It appears in waivers, in
	// spreadsheets sent to vendors, and in tickets that outlive the release, so
	// it is never reused and never renumbered.
	ID string `json:"id"`
	// Title is one line, in the words the report uses.
	Title string `json:"title"`
	// Description says what is asserted, for a vendor engineer who has to
	// satisfy it. Not a restatement of the title.
	Description string `json:"description,omitempty"`
	// Rationale says WHY the organization requires it. This is what stops a
	// check being carried forward after the reason for it is gone, and it is
	// the field that makes a vendor argue with the requirement rather than with
	// the tool.
	Rationale string `json:"rationale,omitempty"`

	Severity Severity `json:"severity"`
	Tier     Tier     `json:"tier,omitempty"`
	Category string   `json:"category,omitempty"`
	// Subcategory names the MECHANISM in the words an engineer uses for it -
	// "PodDisruptionBudget", "Taints & tolerations", "Seccomp". Category is the
	// section of the standard and is written for whoever is deciding whether to
	// ship; this is written for whoever is about to fix it, and it is what makes
	// findings groupable by the thing they are actually about.
	Subcategory string `json:"subcategory,omitempty"`
	// Keywords is the technical vocabulary this check is findable by: the field
	// paths, the API kinds, the acronyms, the annotation names.
	//
	// # Why a plain-language rewrite needs this
	//
	// Titles and messages are written so that somebody who is not a Kubernetes
	// engineer can act on them, which means they say "the rule that tells the
	// platform how many copies must stay running" rather than
	// "PodDisruptionBudget". That is right for the person deciding whether to
	// ship and useless for the person fixing it: an engineer types `toleration`
	// or `maxUnavailable` or `RWX` into the search box, and the plainer the
	// prose gets the fewer of those words remain anywhere in the report.
	//
	// So the vocabulary is carried deliberately instead of being a side effect
	// of how the sentences happen to be worded. It is indexed by the search on
	// the findings table, so both readers get the report they need out of one
	// set of results.
	Keywords []string `json:"keywords,omitempty"`

	Remediation string `json:"remediation,omitempty"`
	Reference   string `json:"reference,omitempty"`

	// The four fields below are what turns a finding from a statement into
	// something a reader can triage without being a Kubernetes engineer. They
	// are declared on the check because they are properties of the RULE, not of
	// the instance: "who fixes a missing memory limit" has one answer, and
	// writing it once is what stops the report answering it differently in two
	// rows.

	// Confidence says how firmly the tool can assert this from the manifest
	// alone. It is the field that keeps an argument short: a vendor disputing a
	// `needs-review` finding is not disputing the tool, they are supplying the
	// context the tool said it did not have.
	Confidence Confidence `json:"confidence,omitempty"`
	// WhenItBites is when the consequence actually arrives. A defect that only
	// shows up during node maintenance is urgent before a platform upgrade
	// window and can wait otherwise, and nothing else in a finding says so.
	WhenItBites Timing `json:"whenItBites,omitempty"`
	// FixOwner is who makes the change. It replaces the reader's guess, and it
	// is the difference between a report a release manager can route and one
	// they have to ask about.
	FixOwner FixOwner `json:"fixOwner,omitempty"`
	// FixEffort is how much work it is, so a reader can plan rather than
	// discover.
	FixEffort FixEffort `json:"fixEffort,omitempty"`
	// FixExample is the corrected configuration, as YAML. Prose describing a
	// fix and the four lines that are the fix are not the same artifact, and
	// the second one is the one that gets applied.
	FixExample string `json:"fixExample,omitempty"`

	// AppliesTo selects the subjects. Mandatory: see the type comment.
	AppliesTo AppliesTo `json:"appliesTo"`
	// Assert is the condition. True means compliant.
	Assert Assert `json:"assert,omitempty"`

	// Engine names the implementation. Empty means the declarative one. A check
	// the platform must answer itself - determinacy, the artifact tree, another
	// feature's stored result - says "builtin" and is registered in Go.
	Engine string `json:"engine,omitempty"`

	// Deprecated retires a check without freeing its ID, so an old report and
	// an old waiver still resolve to an explanation.
	Deprecated   bool   `json:"deprecated,omitempty"`
	SupersededBy string `json:"supersededBy,omitempty"`

	// Pack is filled in by the loader from the manifest that carried the check.
	// Authors do not write it.
	Pack string `json:"pack,omitempty"`
}

// Engine names.
const (
	// EngineDeclarative is the default: YAML shorthand and CEL, compiled at
	// load.
	EngineDeclarative = "declarative"
	// EngineBuiltin is a check implemented in Go and registered by name,
	// for the ones that need to consult the platform rather than the manifest.
	EngineBuiltin = "builtin"
)

// EngineName is the engine this check uses, defaulted.
func (c Check) EngineName() string {
	if c.Engine == "" {
		return EngineDeclarative
	}
	return c.Engine
}

// Prefix is the ID's owning namespace - "PDB" of "PDB-02".
func (c Check) Prefix() string {
	if i := strings.IndexByte(c.ID, '-'); i > 0 {
		return c.ID[:i]
	}
	return c.ID
}

// checkIDPattern is deliberately strict. An ID is a key in exported
// spreadsheets, in waiver files and in URLs, so it may not contain anything
// that needs quoting or escaping anywhere it is written.
var checkIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,11}-[0-9]{2,3}$`)

// Validate reports every problem with a check, not just the first.
//
// # Why it returns all of them
//
// A pack is edited by a person who then waits for a reload. Reporting one error
// per attempt turns a five-mistake manifest into five round trips, and the
// author learns nothing about the shape of the schema. The loader prints them
// together, each naming the check.
func (c Check) Validate() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	switch {
	case c.ID == "":
		add("id is required")
	case !checkIDPattern.MatchString(c.ID):
		add("id %q is not of the form PREFIX-NN (uppercase prefix, hyphen, two or three digits)", c.ID)
	}
	if strings.TrimSpace(c.Title) == "" {
		add("title is required: it is the line a vendor reads in the report")
	}
	if !c.Severity.Valid() {
		add("severity %q must be one of critical, warning, inform", c.Severity)
	}
	if c.Tier != 0 && c.Tier != Tier1 && c.Tier != Tier2 {
		add("tier %d must be 1 or 2", c.Tier)
	}
	// Subcategory and Keywords are not required HERE, for the same reason
	// Rationale is not: they are what the report needs to be usable, not what
	// the check needs to be evaluable, and refusing to load a check over its
	// metadata would make a finding vanish rather than read badly. The shipped
	// pack is held to them by baseline/contract_test.go, and any pack should be
	// - see docs/compliance/02-authoring-checks.md. An empty keyword is a
	// different thing: it is a malformed value that would match every search.
	for _, k := range c.Keywords {
		if strings.TrimSpace(k) == "" {
			add("keywords contains an empty term, which would match every search")
		}
	}
	if !c.Confidence.Valid() {
		add("confidence %q must be one of confirmed, probable, needs-review", c.Confidence)
	}
	if !c.WhenItBites.Valid() {
		add("whenItBites %q must be one of install, upgrade, node-maintenance, under-load, on-failure, continuously", c.WhenItBites)
	}
	if !c.FixOwner.Valid() {
		add("fixOwner %q must be one of chart-template, chart-values, application, build-pipeline, platform-team, needs-decision", c.FixOwner)
	}
	if !c.FixEffort.Valid() {
		add("fixEffort %q must be one of low, medium, high", c.FixEffort)
	}
	// The severity rubric, mechanical: critical requires confirmed.
	//
	// A check that says in its own metadata that the finding might not hold for
	// this workload cannot also fail the release on its own - that combination
	// is what produces the argument where a vendor is told their deliberate,
	// correct design choice is a blocking defect, and one of those is enough to
	// cost the whole report its credibility.
	//
	// The validation audit put a number on it: 429 of 481 blocking findings
	// were confirmed from the chart, and one check accounted for 52 of the rest
	// - every one of them a reference the platform might well supply itself.
	// Its own confidence said so, and it was blocking anyway.
	if c.Severity == SeverityCritical && c.Confidence != "" && c.Confidence != ConfidenceConfirmed {
		add("a check with confidence %q may not be severity critical: only a finding read directly from the chart can fail the release on its own", c.Confidence)
	}
	if c.Deprecated {
		// A retired check runs nothing, so the rest of the schema does not
		// apply to it. It still has to explain itself, because the reason to
		// keep it in the catalogue is that old reports point at it.
		if strings.TrimSpace(c.Description) == "" && c.SupersededBy == "" {
			add("a deprecated check needs a description or a supersededBy, or the ID in an old report explains nothing")
		}
		return errs
	}

	switch c.EngineName() {
	case EngineDeclarative:
		if c.Assert.Empty() {
			add("assert is required: a declarative check with no assertion would pass everything it applies to")
		}
		if c.Assert.ObserveOnPass && c.Assert.Observed == "" {
			add("assert.observeOnPass records the observed value on a pass, and this check has no observed expression to record")
		}
		if sup := c.Assert.SupersededBy; sup != nil {
			if !checkIDPattern.MatchString(sup.Check) {
				add("assert.supersededBy.check %q is not a check id, and a finding that stands down has to say which check to read instead", sup.Check)
			}
			if sup.Check == c.ID {
				add("assert.supersededBy.check names this check itself")
			}
			if strings.TrimSpace(sup.When) == "" {
				add("assert.supersededBy needs a `when`: without one this check would never report anything")
			}
			if strings.TrimSpace(sup.Because) == "" {
				add("assert.supersededBy needs a `because`: a skipped finding with no reason reads as the check not having run")
			}
		}
	case EngineBuiltin:
		if !c.Assert.Empty() {
			add("a builtin check must not carry an assert block: the Go implementation is the assertion, and two sources of truth is one too many")
		}
	default:
		add("engine %q is not known (declarative, builtin)", c.Engine)
	}

	errs = append(errs, c.AppliesTo.Validate()...)
	return errs
}

// AppliesTo declares which subjects a check judges - the denominator.
//
// # Why containers are a first-class selector
//
// A Deployment with a main container, two sidecars and an init container is
// four subjects for a resources check, and a vendor fixes them one at a time.
// A check that reported "Deployment/foo: missing limits" would be one row for
// four defects, and after the vendor fixes three it says exactly what it said
// before. Making the container the subject is what makes the count mean
// something.
//
// # Why reaching the pod spec is not the author's job
//
// A CronJob's pod spec is at spec.jobTemplate.spec.template.spec, and every
// other workload's is at spec.template.spec. Seven of the organization's
// sixteen existing policies declare CronJob in their kind list and then read
// spec.template.spec, so they match a CronJob, find nothing, and report
// nothing - a silent false negative on every scheduled job in the estate. Here
// the traversal is the engine's, written once.
type AppliesTo struct {
	// Kinds is the object kinds this check judges. Empty means every kind,
	// which is almost never what an author means and is worth being explicit
	// about.
	Kinds []string `json:"kinds,omitempty"`
	// APIGroups narrows by group when kind alone is ambiguous - two CRDs called
	// Cluster in one release is normal.
	APIGroups []string `json:"apiGroups,omitempty"`

	// Labels and Annotations select by metadata. A value of "*" means the key
	// must be present with any value; any other value must match exactly.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Charts limits the check to named charts, for a standard that applies to
	// one vendor component. Glob syntax, matched against the chart name.
	Charts []string `json:"charts,omitempty"`

	// Containers selects container subjects instead of the whole object.
	Containers ContainerScope `json:"containers,omitempty"`

	// Subject moves the bound value from the object to something inside it.
	//
	// `podSpec` is the one that earns its place: a check about hostNetwork, a
	// service account or a termination grace period is the same check on a
	// Deployment and on a CronJob, and only the path differs - by two levels,
	// in the one case everybody forgets. Declaring the subject lets the check
	// be written once, against `spec.hostNetwork`, and applied to every
	// workload kind.
	Subject SubjectScope `json:"subject,omitempty"`

	// Where is an optional CEL predicate for applicability the fields above
	// cannot express. It decides membership of the denominator, not compliance:
	// a subject Where excludes is not counted at all, where one the assertion
	// rejects is a failure. Confusing the two is how a check silently narrows
	// itself until it applies to nothing.
	Where string `json:"where,omitempty"`
}

// SubjectScope says what inside the object a check judges.
type SubjectScope string

const (
	// SubjectObject binds the whole object. The default.
	SubjectObject SubjectScope = ""
	// SubjectPodSpec binds the pod spec, wherever the kind keeps it. Paths in
	// the check are then relative to the pod spec: "hostNetwork", not
	// "spec.template.spec.hostNetwork", and correct for a CronJob without the
	// author knowing a CronJob is different.
	SubjectPodSpec SubjectScope = "podSpec"
)

// Valid reports whether s is a known subject scope.
func (s SubjectScope) Valid() bool {
	return s == SubjectObject || s == SubjectPodSpec
}

// ContainerScope says which containers of a workload are subjects.
type ContainerScope string

const (
	// ScopeNone makes the object itself the subject. The default, and
	// writable as `none` so a manifest can say so rather than relying on a
	// reader knowing that an omitted field means the object.
	ScopeNone ContainerScope = ""
	// ScopeObject is `none` spelled out. Identical to ScopeNone.
	ScopeObject ContainerScope = "none"
	// ScopeAll is every container: main, init and ephemeral. The right default
	// for anything about images, probes or resources, because an init container
	// with no limits is the same defect as a main one with no limits - and is
	// the case §3.7 of the policy review says is usually forgotten.
	ScopeAll ContainerScope = "all"
	// ScopeMain is spec.containers only.
	ScopeMain ContainerScope = "main"
	// ScopeInit is spec.initContainers only.
	ScopeInit ContainerScope = "init"
)

// Valid reports whether s is a known scope.
func (s ContainerScope) Valid() bool {
	switch s {
	case ScopeNone, ScopeObject, ScopeAll, ScopeMain, ScopeInit:
		return true
	}
	return false
}

// SelectsContainers reports whether the subject is a container rather than an
// object.
func (s ContainerScope) SelectsContainers() bool { return s != ScopeNone && s != ScopeObject }

// Validate reports every problem with an applicability declaration.
func (a AppliesTo) Validate() []error {
	var errs []error
	if !a.Subject.Valid() {
		errs = append(errs, fmt.Errorf("appliesTo.subject %q must be podSpec or omitted", a.Subject))
	}
	if a.Subject == SubjectPodSpec && a.Containers.SelectsContainers() {
		errs = append(errs, fmt.Errorf("appliesTo declares both subject: podSpec and containers: %s; a subject is one thing", a.Containers))
	}
	if !a.Containers.Valid() {
		errs = append(errs, fmt.Errorf("appliesTo.containers %q must be one of all, main, init, none, or omitted", a.Containers))
	}
	for _, k := range a.Kinds {
		if k == "" {
			errs = append(errs, fmt.Errorf("appliesTo.kinds contains an empty kind"))
		}
	}
	for k, v := range a.Labels {
		if k == "" {
			errs = append(errs, fmt.Errorf("appliesTo.labels contains an empty key (value %q)", v))
		}
	}
	for k, v := range a.Annotations {
		if k == "" {
			errs = append(errs, fmt.Errorf("appliesTo.annotations contains an empty key (value %q)", v))
		}
	}
	if len(a.Kinds) == 0 && a.Where == "" && len(a.Charts) == 0 {
		errs = append(errs, fmt.Errorf("appliesTo selects every resource in the release; declare kinds, or say so with where: \"true\""))
	}
	return errs
}

// MatchesKind reports whether an object of this apiVersion/kind is in scope.
//
// Kind and group are compared case-sensitively, as Kubernetes spells them: a
// manifest saying "deployment" is not a Deployment to the API server either,
// and quietly accepting it here would make this tool agree with a manifest the
// cluster will reject.
func (a AppliesTo) MatchesKind(apiVersion, kind string) bool {
	if len(a.Kinds) > 0 && !contains(a.Kinds, kind) {
		return false
	}
	if len(a.APIGroups) > 0 && !contains(a.APIGroups, groupOf(apiVersion)) {
		return false
	}
	return true
}

// MatchesMeta reports whether an object's labels and annotations put it in
// scope. "*" means present with any value.
func (a AppliesTo) MatchesMeta(labels, annotations map[string]string) bool {
	return matchSelector(a.Labels, labels) && matchSelector(a.Annotations, annotations)
}

func matchSelector(want, have map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok {
			return false
		}
		if v != "*" && got != v {
			return false
		}
	}
	return true
}

// groupOf returns the API group of an apiVersion. Core objects are in the empty
// group, which is what "v1" means and what a selector spells as "".
func groupOf(apiVersion string) string {
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Assert is the condition, in the declarative forms and the escape hatch.
//
// # Why there is a shorthand at all
//
// Most of the 88 baseline checks are "field X of kind Y must satisfy Z", and an
// expression language is a poor way to write that eighty times: eighty chances
// to get null-safety wrong, and eighty different spellings of the same
// requirement in the report. The shorthand compiles to the same evaluator, so
// there is one semantics, and it fills in the observed value, the locus and the
// message from the form itself - which is how a report gets a useful "expected"
// column without every author writing one.
//
// All populated forms must hold. They are AND, never OR: a check whose parts
// are alternatives is two checks, and a vendor can act on two checks.
type Assert struct {
	// Required paths must exist and be non-empty. The single most common
	// assertion in the catalogue.
	Required []string `json:"required,omitempty"`
	// Forbidden paths must not exist.
	Forbidden []string `json:"forbidden,omitempty"`
	// Equals is exact equality, path to value.
	Equals map[string]any `json:"equals,omitempty"`
	// OneOf constrains a path to a set.
	OneOf map[string][]any `json:"oneOf,omitempty"`
	// Matches is an RE2 match on the string form of a path.
	Matches map[string]string `json:"matches,omitempty"`
	// EqualPaths asserts two paths hold the same value - the requests-equal-
	// limits family, which is otherwise an expression in every check that needs
	// it.
	EqualPaths []PathPair `json:"equalPaths,omitempty"`
	// Numeric bounds a path parsed as a Kubernetes quantity, so "250m", "1Gi"
	// and "0.5" are all comparable without the author knowing the suffix rules.
	Numeric map[string]Bound `json:"numeric,omitempty"`

	// Expr is CEL, for everything the forms above cannot say. True means
	// compliant.
	Expr string `json:"expr,omitempty"`

	// ObserveOnPass records the observed value on a PASS as well as on a
	// failure.
	//
	// # Why a check would want that
	//
	// Some checks exist to report what is there rather than to reject it: which
	// containers cap their processing power, which claims are shared, what a
	// release runs outside the ordinary install. Without this, the only way to
	// make such a check say anything is to make it FAIL on every subject - and
	// that is how a pack ends up with three checks producing a third of the
	// report's rows and close to none of its defects, which is the shape the
	// audit in docs/compliance/compliance-report.md found.
	//
	// With it, the check passes on everything correct and still carries what it
	// saw, so the inventory lives in the full record and the action report stays
	// about defects.
	//
	// It applies only to the author-supplied `observed`, never to the
	// shorthand's per-term one: a term's observed value describes the term that
	// failed, and there is no failing term on a pass. An expression used this
	// way has to read correctly in both cases - "runs at pre-upgrade,
	// pre-install", not "runs at nothing Helm recognises".
	ObserveOnPass bool `json:"observeOnPass,omitempty"`

	// Observed, Expected, Locus and Message override what the shorthand would
	// have derived. Observed and Message are CEL expressions returning a
	// string, so a finding can name the value that offended.
	Observed string `json:"observed,omitempty"`
	Expected string `json:"expected,omitempty"`
	Locus    string `json:"locus,omitempty"`
	Message  string `json:"message,omitempty"`

	// LocusExpr computes the locus instead of stating it, for a check whose
	// field depends on what the manifest says.
	//
	// # The defect this exists for
	//
	// A container that inherits runAsNonRoot from its pod was reported at
	// spec.template.spec.containers[0].securityContext.runAsNonRoot - a line
	// that is not in the manifest. The reader opens the evidence, finds no such
	// field, and either distrusts the finding or goes hunting for the real one.
	//
	// A result beginning with "spec" is taken as an ABSOLUTE path from the
	// object and is not prefixed with the container's own path; anything else
	// is relative to the subject, exactly like Locus.
	LocusExpr string `json:"locusExpr,omitempty"`

	// Effective is what actually applies at run time, where that differs from
	// what the manifest says.
	//
	// # Why a report needs both
	//
	// Nearly every finding worth writing is a gap between a declared value and
	// an effective one: a field left out and filled in by a platform default,
	// one setting overriding another, a value inherited from the pod,
	// arithmetic that rounds a percentage down to zero, a limit that silently
	// becomes the request. Naming only the declared value asks the reader to
	// know the resolution rule; naming both is the whole finding.
	//
	// It is a CEL expression returning a string, evaluated on a failure and -
	// like Observed - on a pass when observeOnPass is set.
	Effective string `json:"effective,omitempty"`

	// SupersededBy names a check whose finding, when it fires on the same
	// subject, makes this one unactionable.
	SupersededBy *Supersession `json:"supersededBy,omitempty"`
}

// Supersession is one check standing down in favour of another that has already
// reported the root cause.
//
// # The defect this exists for
//
// A container with `privileged: true` gets three blocking findings: the
// privilege itself, the missing capability drop, and the permitted privilege
// escalation. The second and third cannot be acted on - the kernel grants the
// full capability set to a privileged container whatever `capabilities.drop`
// says, and permits escalation whatever `allowPrivilegeEscalation` says. A
// reader fixes two of the three, changes nothing about the container's actual
// powers, and learns that the tool counts rather than reasons.
//
// A superseded subject is recorded as SKIPPED, not dropped: it stays in the
// full record, with a sentence saying which check owns it, so nobody concludes
// the check was never run.
type Supersession struct {
	// Check is the ID that owns the root cause - the one whose finding has to
	// be acted on first.
	Check string `json:"check"`
	// When is CEL over the same subject. True means this subject's finding is
	// superseded.
	When string `json:"when"`
	// Because is one clause, in plain language, saying why this finding cannot
	// be acted on while the other holds. It is joined onto a sentence, so it
	// reads as "...is not reported here because <because>".
	Because string `json:"because"`
}

// PathPair is the two operands of an equalPaths assertion.
type PathPair struct {
	A string
	B string
}

// UnmarshalJSON accepts the two-element list form the manifest uses. Documents
// are decoded with sigs.k8s.io/yaml, which routes YAML through JSON, so this is
// where a YAML list arrives.
//
//	equalPaths:
//	  - [resources.requests.cpu, resources.limits.cpu]
//	  - [resources.requests.memory, resources.limits.memory]
func (p *PathPair) UnmarshalJSON(b []byte) error {
	var pair []string
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("equalPaths entry must be a list of two paths: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("equalPaths entry needs exactly two paths, got %d", len(pair))
	}
	p.A, p.B = pair[0], pair[1]
	return nil
}

// MarshalJSON writes the pair back in the form it was written, so the API
// serves a check in the shape its author wrote it.
func (p PathPair) MarshalJSON() ([]byte, error) { return json.Marshal([]string{p.A, p.B}) }

// Bound is a numeric range. Either end may be omitted.
type Bound struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// Empty reports whether nothing is asserted, which is a load error for a
// declarative check: it would pass everything it applies to and read on the
// catalogue page as a rule being enforced.
func (a Assert) Empty() bool {
	return len(a.Required) == 0 && len(a.Forbidden) == 0 && len(a.Equals) == 0 &&
		len(a.OneOf) == 0 && len(a.Matches) == 0 && len(a.EqualPaths) == 0 &&
		len(a.Numeric) == 0 && a.Expr == ""
}

// Pack is a manifest: an identity, and the checks it owns.
type Pack struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   PackMetadata `json:"metadata"`
	Spec       PackSpec     `json:"spec"`
}

// PackMetadata identifies a pack and the ID namespace it owns.
type PackMetadata struct {
	Name string `json:"name"`
	// Prefix is the check-ID namespace this pack OWNS. Two packs claiming one
	// prefix is a load error for the second, named in the message. This is what
	// makes an ID globally unique with no central registry and no coordination
	// between the teams writing packs.
	Prefix      string `json:"prefix"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Maintainer  string `json:"maintainer,omitempty"`
	Reference   string `json:"reference,omitempty"`
}

// PackSpec carries the checks. Prefixes is for a pack that legitimately owns
// several ID namespaces - the shipped baseline owns thirteen, one per category
// of the organization's source catalogue, so the IDs in their existing document
// are the IDs in the tool and nobody has to translate.
type PackSpec struct {
	Prefixes []string `json:"prefixes,omitempty"`
	Checks   []Check  `json:"checks"`
}

// OwnedPrefixes is every ID namespace this pack claims.
func (p Pack) OwnedPrefixes() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append([]string{p.Metadata.Prefix}, p.Spec.Prefixes...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Expected manifest identity. A file that is not a PolicyPack is skipped rather
// than rejected: a policy directory holds waiver files and READMEs too, and a
// loader that failed on them would make the directory unusable for anything
// else.
const (
	PackAPIVersion = "softwaregateway.io/v1alpha1"
	PackKind       = "PolicyPack"
)

// Validate reports every structural problem with a pack, excluding the checks
// themselves.
func (p Pack) Validate() []error {
	var errs []error
	if p.APIVersion != PackAPIVersion {
		errs = append(errs, fmt.Errorf("apiVersion %q is not %s", p.APIVersion, PackAPIVersion))
	}
	if p.Kind != PackKind {
		errs = append(errs, fmt.Errorf("kind %q is not %s", p.Kind, PackKind))
	}
	if strings.TrimSpace(p.Metadata.Name) == "" {
		errs = append(errs, fmt.Errorf("metadata.name is required"))
	}
	if len(p.OwnedPrefixes()) == 0 {
		errs = append(errs, fmt.Errorf("metadata.prefix is required: it is the ID namespace this pack owns, and without it two packs can define the same check ID"))
	}
	for _, pfx := range p.OwnedPrefixes() {
		if !prefixPattern.MatchString(pfx) {
			errs = append(errs, fmt.Errorf("prefix %q must be 2 to 12 uppercase letters or digits, starting with a letter", pfx))
		}
	}
	if len(p.Spec.Checks) == 0 {
		errs = append(errs, fmt.Errorf("spec.checks is empty"))
	}
	return errs
}

var prefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,11}$`)
