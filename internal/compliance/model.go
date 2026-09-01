// Package compliance decides whether a vendor's release follows this
// organization's own Kubernetes and CNF standards.
//
// It is the third of three questions the platform answers about a release, and
// the three are deliberately kept apart because they fail independently:
//
//	verification  is it authentic?          signatures, cosign
//	security      is it safe?               Xray, CVEs
//	compliance    is it built as we require? this package
//
// The rules a result has to obey are ground truth, argued in
// docs/compliance/00-compliance-model.md, and this file is where they become
// types. Five of them shape everything here:
//
//  1. A result is about ONE resource. Not "the chart" - a finding a release
//     engineer cannot paste into a vendor ticket is not a finding.
//  2. A pass is a result, and so is a skip. An engine emitting only violations
//     reports "compliant", "not applicable" and "the traversal never reached
//     it" identically, and the third is what the organization's existing Rego
//     does today.
//  3. Severity belongs to the check; outcome belongs to the result. Two fields,
//     because a check that picks its own severity per finding has no severity.
//  4. Say what is actually known. A value that a values file can override is
//     not the same fact as one the template fixes, and reporting them alike is
//     how tier-1 checking earns a reputation for crying wolf.
//  5. Reproducible, or it is an opinion. Every run records what produced it.
package compliance

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Outcome is what happened when one check was evaluated against one subject.
//
// # Why `error` is an outcome and not an error return
//
// A check that could not be decided has told us something, and what it told us
// is not "pass". The alternative - dropping undecidable checks and returning an
// error from the run - loses the identity of what could not be decided, which
// is the only thing a person can act on. So `helm` being absent produces a
// wall of `error` results naming the charts that could not be rendered, the run
// is reported inconclusive, and nobody reads a green screen over an unrendered
// release.
type Outcome string

const (
	// OutcomePass means the check applied to this subject and the subject
	// satisfies it.
	OutcomePass Outcome = "pass"
	// OutcomeFail means the check applied and the subject does not satisfy it.
	OutcomeFail Outcome = "fail"
	// OutcomeSkip means the check did not apply. Recorded rather than omitted,
	// because "no PodDisruptionBudget checks ran, this release has no
	// workloads" and "the PDB checks did not load" look identical otherwise.
	OutcomeSkip Outcome = "skip"
	// OutcomeError means the check could not be decided: the chart would not
	// render, the expression faulted, the pack failed to load. Never folded
	// into pass or fail.
	OutcomeError Outcome = "error"
	// OutcomeWaived means the check failed and an accepted, scoped, expiring
	// waiver covers it. Kept distinct from pass so a waiver is visible for
	// exactly as long as it exists, and so its expiry can be reported.
	OutcomeWaived Outcome = "waived"
)

// Decided reports whether this outcome carries a judgement about the subject.
//
// Skips and errors do not. Coverage arithmetic and verdicts both key off this
// rather than counting rows, so an inconclusive run can never be summarized as
// a percentage that hides what was not looked at.
func (o Outcome) Decided() bool { return o == OutcomePass || o == OutcomeFail || o == OutcomeWaived }

// Label is the outcome in the words the interface and the report use.
func (o Outcome) Label() string {
	switch o {
	case OutcomePass:
		return "Pass"
	case OutcomeFail:
		return "Fail"
	case OutcomeSkip:
		return "Not applicable"
	case OutcomeError:
		return "Could not be checked"
	case OutcomeWaived:
		return "Waived"
	default:
		return "Unknown"
	}
}

// Valid reports whether o is one of the five outcomes. Used when reading a
// stored row or a fixture's expectations, where an unrecognized value must be
// rejected rather than silently treated as a pass.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomePass, OutcomeFail, OutcomeSkip, OutcomeError, OutcomeWaived:
		return true
	}
	return false
}

// Severity is how much this organization cares, declared once on the check.
//
// It is not on the result. A check cannot decide that this instance of its own
// violation matters less than that one - if two cases genuinely differ in
// consequence they are two checks with two IDs, which is also the only form a
// vendor can act on separately.
type Severity string

