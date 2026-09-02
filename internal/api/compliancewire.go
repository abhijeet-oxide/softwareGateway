package api

import (
	"errors"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The compliance API's wire shapes.
//
// # Why these are not the store rows
//
// A store row is a database concern and it changes when an index does. What a
// client renders is a contract, and the two drifting apart is how a UI ends up
// reaching into columns nobody meant to expose. The conversion is dull on
// purpose.
//
// # Why the labels are here and not in the interface
//
// "Compliant with warnings", "Overridable in values", "Could not be checked" -
// each of these is a sentence with a precise meaning that the model owns. Two
// copies of that vocabulary, one in Go and one in TypeScript, is two places
// for them to diverge, and the divergence would be a screen that says
// something the engine did not mean.

// ComplianceRunView is one run, without its results.
type ComplianceRunView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	// VerdictLabel is the verdict as the interface states it.
	VerdictLabel string `json:"verdictLabel,omitempty"`

	// Provenance. Rule 5: a report that cannot say what produced it cannot be
	// re-derived, and re-deriving it is what happens when a vendor disputes a
	// finding.
	BundleDigest string `json:"bundleDigest,omitempty"`
	HelmVersion  string `json:"helmVersion,omitempty"`
	KubeVersion  string `json:"kubeVersion,omitempty"`
	Checks       int    `json:"checks"`

	Counts    ComplianceCounts `json:"counts"`
	Truncated bool             `json:"truncated,omitempty"`
	Trigger   string           `json:"trigger,omitempty"`

	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	// Log is the run's transcript, in the same shape the live panel reads. On
	// the finished run because the question the timeline answers - which charts
	// refused, and what the nine minutes went on - is asked after the run rather
	// than during it, and until this was stored the answer disappeared with the
	// Coordinator's memory the moment the check ended.
	Log []compliance.ProgressEvent `json:"log,omitempty"`
	// LogTruncated says the transcript is at the ring's cap, so lines were
	// dropped from the front. A log that silently begins in the middle of a run
	// is one somebody reads as the whole run.
	LogTruncated bool `json:"logTruncated,omitempty"`
}

// ComplianceCounts is the tally. Severity counts are of FAILURES only: a
// passing blocking check is not a blocking anything, and counting it under
// "blocking" produces the number nobody can interpret.
type ComplianceCounts struct {
	Pass   int `json:"pass"`
	Fail   int `json:"fail"`
	Skip   int `json:"skip"`
	Error  int `json:"error"`
	Waived int `json:"waived"`

	Blocking int `json:"blocking"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`

	// The DISTINCT checks behind those numbers.
	//
	// A release breaks five rules in a hundred and seventy-one places. "171" is
	// how much replacing there is to do; "5" is how many conversations, and it
	// is the number somebody means when they ask how many problems a release
	// has. Produced by the server because the interface groups the rows it was
	// sent, so a count taken from those would be a count of the page.
	UniqueBlocking int `json:"uniqueBlocking"`
	UniqueWarning  int `json:"uniqueWarning"`
	UniqueInfo     int `json:"uniqueInfo"`
}

// ComplianceChartView is one chart's contribution - the run's denominator.
type ComplianceChartView struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	// ErrorKind classifies the failure, ErrorLabel names it in the words the
	// table shows, and ErrorHint says what the reader does about it. Carried
	// rather than mapped in the interface so the two cannot drift, and because
	// an export a vendor opens has no interface to map it with.
	ErrorKind  string `json:"errorKind,omitempty"`
	ErrorLabel string `json:"errorLabel,omitempty"`
	ErrorHint  string `json:"errorHint,omitempty"`
	// ErrorValue is the values key the chart demanded, pulled out of helm's
	// paragraph. Six of the eight charts that failed in a real orb failed for
	// one reason - a `global.registry` an umbrella supplies - and the only
	// thing their eight different messages had in common was that key.
	ErrorValue string `json:"errorValue,omitempty"`
	// ErrorFile is the template helm named, and ErrorInTest says it is a helm
	// test hook. `helm install` never applies one, so a chart failing only
	// there installs perfectly and still cannot be checked - a distinction a
	// vendor needs before they dismiss the finding.
	ErrorFile   string `json:"errorFile,omitempty"`
	ErrorInTest bool   `json:"errorInTest,omitempty"`
	// Attempts is how many renders were tried, and Retryable whether a further
	// one could have helped. "Retried and failed again" and "not retried,
	// because a second render of the same bytes returns the same error" are
	// different facts.
	Attempts int `json:"attempts,omitempty"`
	// Not omitempty: FALSE is the informative value here. "Not retried, because
	// a second render of the same bytes returns the same error" is a thing the
	// coverage table says, and an omitted false reaches the interface as
	// "unknown" - which it then cannot say anything about.
	Retryable bool `json:"retryable"`

	Resources int `json:"resources"`
}

