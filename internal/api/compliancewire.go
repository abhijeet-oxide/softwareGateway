package api

import (
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
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
}

// ComplianceChartView is one chart's contribution - the run's denominator.
type ComplianceChartView struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Resources int    `json:"resources"`
}

// ComplianceResultView is one finding, addressed.
//
// Every address field is here even when a client could derive it, because
// deriving it needs the release - and the most important consumer of this shape
// is an export a vendor opens without access to this platform.
type ComplianceResultView struct {
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
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

func complianceChartViews(rows []store.ComplianceChartRow) []ComplianceChartView {
	out := make([]ComplianceChartView, 0, len(rows))
	for _, c := range rows {
		out = append(out, ComplianceChartView{
			Name: c.Name, Version: c.Version, Digest: c.ArtifactDigest, Ref: c.ArtifactRef,
			Status: c.Status, Error: c.Error, Resources: c.Resources,
		})
	}
	return out
}

func complianceResultViews(rows []store.ComplianceResultRow) []ComplianceResultView {
	out := make([]ComplianceResultView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ComplianceResultView{
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