const (
	// SeverityBlock is a violation the organization will not accept. It decides
	// the verdict.
	SeverityBlock Severity = "block"
	// SeverityWarn is a violation worth a conversation with the vendor. It does
	// not fail the release on its own.
	SeverityWarn Severity = "warn"
	// SeverityInfo is an observation. It never fails anything, and exists so a
	// check can be introduced and measured before it is given teeth.
	SeverityInfo Severity = "info"
)

// Rank orders severities for sorting, most consequential first.
func (s Severity) Rank() int {
	switch s {
	case SeverityBlock:
		return 0
	case SeverityWarn:
		return 1
	case SeverityInfo:
		return 2
	default:
		return 3
	}
}

// Label is the severity as the interface writes it.
func (s Severity) Label() string {
	switch s {
	case SeverityBlock:
		return "Blocking"
	case SeverityWarn:
		return "Warning"
	case SeverityInfo:
		return "Informational"
	default:
		return "Unknown"
	}
}

// Valid reports whether s is one of the three severities.
func (s Severity) Valid() bool {
	switch s {
	case SeverityBlock, SeverityWarn, SeverityInfo:
		return true
	}
	return false
}

// Determinacy says how firmly a rendered value is established.
//
// # Why this exists at all
//
// Tier-1 checking judges charts without the site's values file. A finding on a
// value the operator will override anyway is noise, and enough of it is how a
// checking tool becomes something people close. But refusing to report anything
// overridable would leave almost nothing, because a Helm chart's whole purpose
// is that its values are overridable.
//
// So the engine measures instead of assuming: it renders twice - once with the
// chart's own values, once with them perturbed - and compares. A value that did
// not move is fixed by the template and the vendor must change the chart. A
// value that moved is a default, and the finding is a question for the site's
// values file rather than a defect in the delivery.
//
// This is the mechanism that lets a tier-1 result block without lying.
type Determinacy string

const (
	// DeterminacyFixed means the template sets this value and no values file
	// can change it. A failure here is the vendor's to fix.
	DeterminacyFixed Determinacy = "fixed"
	// DeterminacyConfigurable means the value came from the chart's defaults
	// and a values file can override it. A failure here is a question for
	// whoever writes the site values, not necessarily a defect.
	DeterminacyConfigurable Determinacy = "configurable"
	// DeterminacyUnknown means the probe could not establish which. Reported as
	// itself: guessing "fixed" invents vendor defects and guessing
	// "configurable" excuses real ones.
	DeterminacyUnknown Determinacy = "unknown"
	// DeterminacyNA means the question does not apply - a chart-structure check
	// reading Chart.yaml, where there is no rendering and nothing to override.
	DeterminacyNA Determinacy = "na"
)

// Label is the determinacy in the words the report uses, chosen so a vendor
// engineer reading the spreadsheet knows whose problem it is.
func (d Determinacy) Label() string {
	switch d {
	case DeterminacyFixed:
		return "Fixed by the chart"
	case DeterminacyConfigurable:
		return "Overridable in values"
	case DeterminacyUnknown:
		return "Could not be established"
	case DeterminacyNA:
		return "Not applicable"
	default:
		return "Unknown"
	}
}

// Valid reports whether d is one of the four determinacies.
func (d Determinacy) Valid() bool {
	switch d {
	case DeterminacyFixed, DeterminacyConfigurable, DeterminacyUnknown, DeterminacyNA:
		return true
	}
	return false
}

// Tier separates what can be decided from the delivery alone from what needs
// the site's own values.
type Tier int

const (
	// Tier1 is decidable from the delivered artifacts: chart structure, and
	// manifests rendered with the chart's own defaults.
	Tier1 Tier = 1
	// Tier2 needs a site values file to decide. Catalogued and reported as
	// deferred rather than attempted, because a tier-2 check evaluated without
	// its values is a coin toss dressed as a result.
	Tier2 Tier = 2
)

// Verdict is the whole-run answer, derived from results and never stored
// independently of them.
type Verdict string

