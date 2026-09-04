package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/export"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The compliance report.
//
// # Who this is for, and why it is not the screen
//
// The tab is for somebody triaging. The report is for somebody who is not going
// to open this platform at all: a vendor engineer sent a spreadsheet and asked
// to fix their chart, an auditor asked to show that a release was checked, a
// release manager pasting one number into a decision record.
//
// That reader has no filters, no drawer and no rendered manifest to click, so
// every row has to carry its whole address - the chart, the template, the LINE,
// the object, the container, the field - and the workbook has to open on a page
// that says what this is before it says what is wrong with it.
//
// # Why the grouped and the flat table are both in the file
//
// They answer different questions and neither is the other's summary. "Five
// rules are broken" is what a vendor gets a meeting about; "here are the 171
// places" is what they work through afterwards. Shipping one and calling it the
// report means somebody re-derives the other by hand in the same spreadsheet.
//
// # Why this is built server-side
//
// The same reason the security export is: a client can only export the page it
// has loaded, and the file would look complete. The read here is the whole run.

// handleExportPackageCompliance serves
// GET /products/{product}/packages/{package}/compliance/export.
func (s *Server) handleExportPackageCompliance(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.ComplianceStore == nil {
		Error(w, r, v1.CodeUnavailable, "compliance is not configured on this Coordinator")
		return
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	format, err := export.ParseFormat(r.URL.Query().Get("format"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	// A ZIP of a compliance run would be the rendered manifests, which have
	// their own endpoint and their own provenance header. Offering it here
	// would be a second name for that download.
	if format == export.FormatZIP {
		Error(w, r, v1.CodeInvalidArgument,
			"a compliance report is csv, xlsx or json; the rendered manifests have their own download")
		return
	}

	run, err := s.deps.ComplianceStore.LatestComplianceRun(r.Context(), pkg.ID)
	if err != nil {
		// A release nobody has checked has no report, and saying so is the
		// answer rather than serving an empty workbook that reads as a pass.
		Error(w, r, v1.CodeNotFound,
			"this release has not been checked, so there is no compliance report to export")
		return
	}

	charts, err := s.deps.ComplianceStore.ComplianceCharts(r.Context(), run.ID)
	if err != nil {
		s.internal(w, r, "read compliance charts for export", err)
		return
	}
	// EVERYTHING, not a page. See store.MaxComplianceResults.
	results, _, err := s.deps.ComplianceStore.ComplianceResults(r.Context(), run.ID,
		store.ComplianceFilter{Limit: store.MaxComplianceResults})
	if err != nil {
		s.internal(w, r, "read compliance results for export", err)
		return
	}
	unique, err := s.deps.ComplianceStore.ComplianceUniqueChecks(r.Context(), run.ID)
	if err != nil {
		s.internal(w, r, "count distinct compliance checks for export", err)
		return
	}

	book := complianceBook(productName, pkg, run, unique, charts, results, s.complianceHelm())
	markComplianceTable(&book, r.URL.Query().Get("table"))
	s.writeExport(w, r, format, book,
		[]string{productName, releaseLabel(pkg), "compliance"})
}

// markComplianceTable moves the single-table formats' choice of sheet.
//
// A workbook holds every table and a CSV holds one, and which one depends on
// what the reader is about to do: a vendor conversation wants the rules, the
// work afterwards wants the places. An unrecognised name leaves the book's own
// default, because a download is a link somebody already clicked and a 400 in a
// new tab is a worse answer than the sensible table.
func markComplianceTable(book *export.Book, table string) {
	want := map[string]string{
		"unique":    sheetUniqueFindings,
		"checks":    sheetUniqueFindings,
		"findings":  sheetAllFindings,
		"all":       sheetAllFindings,
		"unchecked": sheetUnchecked,
		"charts":    sheetCharts,
		"rulebook":  sheetRulebook,
		"summary":   sheetSummary,
	}[strings.ToLower(strings.TrimSpace(table))]
	if want == "" {
		return
	}
	found := false
	for i := range book.Sheets {
		if book.Sheets[i].Name == want {
			found = true
		}
	}
	if !found {
		return
	}
	for i := range book.Sheets {
		book.Sheets[i].Primary = book.Sheets[i].Name == want
	}
}

// The sheet names, in one place because two things address them: the workbook
// builds them and `table=` selects one.
const (
	sheetSummary        = "Summary"
	sheetUniqueFindings = "Unique findings"
	sheetAllFindings    = "All findings"
	sheetUnchecked      = "Unchecked"
	sheetCharts         = "Charts"
	sheetRulebook       = "Rulebook"
)

// complianceBook assembles the whole report.
func complianceBook(
	productName string, pkg store.PackageRow, run store.ComplianceRunRow,
	unique store.ComplianceUniqueCounts,
	charts []store.ComplianceChartRow, results []store.ComplianceResultRow,
	helm ComplianceHelmView,
) export.Book {
	release := releaseLabel(pkg)
	views := complianceResultViews(results)
	chartViews := complianceChartViews(charts)

	book := export.Book{Sheets: []export.Sheet{
		complianceSummarySheet(productName, release, pkg, run, unique, chartViews, helm),
		complianceUniqueSheet(productName, release, views),
		complianceFindingsSheet(productName, release, pkg.ManifestDigest, views),
	}}
	// The two conditional tabs. An empty "Unchecked" tab reads as a run that
	// decided everything, which is the one thing this feature must never say by
	// accident - so it appears when it has rows, and the summary carries the
	// zero otherwise.
	if sheet := complianceUncheckedSheet(productName, release, views); len(sheet.Rows) > 0 {
		book.Sheets = append(book.Sheets, sheet)
	}
	book.Sheets = append(book.Sheets,
		complianceChartsSheet(productName, release, chartViews),
		complianceRulebookSheet(views),
	)
	book.JSON = complianceReportJSON(productName, release, pkg, run, unique, chartViews, views, helm)
	return book
}

// ---------------------------------------------------------------------------
// Summary

// complianceSummarySheet is the page the workbook opens on.
//
// Grouped under the four questions somebody opens a compliance report with, in
// the order they ask them: what is this, what was it checked against, how much
// of it was actually checked, and what was found. A flat list of thirty fields
// in the order somebody wrote them makes a reader read all thirty to find one.
func complianceSummarySheet(
	productName, release string, pkg store.PackageRow, run store.ComplianceRunRow,
	unique store.ComplianceUniqueCounts, charts []ComplianceChartView, helm ComplianceHelmView,
) export.Sheet {
	rendered, failed, skipped := 0, 0, 0
	objects := 0
	for _, c := range charts {
		switch c.Status {
		case string(compliance.RenderOK):
			rendered++
			objects += c.Resources
		case string(compliance.RenderSkipped):
			skipped++
		default:
			failed++
		}
	}
	decided := run.Pass + run.Fail + run.Waived
	total := decided + run.Errors + run.Skip

	sheet := export.Sheet{
		Name:    sheetSummary,
		Title:   "Compliance report - " + productName + " " + release,
		Headers: []string{"Field", "Value"},
		Widths:  []int{34, 78},
		// The value column holds a sentence in two places - the tier and the
		// truncation notice - and a digest everywhere else.
		Wrap: []int{1},
	}
	add := func(field, value string) {
		sheet.Rows = append(sheet.Rows, []string{field, value})
	}
	// A blank field is a spacer row, and the heading above each block is
	// upper-case: the first column is bold throughout a field/value grid, so a
	// sentence-case heading would look like one more label. This is the shape
	// the security summary uses, for the same reason.
	group := func(heading string) {
		sheet.Rows = append(sheet.Rows, []string{"", ""}, []string{heading, ""})
	}

	add("Product", productName)
	add("Release", release)
	add("Release digest", pkg.ManifestDigest)
	add("Checked", formatExportTime(run.FinishedAt))
	add("Exported at", nowRFC3339())
	add("Run", run.ID)
	add("Verdict", compliance.Verdict(run.Verdict).Label())

	// Rule 5, in the one place a reader who disputes a finding will look. A
	// report that cannot say what produced it cannot be re-derived, and
	// re-deriving it is exactly what happens when a vendor disputes it.
	group("CHECKED AGAINST")
	add("Rulebook digest", run.BundleDigest)
	add("Checks in the rulebook", strconv.Itoa(run.Checks))
	add("helm", helmVersionOr(helm))
	add("Kubernetes API version", run.KubeVersion)
	add("Tier", "1 - what the vendor shipped, rendered at the chart's own default values")

	group("COVERAGE")
	add("Charts in the release", strconv.Itoa(len(charts)))
	add("Charts rendered", strconv.Itoa(rendered))
	add("Charts that did not render", strconv.Itoa(failed))
	add("Charts not fetched", strconv.Itoa(skipped))
	add("Kubernetes objects checked", strconv.Itoa(objects))
	add("Checks evaluated", strconv.Itoa(total))
	add("Checks decided", strconv.Itoa(decided))
	add("Checks not decided", strconv.Itoa(run.Errors))

	// Rules first, then places. "Five rules are broken" is what a vendor gets a
	// meeting about; "171 places" is what they work through afterwards, and one
	// number cannot say both.
	group("FOUND")
	add("Critical checks failed", strconv.Itoa(unique.Blocking))
	add("Critical findings", strconv.Itoa(run.Blocking))
	add("Warning checks failed", strconv.Itoa(unique.Warning))
	add("Warning findings", strconv.Itoa(run.Warning))
	add("Informational findings", strconv.Itoa(run.Info))
	add("Checks passed", strconv.Itoa(run.Pass))
	add("Findings waived", strconv.Itoa(run.Waived))

	if run.Truncated {
		group("INCOMPLETE")
		add("Results were cut short",
			"The run produced more results than it is allowed to store, so this file is short of them")
	}

	if run.Errors > 0 {
		// Said once, at the top, rather than on every row it qualifies. A
		// reader who quotes "5 critical" out of this file without it is
		// quoting a floor as a total.
		// Said once, at the top, rather than on every row it qualifies. A
		// reader who quotes "5 critical" out of this file without it is quoting
		// a floor as a total.
		sheet.Note = "This release has NOT been shown to meet the standards it was checked " +
			"against: " + strconv.Itoa(run.Errors) + " checks could not be decided, almost " +
			"always because a chart did not render. See the Charts sheet for which, and why."
	}
	return sheet
}

func helmVersionOr(h ComplianceHelmView) string {
	if h.Version != "" {
		return h.Version
	}
	if h.Reason != "" {
		return "not available: " + h.Reason
	}
	return ""
}

// ---------------------------------------------------------------------------
// Unique findings

// complianceUniqueSheet is one row per RULE that failed.
//
// The vendor conversation. A release breaks five rules in a hundred and
// seventy-one places, and this is the sheet somebody sends: what the rule is,
// why it exists, how widely it is broken, whose it is to fix, and what to do
// about it. The places are the next sheet along.
func complianceUniqueSheet(productName, release string, views []ComplianceResultView) export.Sheet {
	type group struct {
		view    ComplianceResultView
		places  int
		charts  map[string]struct{}
		kinds   map[string]struct{}
		owners  map[string]struct{}
		samples []string
	}
	byCheck := map[string]*group{}
	var order []string

	for _, v := range views {
		if v.Outcome != string(compliance.OutcomeFail) {
			continue
		}
		g := byCheck[v.Check]
		if g == nil {
			g = &group{
				view:   v,
				charts: map[string]struct{}{},
				kinds:  map[string]struct{}{},
				owners: map[string]struct{}{},
			}
			byCheck[v.Check] = g
			order = append(order, v.Check)
		}
		g.places++
		if v.Chart != "" {
			g.charts[v.Chart] = struct{}{}
		}
		if v.Kind != "" {
			g.kinds[v.Kind] = struct{}{}
		}
		if v.DeterminacyLabel != "" && v.Determinacy != string(compliance.DeterminacyNA) {
			g.owners[v.DeterminacyLabel] = struct{}{}
		}
		// Three examples, addressed. A reader deciding whether a rule matters
		// should not have to switch sheets to see one instance of it.
		if len(g.samples) < 3 {
			g.samples = append(g.samples, complianceWhere(v))
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		a, b := byCheck[order[i]], byCheck[order[j]]
		if ra, rb := severityOrder(a.view.Severity), severityOrder(b.view.Severity); ra != rb {
			return ra < rb
		}
		if a.places != b.places {
			return a.places > b.places
		}
		return a.view.Check < b.view.Check
	})

	rows := make([][]string, 0, len(order))
	for _, id := range order {
		g := byCheck[id]
		v := g.view
		rows = append(rows, []string{
			v.Check,
			v.Title,
			compliance.Severity(v.Severity).Label(),
			v.Category,
			v.Subcategory,
			strconv.Itoa(g.places),
			strconv.Itoa(len(g.charts)),
			joinSet(g.kinds),
			v.FixOwnerLabel,
			v.FixEffortLabel,
			whenItBites(v),
			joinSet(g.owners),
			strings.Join(g.samples, "\n"),
			v.Remediation,
			v.FixExample,
			v.Reference,
			v.Pack,
			tierLabel(v.Tier),
			productName,
			release,
		})
	}

	return export.Sheet{
		Name: sheetUniqueFindings,
		Headers: []string{
			"Check", "Title", "Severity", "Category", "Mechanism",
			"Places", "Charts", "Kinds",
			"Who fixes it", "Effort", "When it bites",
			"Value is", "Examples", "Remediation", "Example fix", "Reference", "Pack", "Tier",
			"Product", "Release",
		},
		Rows: rows,
		Widths: []int{12, 46, 11, 20, 26, 9, 9, 20,
			22, 30, 38, 24, 52, 60, 46, 34, 16, 10, 20, 20},
		// Title, Examples, Remediation, Example fix. The examples column holds
		// three addresses on three lines and rendered as the first of them.
		Wrap:    []int{1, 12, 13, 14},
		Primary: true,
	}
}

// ---------------------------------------------------------------------------
// All findings

// complianceFindingsSheet is one row per PLACE a rule was broken.
//
// The working sheet, and the reason every address column is here even where a
// reader of the screen could have derived it: this file is opened by somebody
// with no access to this platform, and "SEC-01, critical" without the chart,
// the template, the line and the field is a row nobody can act on.
func complianceFindingsSheet(
	productName, release, releaseDigest string, views []ComplianceResultView,
) export.Sheet {
	rows := make([][]string, 0, len(views))
	for _, v := range views {
		if v.Outcome != string(compliance.OutcomeFail) && v.Outcome != string(compliance.OutcomeWaived) {
			continue
		}
		rows = append(rows, complianceFindingRow(productName, release, releaseDigest, v))
	}
	return export.Sheet{
		Name:    sheetAllFindings,
		Headers: complianceFindingHeaders,
		Rows:    rows,
		Widths:  complianceFindingWidths,
		// Title, Search terms, Field, Finding, Remediation, Example fix - the
		// ones that are prose, YAML, a term list, or a path long enough to be
		// clipped.
		Wrap: []int{1, 10, 21, 24, 25, 26},
	}
}

// complianceUncheckedSheet is every check that could not be decided.
//
// Its own sheet, and not a filter on the one above, because it is a different
// conversation: nothing here is a defect in the release. It is the list of what
// this report does NOT cover, which is what makes the rest of it a floor rather
// than a total.
func complianceUncheckedSheet(productName, release string, views []ComplianceResultView) export.Sheet {
	rows := make([][]string, 0)
	for _, v := range views {
		if v.Outcome != string(compliance.OutcomeError) {
			continue
		}
		rows = append(rows, []string{
			v.Check,
			v.Title,
			compliance.Severity(v.Severity).Label(),
			v.Chart,
			v.ChartVersion,
			v.Error,
			productName,
			release,
		})
	}
	return export.Sheet{
		Name: sheetUnchecked,
		Note: "Nothing here is a defect in the release. These are checks the run could not " +
			"decide - almost always because a chart did not render - and they are why the " +
			"findings in this report are a floor rather than a total.",
		Headers: []string{
			"Check", "Title", "Severity", "Chart", "Chart version",
			"Why it could not be decided", "Product", "Release",
		},
		Rows:   rows,
		Widths: []int{12, 46, 11, 26, 14, 80, 20, 20},
		Wrap:   []int{1, 5},
	}
}

// The findings sheet's columns, in the order a reader uses them.
//
// # Why "Who fixes it" and "Value is" are two columns
//
// They were one, headed "Owner", and it held neither. It held the DETERMINACY -
// whether the chart fixes the value or a values file can override it - and on a
// real report a third of its rows read "Could not be established", which is not
// something anybody can route anywhere. The two questions are both worth
// asking and they are different questions: one is whose change it is, the other
// is whether the vendor or the site owns the value.
var complianceFindingHeaders = []string{
	"Check", "Title", "Severity", "Outcome",
	"Who fixes it", "Effort", "When it bites", "Confidence", "Value is",
	"Mechanism", "Search terms",
	"Chart", "Chart version", "Template file", "Line",
	"API version", "Kind", "Namespace", "Resource", "Container", "Container type",
	"Field", "Observed", "Expected", "Finding",
	"Remediation", "Example fix", "Reference", "Category", "Pack", "Tier",
	"Chart digest", "Chart reference", "Product", "Release", "Release digest",
	"Waiver", "Fingerprint",
}

var complianceFindingWidths = []int{
	12, 46, 11, 12,
	22, 30, 38, 34, 24,
	26, 52,
	24, 14, 40, 7,
	16, 18, 16, 26, 16, 14,
	40, 26, 26, 64,
	60, 46, 34, 20, 16, 10,
	26, 40, 20, 20, 26,
	16, 20,
}

func complianceFindingRow(
	productName, release, releaseDigest string, v ComplianceResultView,
) []string {
	line := ""
	if v.RenderedLine > 0 {
		line = strconv.Itoa(v.RenderedLine)
	}
	return []string{
		v.Check,
		v.Title,
		compliance.Severity(v.Severity).Label(),
		v.OutcomeLabel,

		v.FixOwnerLabel,
		v.FixEffortLabel,
		whenItBites(v),
		v.ConfidenceLabel,
		determinacyOrBlank(v),

		// The other half of the audience. Everything to the left of here is
		// written for somebody deciding whether to ship; these two are the
		// vocabulary the engineer who has to fix it would search for, and the
		// plain-language columns deliberately do not contain them.
		v.Subcategory,
		strings.Join(v.Keywords, ", "),

		v.Chart,
		v.ChartVersion,
		v.SourceFile,
		line,

		v.APIVersion,
		v.Kind,
		v.Namespace,
		v.Name,
		v.Container,
		v.ContainerType,

		v.Locus,
		v.Observed,
		v.Expected,
		firstNonEmpty(v.Message, v.Error),

		v.Remediation,
		v.FixExample,
		v.Reference,
		v.Category,
		v.Pack,
		tierLabel(v.Tier),

		v.ArtifactDigest,
		v.ArtifactRef,
		productName,
		release,
		releaseDigest,

		v.Waiver,
		v.Fingerprint,
	}
}

// ---------------------------------------------------------------------------
// Charts

// complianceChartsSheet is the run's denominator.
//
// Everything under it is a smaller number than it looks unless this sheet is
// read, so it carries not only WHETHER a chart rendered but why not, which
// values it wanted, and whether the failure was in a helm test hook - which
// `helm install` never applies, so a chart failing only there installs
// perfectly and still cannot be checked.
func complianceChartsSheet(productName, release string, charts []ComplianceChartView) export.Sheet {
	rows := make([][]string, 0, len(charts))
	// Failures first: everything below them is a smaller denominator than it
	// looks, and on ninety-five charts the thirteen that did not render are
	// otherwise thirteen rows behind eighty-two others.
	ordered := make([]ComplianceChartView, len(charts))
	copy(ordered, charts)
	sort.SliceStable(ordered, func(i, j int) bool {
		bad := func(c ComplianceChartView) int {
			if c.Status == string(compliance.RenderOK) {
				return 1
			}
			return 0
		}
		if bad(ordered[i]) != bad(ordered[j]) {
			return bad(ordered[i]) < bad(ordered[j])
		}
		return ordered[i].Name < ordered[j].Name
	})

	for _, c := range ordered {
		testHook := ""
		if c.ErrorInTest {
			testHook = "yes"
		}
		retried := ""
		if c.Error != "" {
			retried = "no"
			if c.Attempts > 1 {
				retried = "yes, " + strconv.Itoa(c.Attempts) + " attempts"
			}
		}
		rows = append(rows, []string{
			c.Name,
			c.Version,
			chartRenderedWord(c.Status),
			strconv.Itoa(c.Resources),
			c.ErrorLabel,
			c.ErrorValue,
			c.ErrorFile,
			testHook,
			retried,
			c.Error,
			c.Digest,
			c.Ref,
			productName,
			release,
		})
	}

	return export.Sheet{
		Name: sheetCharts,
		Note: "A chart that did not render contributed no objects, so every check needing " +
			"one of its objects is on the Unchecked sheet rather than passing.",
		Headers: []string{
			"Chart", "Version", "Rendered", "Objects",
			"Reason", "Value required", "Template", "In a helm test hook", "Retried",
			"Renderer message", "Chart digest", "Chart reference", "Product", "Release",
		},
		Rows:   rows,
		Widths: []int{26, 14, 11, 9, 26, 26, 40, 18, 18, 80, 26, 40, 20, 20},
		// The renderer's own message, which is a paragraph.
		Wrap: []int{6, 9},
	}
}

func chartRenderedWord(status string) string {
	switch status {
	case string(compliance.RenderOK):
		return "yes"
	case string(compliance.RenderSkipped):
		return "not fetched"
	default:
		return "no"
	}
}

// ---------------------------------------------------------------------------
// Rulebook

// complianceRulebookSheet is every check this run evaluated, and how it went.
//
// # Why the passes are a tally rather than rows
//
// Because a release produces three thousand passing results and nobody reads
// them one at a time - but "this rule was evaluated four hundred times and
// passed every one" is the sentence that turns an empty findings list into
// evidence. A sheet of three thousand rows saying "pass" would be the same fact
// in a form nobody opens.
func complianceRulebookSheet(views []ComplianceResultView) export.Sheet {
	type tally struct {
		view                              ComplianceResultView
		pass, fail, undecided, skip, waiv int
	}
	byCheck := map[string]*tally{}
	var order []string
	for _, v := range views {
		t := byCheck[v.Check]
		if t == nil {
			t = &tally{view: v}
			byCheck[v.Check] = t
			order = append(order, v.Check)
		}
		switch v.Outcome {
		case string(compliance.OutcomePass):
			t.pass++
		case string(compliance.OutcomeFail):
			t.fail++
		case string(compliance.OutcomeError):
			t.undecided++
		case string(compliance.OutcomeWaived):
			t.waiv++
		default:
			t.skip++
		}
	}
	sort.Strings(order)

	rows := make([][]string, 0, len(order))
	for _, id := range order {
		t := byCheck[id]
		rows = append(rows, []string{
			t.view.Check,
			t.view.Title,
			compliance.Severity(t.view.Severity).Label(),
			t.view.Category,
			t.view.Pack,
			tierLabel(t.view.Tier),
			strconv.Itoa(t.fail),
			strconv.Itoa(t.pass),
			strconv.Itoa(t.undecided),
			strconv.Itoa(t.waiv),
			strconv.Itoa(t.skip),
		})
	}
	return export.Sheet{
		Name: sheetRulebook,
		Note: "Every check this run evaluated, and how it went. A rule with no failures and " +
			"a large pass count is evidence; a rule with no results at all was not applicable " +
			"to anything this release ships.",
		Headers: []string{
			"Check", "Title", "Severity", "Category", "Pack", "Tier",
			"Failed", "Passed", "Not decided", "Waived", "Not applicable",
		},
		Rows:   rows,
		Widths: []int{12, 46, 11, 20, 16, 10, 10, 10, 13, 10, 15},
		Wrap:   []int{1},
	}
}

// ---------------------------------------------------------------------------
// JSON

// complianceReportJSON keeps the RELATIONSHIPS a grid throws away.
//
// A machine consuming this wants to know which finding belongs to which chart
// in which release; the flattened sheets have already answered that by
// repeating the address on every row, which is right for a spreadsheet and
// wasteful for a parser.
func complianceReportJSON(
	productName, release string, pkg store.PackageRow, run store.ComplianceRunRow,
	unique store.ComplianceUniqueCounts, charts []ComplianceChartView,
	views []ComplianceResultView, helm ComplianceHelmView,
) any {
	view := complianceRunView(run)
	view.Counts.UniqueBlocking = unique.Blocking
	view.Counts.UniqueWarning = unique.Warning
	view.Counts.UniqueInfo = unique.Info
	return map[string]any{
		"product":       productName,
		"release":       release,
		"releaseDigest": pkg.ManifestDigest,
		"run":           view,
		"helm":          helm,
		"charts":        charts,
		"results":       views,
	}
}

// ---------------------------------------------------------------------------
// Small shared pieces

// complianceWhere is one occurrence's address on one line, for the grouped
// sheet's examples column.
func complianceWhere(v ComplianceResultView) string {
	parts := make([]string, 0, 4)
	if v.Chart != "" {
		parts = append(parts, v.Chart)
	}
	if v.Kind != "" {
		object := v.Kind + " " + v.Name
		if v.Container != "" {
			object += " · container " + v.Container
		}
		parts = append(parts, object)
	}
	if v.Locus != "" {
		parts = append(parts, v.Locus)
	}
	return strings.Join(parts, " · ")
}

// whenItBites writes the timing as a sentence a reader can sort a plan by.
//
// It reads as a fragment on purpose - "on the next upgrade", "when a server is
// taken out for maintenance" - so the column says when the consequence arrives
// rather than naming a category the reader has to look up.
func whenItBites(v ComplianceResultView) string {
	if v.WhenItBitesLabel == "" {
		return ""
	}
	return "Bites " + v.WhenItBitesLabel
}

func determinacyOrBlank(v ComplianceResultView) string {
	if v.Determinacy == "" || v.Determinacy == string(compliance.DeterminacyNA) {
		return ""
	}
	return v.DeterminacyLabel
}

// tierLabel writes the tier as a number, and nothing for the zero value.
//
// "0" would read as a tier, and there is no tier 0.
func tierLabel(tier int) string {
	if tier <= 0 {
		return ""
	}
	return strconv.Itoa(tier)
}

// severityOrder ranks a severity worst-first, with an unknown value last so a
// value this build has never seen cannot promote itself above a critical.
func severityOrder(severity string) int {
	switch compliance.Severity(severity) {
	case compliance.SeverityBlock:
		return 0
	case compliance.SeverityWarn:
		return 1
	case compliance.SeverityInfo:
		return 2
	default:
		return 3
	}
}

func joinSet(set map[string]struct{}) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// formatExportTime writes a timestamp the way every other export does, and
// nothing at all for a run that has not finished.
func formatExportTime(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}