// ComplianceResultView is one finding, addressed.
//
// Every address field is here even when a client could derive it, because
// deriving it needs the release - and the most important consumer of this shape
// is an export a vendor opens without access to this platform.
type ComplianceResultView struct {
	// Seq is this result's position in the run, which is its identity there.
	// Carried so the interface can ask for the rendered manifest THIS result
	// was judged against without describing it: an excerpt is a claim about
	// what the run found, so the run has to be what says where to point.
	Seq int `json:"seq"`

	Check       string `json:"check"`
	Title       string `json:"title,omitempty"`
	Severity    string `json:"severity"`
	Category    string `json:"category,omitempty"`
	Pack        string `json:"pack,omitempty"`
	Tier        int    `json:"tier,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	Reference   string `json:"reference,omitempty"`

	Outcome      string `json:"outcome"`
	OutcomeLabel string `json:"outcomeLabel"`
	// Determinacy is the difference between the vendor's defect and the site's
	// decision, which is the first split somebody makes triaging a report.
	Determinacy      string `json:"determinacy,omitempty"`
	DeterminacyLabel string `json:"determinacyLabel,omitempty"`

	Chart          string `json:"chart,omitempty"`
	ChartVersion   string `json:"chartVersion,omitempty"`
	SubchartPath   string `json:"subchartPath,omitempty"`
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	ArtifactRef    string `json:"artifactRef,omitempty"`
	SourceFile     string `json:"sourceFile,omitempty"`
	RenderedLine   int    `json:"renderedLine,omitempty"`
	APIVersion     string `json:"apiVersion,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	Name           string `json:"name,omitempty"`
	Container      string `json:"container,omitempty"`
	ContainerType  string `json:"containerType,omitempty"`
	Locus          string `json:"locus,omitempty"`

	Observed string `json:"observed,omitempty"`
	Expected string `json:"expected,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    string `json:"error,omitempty"`

	Waiver        string     `json:"waiver,omitempty"`
	WaiverExpires *time.Time `json:"waiverExpires,omitempty"`
	Fingerprint   string     `json:"fingerprint,omitempty"`
}

// PackageComplianceView is what a release's compliance tab reads.
type PackageComplianceView struct {
	Product string `json:"product"`
	Release string `json:"release"`

	// Run is absent when the release has never been checked. ABSENT MEANS NOT
	// CHECKED, and the interface must render it as such - never as a pass.
	Run *ComplianceRunView `json:"run,omitempty"`
	// Progress is present only while a run is live, so the tab can poll one
	// endpoint rather than two.
	Progress *compliance.Progress `json:"progress,omitempty"`

	Charts  []ComplianceChartView  `json:"charts,omitempty"`
	Results []ComplianceResultView `json:"results,omitempty"`
	// Total is the count BEFORE the page was taken. A page with no total lies
	// by omission: forty findings and the first forty of nine hundred look the
	// same otherwise.
	Total int `json:"total"`
	// Helm reports whether this Coordinator can render charts at all. Without
	// it, a tab full of "could not be checked" has no explanation on screen.
	Helm ComplianceHelmView `json:"helm"`
	// Analysed says whether this release's manifest tree has been walked.
	//
	// A run needs the chart artifacts' LAYER digests, and those are recorded by
	// the walk. Before it there is nothing to fetch - so this is on the wire and
	// the tab offers the walk, rather than offering a button that fails and
	// leaves a recorded failure explaining something the reader could have been
	// told first.
	Analysed bool `json:"analysed"`
}

// ComplianceHelmView is the renderer's availability.
type ComplianceHelmView struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ComplianceListingView is the Software listing's column.
type ComplianceListingView struct {
	State    string `json:"state,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
	Label    string `json:"label,omitempty"`
	Blocking int    `json:"blocking"`
	Warning  int    `json:"warning"`
	Error    int    `json:"error"`
	Pass     int    `json:"pass"`
	// The DISTINCT checks behind Blocking and Warning. What the tab label and
	// the listing show: "5 rules" is the number somebody means when they ask
	// how many problems a release has, and "171 places" is how much editing
	// there is to do.
	UniqueBlocking int `json:"uniqueBlocking"`
	UniqueWarning  int `json:"uniqueWarning"`

	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

// PolicyCatalogueView is the rulebook: what will be checked, and why.
type PolicyCatalogueView struct {
	BundleDigest string            `json:"bundleDigest,omitempty"`
	Packs        []PolicyPackView  `json:"packs"`
	Checks       []PolicyCheckView `json:"checks"`
}

// PolicyPackView is one pack and whether it loaded.
type PolicyPackView struct {
	Name        string   `json:"name"`
	Prefixes    []string `json:"prefixes,omitempty"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Maintainer  string   `json:"maintainer,omitempty"`
	Reference   string   `json:"reference,omitempty"`
	Builtin     bool     `json:"builtin,omitempty"`
	Checks      int      `json:"checks"`
	// Errors is why a pack did not load. Surfaced rather than logged: a pack
	// that failed is a set of checks that will report `error`, and the reader
	// needs to know which and why.
	Errors []string `json:"errors,omitempty"`
}

// PolicyCheckView is one rule, in full.
//
// This is what a vendor reads when they ask what will be checked before they
// ship, and what a reviewer reads when settling an argument about a finding.
type PolicyCheckView struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Rationale   string `json:"rationale,omitempty"`
	Severity    string `json:"severity"`
	Tier        int    `json:"tier,omitempty"`
	Category    string `json:"category,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	Reference   string `json:"reference,omitempty"`
	Pack        string `json:"pack,omitempty"`
	Engine      string `json:"engine,omitempty"`

	// AppliesTo is what the check judges, in words. A reader arguing about a
	// finding asks "does this even apply to my CronJob?" first.
	AppliesTo    string `json:"appliesTo,omitempty"`
	Deprecated   bool   `json:"deprecated,omitempty"`
	SupersededBy string `json:"supersededBy,omitempty"`
}

// ---------------------------------------------------------------------------
// Conversions

func complianceRunView(r store.ComplianceRunRow) ComplianceRunView {
	return ComplianceRunView{
		ID: r.ID, State: r.State, Error: r.Error,
		Verdict:      r.Verdict,
		VerdictLabel: compliance.Verdict(r.Verdict).Label(),
		BundleDigest: r.BundleDigest, HelmVersion: r.HelmVersion, KubeVersion: r.KubeVersion,
		Checks: r.Checks,
		Counts: ComplianceCounts{
			Pass: r.Pass, Fail: r.Fail, Skip: r.Skip, Error: r.Errors, Waived: r.Waived,
			Blocking: r.Blocking, Warning: r.Warning, Info: r.Info,
		},
		Truncated: r.Truncated, Trigger: r.Trigger,
		Log:          r.Log,
		LogTruncated: len(r.Log) >= compliance.MaxLogEvents,
		StartedAt:    r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

func complianceChartViews(rows []store.ComplianceChartRow) []ComplianceChartView {
	out := make([]ComplianceChartView, 0, len(rows))
	for _, c := range rows {
		v := ComplianceChartView{
			Name: c.Name, Version: c.Version, Digest: c.ArtifactDigest, Ref: c.ArtifactRef,
			Status: c.Status, Error: c.Error, ErrorKind: c.ErrorKind,
			Attempts: c.Attempts, Resources: c.Resources,
		}
		// A run recorded before failures were classified has an error and no
		// kind. Classifying it on read costs one string scan and turns an old
		// run's coverage table from a column of stack traces into the same
		// grouped view a new one gets.
		if v.ErrorKind == "" && c.Error != "" {
			v.ErrorKind = string(render.ClassifyFailure(errors.New(c.Error)))
		}
		if kind := render.FailureKind(v.ErrorKind); kind != "" {
			v.ErrorLabel = kind.Label()
			v.ErrorHint = kind.Explain()
			v.Retryable = kind.Retryable()
		}
		// Derived on read rather than stored, for the same reason the kind is
		// re-classified above: both are functions of helm's message, so a run
		// recorded before either existed gets the same coverage table a new one
		// does, and no column has to be kept in step with a parser.
		if c.Error != "" {
			err := errors.New(c.Error)
			v.ErrorValue = render.MissingValue(err)
			v.ErrorFile = render.FailingTemplate(err)
			v.ErrorInTest = render.InTestHook(err)
		}
		out = append(out, v)
	}
	return out
}

func complianceResultViews(rows []store.ComplianceResultRow) []ComplianceResultView {
	out := make([]ComplianceResultView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ComplianceResultView{
			Seq:   r.Seq,
			Check: r.CheckID, Title: r.CheckTitle, Severity: r.Severity,
			Category: r.Category, Pack: r.Pack, Tier: r.Tier,
			Remediation: r.Remediation, Reference: r.Reference,

			Outcome:          r.Outcome,
			OutcomeLabel:     compliance.Outcome(r.Outcome).Label(),
			Determinacy:      r.Determinacy,
			DeterminacyLabel: compliance.Determinacy(r.Determinacy).Label(),

			Chart: r.Chart, ChartVersion: r.ChartVersion, SubchartPath: r.SubchartPath,
			ArtifactDigest: r.ArtifactDigest, ArtifactRef: r.ArtifactRef,
			SourceFile: r.SourceFile, RenderedLine: r.RenderedLine,
			APIVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
			Container: r.Container, ContainerType: r.ContainerType, Locus: r.Locus,

			Observed: r.Observed, Expected: r.Expected, Message: r.Message, Error: r.Error,
			Waiver: r.Waiver, WaiverExpires: r.WaiverExpires, Fingerprint: r.Fingerprint,
		})
	}
	return out
}

func complianceListingView(r store.PackageComplianceRow) ComplianceListingView {
	return ComplianceListingView{
		State: r.State, Verdict: r.Verdict,
		Label:    compliance.Verdict(r.Verdict).Label(),
		Blocking: r.Blocking, Warning: r.Warning, Error: r.Errors, Pass: r.Pass,
		UniqueBlocking: r.UniqueBlocking, UniqueWarning: r.UniqueWarning,
		CheckedAt: r.CheckedAt,
	}
}

func policyCatalogueView(cat *compliance.Catalog) PolicyCatalogueView {
	out := PolicyCatalogueView{BundleDigest: cat.BundleDigest}
	for _, p := range cat.Packs() {
		out.Packs = append(out.Packs, PolicyPackView{
			Name: p.Name, Prefixes: p.Prefixes, Version: p.Version,
			Description: p.Description, Maintainer: p.Maintainer, Reference: p.Reference,
			Builtin: p.Builtin, Checks: p.Checks, Errors: p.Errors,
		})
	}
	for _, c := range cat.Checks() {
		out.Checks = append(out.Checks, PolicyCheckView{
			ID: c.ID, Title: c.Title, Description: c.Description, Rationale: c.Rationale,
			Severity: string(c.Severity), Tier: int(c.Tier), Category: c.Category,
			Remediation: c.Remediation, Reference: c.Reference, Pack: c.Pack,
			Engine:     c.EngineName(),
			AppliesTo:  describeAppliesTo(c.AppliesTo),
			Deprecated: c.Deprecated, SupersededBy: c.SupersededBy,
		})
	}
	return out
}

// describeAppliesTo renders a check's denominator as a sentence.
//
// A reader arguing about a finding asks "does this apply to my CronJob at all?"
// before anything else, and the YAML answer is a nested structure they should
// not have to read.
func describeAppliesTo(a compliance.AppliesTo) string {
	kinds := "every resource"
	if len(a.Kinds) > 0 {
		kinds = joinWords(a.Kinds)
	}
	switch {
	case a.Containers.SelectsContainers():
		scope := "every container"
		switch a.Containers {
		case compliance.ScopeMain:
			scope = "each main container"
		case compliance.ScopeInit:
			scope = "each init container"
		}
		return scope + " of " + kinds
	case a.Subject == compliance.SubjectPodSpec:
		return "the pod spec of " + kinds
	default:
		return kinds
	}
}

func joinWords(in []string) string {
	switch len(in) {
	case 0:
		return ""
	case 1:
		return in[0]
	case 2:
		return in[0] + " and " + in[1]
	}
	out := ""
	for i, s := range in[:len(in)-1] {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out + " and " + in[len(in)-1]
}