const (
	// VerdictPass means every applicable check was decided and none of the
	// blocking ones failed.
	VerdictPass Verdict = "pass"
	// VerdictConditional means blocking checks all passed but warnings stand.
	VerdictConditional Verdict = "conditional"
	// VerdictFail means at least one blocking check failed unwaived.
	VerdictFail Verdict = "fail"
	// VerdictInconclusive means something could not be decided. It outranks
	// pass and conditional deliberately: a run that could not render 40 charts
	// is not a run that found nothing wrong with them.
	VerdictInconclusive Verdict = "inconclusive"
)

// Label is the verdict as the interface states it.
func (v Verdict) Label() string {
	switch v {
	case VerdictPass:
		return "Compliant"
	case VerdictConditional:
		return "Compliant with warnings"
	case VerdictFail:
		return "Not compliant"
	case VerdictInconclusive:
		return "Inconclusive"
	default:
		return "Unknown"
	}
}

// Address is where a result is, precisely enough to act on without this tool.
//
// # Why it has this many fields
//
// The requirement it exists for is that a finding can be sent to a vendor and
// acted on by an engineer who has never seen this platform. That person needs
// to open a file. "Deployment/foo has no resource limits" does not let them:
// there are four charts in the release with a Deployment called foo, two of
// them from a subchart, and the field is set in a template they have to find.
//
// Every field below is one step of that path, and the ones a person could
// derive are still recorded because deriving them requires the release, which
// they do not have.
//
// # Why it is handed to checks rather than constructed by them
//
// The engine builds the address and binds it; a check echoes it back and never
// composes one. A check that constructed its own would get the source file
// wrong on a subchart, or omit the artifact digest, or spell the chart name
// from a label instead of from the chart. None of those are hypothetical - they
// are what the address looks like in the sample policies, which stop at
// kind/namespace/name.
type Address struct {
	// Product and Release name the delivery, so a finding survives being
	// exported and read six months later.
	Product string `json:"product"`
	Release string `json:"release,omitempty"`
	// PackageDigest is the release's own digest: the one thing in this struct
	// that cannot be re-pointed at different bytes later.
	PackageDigest string `json:"packageDigest,omitempty"`

	// ArtifactDigest identifies the chart artifact inside the release, and
	// ArtifactRef is where it sits in the repository. Together they are what a
	// vendor pulls to reproduce the finding.
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	ArtifactRef    string `json:"artifactRef,omitempty"`

	// Chart and ChartVersion name the chart. Both, because a vendor ships the
	// same chart name at several versions across a release and the fix goes
	// into one of them.
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	// SubchartPath is the dependency path when the resource came from a
	// subchart - "mysvc/charts/redis". Without it a vendor looks for the
	// template in the parent chart and does not find it.
	SubchartPath string `json:"subchartPath,omitempty"`

	// SourceFile is the template the resource was rendered from, taken from
	// helm's own "# Source:" marker rather than guessed, and RenderedLine is
	// where in the rendered stream it began. The first is what a vendor edits;
	// the second is what makes the finding reproducible from the run's
	// recorded output.
	SourceFile   string `json:"sourceFile,omitempty"`
	RenderedLine int    `json:"renderedLine,omitempty"`

	// The Kubernetes object itself.
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`

	// Container is set when the subject is a container rather than the whole
	// object, and ContainerType says which list it came from. A Deployment with
	// eight containers produces eight results, not one, because the vendor
	// fixes them one at a time.
	Container     string `json:"container,omitempty"`
	ContainerType string `json:"containerType,omitempty"` // main | init | ephemeral

	// Locus is the field the check judged - "spec.template.spec.containers[2].
	// resources.limits.memory". This is the difference between a finding a
	// vendor can act on in a minute and one they have to investigate.
	Locus string `json:"locus,omitempty"`
}

// Container types, as ContainerType records them.
const (
	ContainerMain      = "main"
	ContainerInit      = "init"
	ContainerEphemeral = "ephemeral"
)

// Resource returns the Kubernetes object as a person names it in a ticket:
// "apps/v1 Deployment ns/name". Empty when the address is not about an object.
func (a Address) Resource() string {
	if a.Kind == "" {
		return ""
	}
	var b strings.Builder
	if a.APIVersion != "" {
		b.WriteString(a.APIVersion)
		b.WriteString(" ")
	}
	b.WriteString(a.Kind)
	b.WriteString(" ")
	if a.Namespace != "" {
		b.WriteString(a.Namespace)
		b.WriteString("/")
	}
	b.WriteString(a.Name)
	return b.String()
}

// Where is the address as one line, for a CLI and for the "where" column of the
// vendor report. It reads outside-in, the way a person navigates to it.
func (a Address) Where() string {
	parts := make([]string, 0, 5)
	if a.Chart != "" {
		c := a.Chart
		if a.ChartVersion != "" {
			c += ":" + a.ChartVersion
		}
		parts = append(parts, c)
	}
	if a.SourceFile != "" {
		parts = append(parts, a.SourceFile)
	}
	if r := a.Resource(); r != "" {
		parts = append(parts, r)
	}
	if a.Container != "" {
		parts = append(parts, "container "+a.Container)
	}
	if a.Locus != "" {
		parts = append(parts, a.Locus)
	}
	return strings.Join(parts, " → ")
}

// Result is one check's judgement about one subject.
//
// This is the unit everything else is built from: the UI groups them, the
// export writes one per row, the comparison aligns them by fingerprint. It is
// denormalized on purpose - the check's title and severity are copied in rather
// than joined - because a result exported to a vendor has to stay readable
// after the check it came from has been edited, and because a report is read
// far more often than it is written.
type Result struct {
	// CheckID is the permanent identity - "PDB-02". It outlives this release,
	// appears in waivers and vendor tickets, and is what a comparison joins on.
	CheckID string `json:"checkId"`
	// The check's own metadata, copied at evaluation time so the row explains
	// itself with nothing else loaded.
	CheckTitle  string   `json:"checkTitle,omitempty"`
	Severity    Severity `json:"severity"`
	Tier        Tier     `json:"tier,omitempty"`
	Category    string   `json:"category,omitempty"`
	Pack        string   `json:"pack,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	Reference   string   `json:"reference,omitempty"`

	Outcome     Outcome     `json:"outcome"`
	Determinacy Determinacy `json:"determinacy,omitempty"`
	Address     Address     `json:"address"`

	// Observed and Expected are what the check saw and what it required, in the
	// vendor's own units. A report that says only "failed" makes the vendor
	// reproduce the analysis to learn what value offended.
	Observed string `json:"observed,omitempty"`
	Expected string `json:"expected,omitempty"`
	// Message is one sentence naming the subject and the problem. Written for
	// somebody who does not have this screen open.
	Message string `json:"message,omitempty"`

	// Waiver identifies the accepted exception when Outcome is waived, and
	// WaiverExpires is when it stops applying. Both are shown, because a waiver
	// whose expiry nobody can see is a permanent exception.
	Waiver        string     `json:"waiver,omitempty"`
	WaiverExpires *time.Time `json:"waiverExpires,omitempty"`

	// Error carries why an undecidable check could not be decided - helm's
	// stderr, the expression fault, the pack's load error. Present only for
	// OutcomeError, and never empty when it is: an error with no reason is a
	// dead end for whoever has to fix it.
	Error string `json:"error,omitempty"`
}

