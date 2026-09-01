package compliance

import (
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
	ID string `yaml:"id" json:"id"`
	// Title is one line, in the words the report uses.
	Title string `yaml:"title" json:"title"`
	// Description says what is asserted, for a vendor engineer who has to
	// satisfy it. Not a restatement of the title.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Rationale says WHY the organization requires it. This is what stops a
	// check being carried forward after the reason for it is gone, and it is
	// the field that makes a vendor argue with the requirement rather than with
	// the tool.
	Rationale string `yaml:"rationale,omitempty" json:"rationale,omitempty"`

	Severity Severity `yaml:"severity" json:"severity"`
	Tier     Tier     `yaml:"tier,omitempty" json:"tier,omitempty"`
	Category string   `yaml:"category,omitempty" json:"category,omitempty"`

	Remediation string `yaml:"remediation,omitempty" json:"remediation,omitempty"`
	Reference   string `yaml:"reference,omitempty" json:"reference,omitempty"`

	// AppliesTo selects the subjects. Mandatory: see the type comment.
	AppliesTo AppliesTo `yaml:"appliesTo" json:"appliesTo"`
	// Assert is the condition. True means compliant.
	Assert Assert `yaml:"assert,omitempty" json:"assert,omitempty"`

	// Engine names the implementation. Empty means the declarative one. A check
	// the platform must answer itself - determinacy, the artifact tree, another
	// feature's stored result - says "builtin" and is registered in Go.
	Engine string `yaml:"engine,omitempty" json:"engine,omitempty"`

	// Deprecated retires a check without freeing its ID, so an old report and
	// an old waiver still resolve to an explanation.
	Deprecated   bool   `yaml:"deprecated,omitempty" json:"deprecated,omitempty"`
	SupersededBy string `yaml:"supersededBy,omitempty" json:"supersededBy,omitempty"`

	// Pack is filled in by the loader from the manifest that carried the check.
	// Authors do not write it.
	Pack string `yaml:"-" json:"pack,omitempty"`
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
		add("severity %q must be one of block, warn, info", c.Severity)
	}
	if c.Tier != 0 && c.Tier != Tier1 && c.Tier != Tier2 {
		add("tier %d must be 1 or 2", c.Tier)
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
	Kinds []string `yaml:"kinds,omitempty" json:"kinds,omitempty"`
	// APIGroups narrows by group when kind alone is ambiguous - two CRDs called
	// Cluster in one release is normal.
	APIGroups []string `yaml:"apiGroups,omitempty" json:"apiGroups,omitempty"`

	// Labels and Annotations select by metadata. A value of "*" means the key
	// must be present with any value; any other value must match exactly.
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`

	// Charts limits the check to named charts, for a standard that applies to
	// one vendor component. Glob syntax, matched against the chart name.
	Charts []string `yaml:"charts,omitempty" json:"charts,omitempty"`

	// Containers selects container subjects instead of the whole object.
	Containers ContainerScope `yaml:"containers,omitempty" json:"containers,omitempty"`

	// Where is an optional CEL predicate for applicability the fields above
	// cannot express. It decides membership of the denominator, not compliance:
	// a subject Where excludes is not counted at all, where one the assertion
	// rejects is a failure. Confusing the two is how a check silently narrows
	// itself until it applies to nothing.
	Where string `yaml:"where,omitempty" json:"where,omitempty"`
}

// ContainerScope says which containers of a workload are subjects.
type ContainerScope string

const (
	// ScopeNone makes the object itself the subject. The default.
	ScopeNone ContainerScope = ""
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
	case ScopeNone, ScopeAll, ScopeMain, ScopeInit:
		return true
	}
	return false
}

// SelectsContainers reports whether the subject is a container rather than an
// object.
func (s ContainerScope) SelectsContainers() bool { return s != ScopeNone }

// Validate reports every problem with an applicability declaration.
func (a AppliesTo) Validate() []error {
	var errs []error
	if !a.Containers.Valid() {
		errs = append(errs, fmt.Errorf("appliesTo.containers %q must be one of all, main, init, or omitted", a.Containers))
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
	Required []string `yaml:"required,omitempty" json:"required,omitempty"`
	// Forbidden paths must not exist.
	Forbidden []string `yaml:"forbidden,omitempty" json:"forbidden,omitempty"`
	// Equals is exact equality, path to value.
	Equals map[string]any `yaml:"equals,omitempty" json:"equals,omitempty"`
	// OneOf constrains a path to a set.
	OneOf map[string][]any `yaml:"oneOf,omitempty" json:"oneOf,omitempty"`
	// Matches is an RE2 match on the string form of a path.
	Matches map[string]string `yaml:"matches,omitempty" json:"matches,omitempty"`
	// EqualPaths asserts two paths hold the same value - the requests-equal-
	// limits family, which is otherwise an expression in every check that needs
	// it.
	EqualPaths []PathPair `yaml:"equalPaths,omitempty" json:"equalPaths,omitempty"`
	// Numeric bounds a path parsed as a Kubernetes quantity, so "250m", "1Gi"
	// and "0.5" are all comparable without the author knowing the suffix rules.
	Numeric map[string]Bound `yaml:"numeric,omitempty" json:"numeric,omitempty"`

	// Expr is CEL, for everything the forms above cannot say. True means
	// compliant.
	Expr string `yaml:"expr,omitempty" json:"expr,omitempty"`

	// Observed, Expected, Locus and Message override what the shorthand would
	// have derived. Observed and Message are CEL expressions returning a
	// string, so a finding can name the value that offended.
	Observed string `yaml:"observed,omitempty" json:"observed,omitempty"`
	Expected string `yaml:"expected,omitempty" json:"expected,omitempty"`
	Locus    string `yaml:"locus,omitempty" json:"locus,omitempty"`
	Message  string `yaml:"message,omitempty" json:"message,omitempty"`
}

// PathPair is the two operands of an equalPaths assertion.
type PathPair struct {
	A string
	B string
}

// UnmarshalYAML accepts the two-element list form the manifest uses:
//
//	equalPaths: [resources.requests.cpu, resources.limits.cpu]
//	equalPaths:
//	  - [resources.requests.cpu, resources.limits.cpu]
//	  - [resources.requests.memory, resources.limits.memory]
func (p *PathPair) UnmarshalYAML(unmarshal func(any) error) error {
	var pair []string
	if err := unmarshal(&pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("equalPaths entry needs exactly two paths, got %d", len(pair))
	}
	p.A, p.B = pair[0], pair[1]
	return nil
}

// Bound is a numeric range. Either end may be omitted.
type Bound struct {
	Min *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max *float64 `yaml:"max,omitempty" json:"max,omitempty"`
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
	APIVersion string       `yaml:"apiVersion" json:"apiVersion"`
	Kind       string       `yaml:"kind" json:"kind"`
	Metadata   PackMetadata `yaml:"metadata" json:"metadata"`
	Spec       PackSpec     `yaml:"spec" json:"spec"`
}

// PackMetadata identifies a pack and the ID namespace it owns.
type PackMetadata struct {
	Name string `yaml:"name" json:"name"`
	// Prefix is the check-ID namespace this pack OWNS. Two packs claiming one
	// prefix is a load error for the second, named in the message. This is what
	// makes an ID globally unique with no central registry and no coordination
	// between the teams writing packs.
	Prefix      string `yaml:"prefix" json:"prefix"`
	Version     string `yaml:"version,omitempty" json:"version,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Maintainer  string `yaml:"maintainer,omitempty" json:"maintainer,omitempty"`
	Reference   string `yaml:"reference,omitempty" json:"reference,omitempty"`
}

// PackSpec carries the checks. Prefixes is for a pack that legitimately owns
// several ID namespaces - the shipped baseline owns thirteen, one per category
// of the organization's source catalogue, so the IDs in their existing document
// are the IDs in the tool and nobody has to translate.
type PackSpec struct {
	Prefixes []string `yaml:"prefixes,omitempty" json:"prefixes,omitempty"`
	Checks   []Check  `yaml:"checks" json:"checks"`
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