// Fingerprint is the identity of a finding ACROSS releases.
//
// # What it deliberately excludes
//
// Chart version and release tag. A vendor who ships the same missing memory
// limit in 23.8.1076 and again in 23.9.0001 has one unfixed defect, and a
// fingerprint including the version would report it as one fixed and one new -
// which is exactly the report that makes a comparison useless, because every
// release looks like a complete turnover.
//
// # What it includes and why
//
// The check, the chart by name, the source file, the object's identity and the
// container. The source file rather than the rendered line, because inserting a
// comment at the top of a template must not renumber every finding in it.
func (r Result) Fingerprint() string {
	a := r.Address
	return strings.Join([]string{
		r.CheckID,
		a.Chart,
		a.SubchartPath,
		a.SourceFile,
		a.APIVersion,
		a.Kind,
		a.Namespace,
		a.Name,
		a.Container,
		a.Locus,
	}, "\x1f")
}

// String renders a result the way the CLI prints it.
func (r Result) String() string {
	s := fmt.Sprintf("%-7s %-9s %s", strings.ToUpper(string(r.Outcome)), r.CheckID, r.Address.Where())
	if r.Message != "" {
		s += ": " + r.Message
	}
	return s
}

// Counts is the tally of a run, by outcome and by severity.
//
// Severity counts are of FAILURES only. A passing blocking check is not a
// blocking anything, and counting it under "block" produces the number nobody
// can interpret.
type Counts struct {
	Pass   int `json:"pass"`
	Fail   int `json:"fail"`
	Skip   int `json:"skip"`
	Error  int `json:"error"`
	Waived int `json:"waived"`

	Blocking int `json:"blocking"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

// Total is every result, including the ones that decided nothing.
func (c Counts) Total() int { return c.Pass + c.Fail + c.Skip + c.Error + c.Waived }

// Tally counts a slice of results.
func Tally(results []Result) Counts {
	var c Counts
	for _, r := range results {
		switch r.Outcome {
		case OutcomePass:
			c.Pass++
		case OutcomeFail:
			c.Fail++
		case OutcomeSkip:
			c.Skip++
		case OutcomeError:
			c.Error++
		case OutcomeWaived:
			c.Waived++
		}
		if r.Outcome != OutcomeFail {
			continue
		}
		switch r.Severity {
		case SeverityBlock:
			c.Blocking++
		case SeverityWarn:
			c.Warning++
		case SeverityInfo:
			c.Info++
		}
	}
	return c
}

// Decide is the run's verdict.
//
// # Why inconclusive outranks everything except a blocking failure
//
// An undecided check is not a passed check, and the order here is the whole
// point of Rule 2. A release whose charts would not render has not been shown
// to be compliant; reporting it as "pass, 0 findings" is the single most
// damaging thing this package could do, because it is indistinguishable from a
// clean result and it is wrong in the direction that ships.
//
// A blocking failure still wins over inconclusive: something definite is known
// to be wrong, and that is more actionable than "some of it could not be read".
func Decide(c Counts) Verdict {
	switch {
	case c.Blocking > 0:
		return VerdictFail
	case c.Error > 0:
		return VerdictInconclusive
	case c.Warning > 0:
		return VerdictConditional
	default:
		return VerdictPass
	}
}

// Sort orders results the way a person reads them: worst first, then stably by
// address so two runs of the same release produce byte-identical output.
//
// The stability is not cosmetic. "The same release checked twice is identical"
// is a merge gate, and a map-iteration order leaking into the output would make
// it flap.
func Sort(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if oa, ob := outcomeRank(a.Outcome), outcomeRank(b.Outcome); oa != ob {
			return oa < ob
		}
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() < b.Severity.Rank()
		}
		if a.Address.Chart != b.Address.Chart {
			return a.Address.Chart < b.Address.Chart
		}
		if a.Address.SourceFile != b.Address.SourceFile {
			return a.Address.SourceFile < b.Address.SourceFile
		}
		if a.Address.Kind != b.Address.Kind {
			return a.Address.Kind < b.Address.Kind
		}
		if a.Address.Namespace != b.Address.Namespace {
			return a.Address.Namespace < b.Address.Namespace
		}
		if a.Address.Name != b.Address.Name {
			return a.Address.Name < b.Address.Name
		}
		if a.Address.Container != b.Address.Container {
			return a.Address.Container < b.Address.Container
		}
		if a.CheckID != b.CheckID {
			return a.CheckID < b.CheckID
		}
		return a.Address.Locus < b.Address.Locus
	})
}

// outcomeRank puts the results a person needs to see at the top: failures,
// then the things nobody could decide, then waivers, then the passes and skips
// that are only evidence of coverage.
func outcomeRank(o Outcome) int {
	switch o {
	case OutcomeFail:
		return 0
	case OutcomeError:
		return 1
	case OutcomeWaived:
		return 2
	case OutcomePass:
		return 3
	case OutcomeSkip:
		return 4
	default:
		return 5
	}
}

// ShortDigest is a digest as a person refers to it in conversation and in a
// table column. Twelve hex characters, because that is what everything else in
// this project shows and a reader comparing two screens should not have to
// count characters.
func ShortDigest(d string) string {
	s := strings.TrimPrefix(d, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
