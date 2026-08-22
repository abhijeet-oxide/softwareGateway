package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/export"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Exports.
//
// # Why an export is not "the JSON response, downloaded"
//
// Because the two answer different questions. The API response is a tree - a
// posture holding reports holding findings - and a spreadsheet is a grid. The
// projection between them is where the requirement's real demand lives: every
// row must carry the release, the artifact, the package and the CVE it belongs
// to, and for a comparison it must carry the classification too. A row that
// says "CVE-2024-3094, critical" without saying which image it is in is a row
// nobody can act on, and it is exactly what a naive flattening produces.
//
// JSON exports keep the tree, because a machine consuming an export wants the
// relationships that the grid throws away. Offering the flattened grid as the
// JSON would be shipping the loss as the feature.
//
// # Why filters are applied here and not by the client
//
// The requirement says an export respects the active filters, the search
// criteria and the release context. A client that filtered its own copy would
// export only the page it had loaded - the first fifty of 1,286 - and the file
// would look complete.

// handleExportPackageSecurity serves
// GET /products/{product}/packages/{package}/security/export.
func (s *Server) handleExportPackageSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.Security == nil {
		Error(w, r, v1.CodeUnavailable, "security scanning is not configured on this Coordinator")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	q := r.URL.Query()
	format, err := export.ParseFormat(q.Get("format"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	view := exportView(q.Get("view"))
	filter := parseFindingFilter(q)

	tracker := s.securityProgress.start(q.Get("progressToken"))
	defer s.securityProgress.finish(q.Get("progressToken"))

	ctx, cancel := context.WithTimeout(r.Context(), securityDeadline)
	defer cancel()

	// A detailed export needs findings; a summary export needs counts. Asking
	// for detail on a summary export would make the cheap file the expensive
	// one, on an endpoint people schedule.
	req, problem := s.securityRequestFor(ctx, productName, pkg, view == v1.ExportViewDetailed, false, tracker)
	if problem != "" {
		Error(w, r, v1.CodeUnavailable, problem)
		return
	}

	res, err := s.deps.Security.Posture(ctx, req)
	if err != nil {
		s.internal(w, r, "retrieve security posture for export", err)
		return
	}

	security.ReportStage(tracker, security.StageExporting, 0, 0)
	book := packageSecurityBook(productName, pkg, req.Scope, res, view, filter)
	s.writeExport(w, r, format, book, []string{productName, releaseLabel(pkg), "security", view})
}

// handleExportSecurityComparison serves
// GET /products/{product}/packages/{package}/security/compare/export.
//
// A GET rather than the POST the comparison itself uses, because a download is
// a link: a browser cannot follow a POST to a file, and an export a user cannot
// bookmark or re-fetch is an export they screenshot instead.
func (s *Server) handleExportSecurityComparison(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.Security == nil {
		Error(w, r, v1.CodeUnavailable, "security scanning is not configured on this Coordinator")
		return
	}

	q := r.URL.Query()
	against := strings.TrimSpace(q.Get("against"))
	if against == "" {
		Error(w, r, v1.CodeInvalidArgument,
			"a security comparison export needs a second release: set `against` to a tag or digest")
		return
	}
	format, err := export.ParseFormat(q.Get("format"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	view := exportView(q.Get("view"))
	filter := parseChangeFilter(q)

	base, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}
	other, ok := s.resolveSecondPackage(w, r, productName, v1.SecurityCompareRequest{
		Against: against, Repository: q.Get("repository"),
	})
	if !ok {
		return
	}

	tracker := s.securityProgress.start(q.Get("progressToken"))
	defer s.securityProgress.finish(q.Get("progressToken"))

	ctx, cancel := context.WithTimeout(r.Context(), securityDeadline)
	defer cancel()

	reqA, problem := s.securityRequestFor(ctx, productName, base, true, false, tracker)
	if problem != "" {
		Error(w, r, v1.CodeUnavailable, problem)
		return
	}
	reqB, problem := s.securityRequestFor(ctx, productName, other, true, false, tracker)
	if problem != "" {
		Error(w, r, v1.CodeUnavailable, problem)
		return
	}

	res, err := s.deps.Security.Compare(ctx, security.CompareRequest{
		A: reqA, B: reqB, NameA: releaseLabel(base), NameB: releaseLabel(other),
	})
	if err != nil {
		s.internal(w, r, "compare security for export", err)
		return
	}

	security.ReportStage(tracker, security.StageExporting, 0, 0)
	book := comparisonBook(productName, base, other, reqA.Scope, reqB.Scope, res, view, filter)
	s.writeExport(w, r, format, book,
		[]string{productName, releaseLabel(base), "vs", releaseLabel(other), "security", view})
}

// handleExportSecuritySearch serves
// GET /products/{product}/security/search/export.
func (s *Server) handleExportSecuritySearch(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.SecurityIndex == nil {
		Error(w, r, v1.CodeUnavailable, "security search is not configured on this Coordinator")
		return
	}

	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		Error(w, r, v1.CodeInvalidArgument, "q is required")
		return
	}
	format, err := export.ParseFormat(q.Get("format"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	kind, err := parseSearchKind(q.Get("kind"), query)
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	// An export takes the whole result rather than the page the interface is
	// showing. A file that silently held the first fifty of 1,286 rows would
	// look complete and be wrong, which is the worst combination an export can
	// manage.
	hits, err := s.deps.SecurityIndex.Search(r.Context(), store.SearchFilter{
		Product: productName, Kind: kind, Query: query,
		Exact: q.Get("exact") == "true", Limit: 1000,
	})
	if err != nil {
		s.internal(w, r, "security search for export", err)
		return
	}

	releases := map[string][]store.ReleaseRef{}
	if len(hits) > 0 {
		refs := make([]string, 0, len(hits))
		seen := map[string]bool{}
		for _, h := range hits {
			if !seen[h.ArtifactRef] {
				seen[h.ArtifactRef] = true
				refs = append(refs, h.ArtifactRef)
			}
		}
		if found, err := s.deps.SecurityIndex.ReleasesFor(r.Context(), productName, refs); err == nil {
			releases = found
		}
	}

	payload := toAPISecuritySearch(productName, string(kind), query, q.Get("exact") == "true",
		hits, releases, len(hits) >= 1000)
	s.writeExport(w, r, format, searchBook(payload), []string{productName, "search", string(kind), query})
}

// writeExport streams a book and names the file.
func (s *Server) writeExport(
	w http.ResponseWriter, r *http.Request, format export.Format, book export.Book, nameParts []string,
) {
	filename := export.Filename(nameParts, format, time.Now())

	w.Header().Set("Content-Type", format.ContentType())
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// An export is a point-in-time extract and must never be served from a
	// cache: the whole reason somebody re-runs one is that the answer changed.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	if err := export.Write(w, format, book); err != nil {
		// The status line is already sent, so there is no error response to
		// give: the download simply truncates. Logged so the truncation has an
		// explanation somewhere.
		s.deps.Logger.Error("security export failed mid-stream",
			"error", err, "path", r.URL.Path, "format", format)
	}
}

// ---------------------------------------------------------------------------
// Books
// ---------------------------------------------------------------------------

func exportView(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), v1.ExportViewSummary) {
		return v1.ExportViewSummary
	}
	return v1.ExportViewDetailed
}

// findingFilter is the active filter, applied server-side.
type findingFilter struct {
	severities map[security.Severity]bool
	fixable    *bool
	statuses   map[security.Status]bool
	query      string
}

func parseFindingFilter(q map[string][]string) findingFilter {
	f := findingFilter{}
	if raw := firstValue(q, "severity"); raw != "" {
		f.severities = map[security.Severity]bool{}
		for _, part := range strings.Split(raw, ",") {
			f.severities[security.ParseSeverity(part)] = true
		}
	}
	switch strings.ToLower(firstValue(q, "fixable")) {
	case "true":
		v := true
		f.fixable = &v
	case "false":
		v := false
		f.fixable = &v
	}
	if raw := firstValue(q, "status"); raw != "" {
		f.statuses = map[security.Status]bool{}
		for _, part := range strings.Split(raw, ",") {
			f.statuses[security.Status(strings.TrimSpace(part))] = true
		}
	}
	f.query = strings.ToLower(strings.TrimSpace(firstValue(q, "q")))
	return f
}

func (f findingFilter) keepReport(r security.Report) bool {
	return len(f.statuses) == 0 || f.statuses[r.Status]
}

func (f findingFilter) keepFinding(fd security.Finding) bool {
	if len(f.severities) > 0 && !f.severities[fd.Severity] {
		return false
	}
	if f.fixable != nil && fd.Fixable != *f.fixable {
		return false
	}
	if f.query != "" {
		haystack := strings.ToLower(fd.CVE + " " + fd.ID + " " + fd.Component.Name + " " + fd.Summary)
		if !strings.Contains(haystack, f.query) {
			return false
		}
	}
	return true
}

// changeFilter is the comparison view's filter.
type changeFilter struct {
	findingFilter
	types map[security.ChangeType]bool
}

func parseChangeFilter(q map[string][]string) changeFilter {
	f := changeFilter{findingFilter: parseFindingFilter(q)}
	if raw := firstValue(q, "change"); raw != "" {
		f.types = map[security.ChangeType]bool{}
		for _, part := range strings.Split(raw, ",") {
			f.types[security.ChangeType(strings.TrimSpace(part))] = true
		}
	}
	return f
}

func (f changeFilter) keepChange(c security.Change) bool {
	if len(f.types) > 0 && !f.types[c.Type] {
		return false
	}
	if len(f.severities) > 0 && !f.severities[c.Severity] {
		return false
	}
	if f.fixable != nil && c.Fixable != *f.fixable {
		return false
	}
	if f.query != "" {
		haystack := strings.ToLower(c.CVE + " " + c.ID + " " + c.Component.Name + " " +
			c.Summary + " " + c.Artifact.ArtifactKey())
		if !strings.Contains(haystack, f.query) {
			return false
		}
	}
	return true
}

func firstValue(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// packageSecurityBook projects a release's posture into sheets.
func packageSecurityBook(
	productName string, pkg store.PackageRow, scope security.Scope,
	res security.Result, view string, filter findingFilter,
) export.Book {
	state, message := securityState(res)
	release := releaseLabel(pkg)

	summary := export.Sheet{
		Name:    "Summary",
		Headers: []string{"Field", "Value"},
		Rows: [][]string{
			{"Product", productName},
			{"Release", release},
			{"Release digest", pkg.ManifestDigest},
			{"Repository", scope.Repository},
			{"Scanner", res.Provider},
			{"Scanner enabled", strconv.FormatBool(res.Enabled)},
			{"Data state", state},
			{"Data note", message},
			{"Artifacts", strconv.Itoa(res.Posture.Coverage.Artifacts)},
			{"Artifacts scanned", strconv.Itoa(res.Posture.Coverage.Scanned)},
			{"Artifacts not scanned", strconv.Itoa(res.Posture.Coverage.NotScanned)},
			{"Artifacts unavailable", strconv.Itoa(res.Posture.Coverage.Unavailable)},
			{"Artifacts not applicable", strconv.Itoa(res.Posture.Coverage.Unsupported)},
			{"Coverage complete", strconv.FormatBool(res.Posture.Coverage.Complete())},
			{"Vulnerabilities", strconv.Itoa(res.Posture.Counts.Total)},
			{"Fixable", strconv.Itoa(res.Posture.Counts.Fixable)},
			{"Non-fixable", strconv.Itoa(res.Posture.Counts.NonFixable)},
			{"Critical", strconv.Itoa(res.Posture.Counts.BySeverity.Critical)},
			{"High", strconv.Itoa(res.Posture.Counts.BySeverity.High)},
			{"Medium", strconv.Itoa(res.Posture.Counts.BySeverity.Medium)},
			{"Low", strconv.Itoa(res.Posture.Counts.BySeverity.Low)},
			{"Unknown severity", strconv.Itoa(res.Posture.Counts.BySeverity.Unknown)},
			{"Distinct vulnerabilities", strconv.Itoa(res.Posture.UniqueCounts.Total)},
			{"Exported at", time.Now().UTC().Format(time.RFC3339)},
		},
	}

	book := export.Book{Sheets: []export.Sheet{summary}}
	if view == v1.ExportViewSummary {
		book.JSON = summaryJSON(productName, pkg, scope, res)
		return book
	}

	// Every row carries its whole address - release, artifact, package, CVE -
	// because a spreadsheet row is read on its own, out of order, filtered, and
	// pasted into a ticket. A row that relied on the sheet's context for its
	// meaning loses that meaning the moment somebody sorts the sheet.
	detail := export.Sheet{
		Name: "Findings",
		Headers: []string{
			"Product", "Release", "Release digest", "Artifact", "Artifact tag", "Artifact digest",
			"Artifact kind", "Scan status", "CVE", "Issue ID", "Severity", "Fixable", "Fixed in",
			"Package", "Package version", "Package type", "CVSS", "Published", "Scanner", "Summary",
		},
	}
	for _, report := range res.Posture.Reports {
		if !filter.keepReport(report) {
			continue
		}
		// An artifact with no findings still gets a row when it was NOT
		// scanned. An export whose absent rows meant both "clean" and "nobody
		// looked" would be the whole failure of this feature, in a file.
		if len(report.Findings) == 0 {
			if report.Status == security.StatusScanned {
				continue
			}
			detail.Rows = append(detail.Rows, []string{
				productName, release, pkg.ManifestDigest,
				report.Artifact.ArtifactKey(), report.Artifact.Tag, report.Artifact.Digest,
				report.Artifact.Kind, string(report.Status),
				"", "", "", "", "", "", "", "", "", "", report.Provider, report.Message,
			})
			continue
		}
		for _, f := range report.Findings {
			if !filter.keepFinding(f) {
				continue
			}
			detail.Rows = append(detail.Rows, []string{
				productName, release, pkg.ManifestDigest,
				report.Artifact.ArtifactKey(), report.Artifact.Tag, report.Artifact.Digest,
				report.Artifact.Kind, string(report.Status),
				f.CVE, f.ID, string(f.Severity), strconv.FormatBool(f.Fixable),
				strings.Join(f.FixedIn, " "),
				f.Component.Name, f.Component.Version, f.Component.Type,
				formatScore(f.CVSSScore), formatPublished(f.Published), f.Provider, f.Summary,
			})
		}
	}

	book.Sheets = append(book.Sheets, detail)
	book.JSON = detailJSON(productName, pkg, scope, res, filter)
	return book
}

// comparisonBook projects a comparison into sheets.
func comparisonBook(
	productName string, base, other store.PackageRow,
	scopeA, scopeB security.Scope, res security.CompareResult, view string, filter changeFilter,
) export.Book {
	c := res.Comparison

	summary := export.Sheet{
		Name:    "Summary",
		Headers: []string{"Field", "Value"},
		Rows: [][]string{
			{"Product", productName},
			{"Base release", releaseLabel(base)},
			{"New release", releaseLabel(other)},
			{"Verdict", string(c.Verdict)},
			{"Headline", c.Headline},
			{"Explanation", c.Explanation},
			{"Caveats", strings.Join(c.Caveats, " ")},
			{"Resolved", strconv.Itoa(c.Resolved.Total)},
			{"Resolved critical", strconv.Itoa(c.Resolved.BySeverity.Critical)},
			{"Resolved high", strconv.Itoa(c.Resolved.BySeverity.High)},
			{"Resolved medium", strconv.Itoa(c.Resolved.BySeverity.Medium)},
			{"Resolved low", strconv.Itoa(c.Resolved.BySeverity.Low)},
			{"Introduced", strconv.Itoa(c.Introduced.Total)},
			{"Introduced critical", strconv.Itoa(c.Introduced.BySeverity.Critical)},
			{"Introduced high", strconv.Itoa(c.Introduced.BySeverity.High)},
			{"Introduced medium", strconv.Itoa(c.Introduced.BySeverity.Medium)},
			{"Introduced low", strconv.Itoa(c.Introduced.BySeverity.Low)},
			{"Unchanged", strconv.Itoa(c.Unchanged.Total)},
			{"Became more severe", strconv.Itoa(c.SeverityIncreased.Total)},
			{"Became less severe", strconv.Itoa(c.SeverityDecreased.Total)},
			{"On removed artifacts", strconv.Itoa(c.RemovedArtifact.Total)},
			{"Artifacts common", strconv.Itoa(c.ArtifactSummary.Common)},
			{"Artifacts upgraded", strconv.Itoa(c.ArtifactSummary.Upgraded)},
			{"Artifacts added", strconv.Itoa(c.ArtifactSummary.Added)},
			{"Artifacts removed", strconv.Itoa(c.ArtifactSummary.Removed)},
			{"Artifacts not comparable", strconv.Itoa(c.ArtifactSummary.NotComparable)},
			{"Exported at", time.Now().UTC().Format(time.RFC3339)},
		},
	}

	book := export.Book{Sheets: []export.Sheet{summary}}
	if view == v1.ExportViewSummary {
		book.JSON = toAPISecurityComparison(productName, base, other, scopeA, scopeB, summaryOnly(res))
		return book
	}

	detail := export.Sheet{
		Name: "Changes",
		Headers: []string{
			"Product", "Base release", "New release", "Change", "Artifact", "Artifact change",
			"Artifact tag", "Artifact digest", "CVE", "Issue ID", "Severity", "From severity",
			"To severity", "Fixable", "Fixed in", "Package", "Package version", "Package type",
			"Resolved by removal", "Scanner", "Summary",
		},
	}
	for _, ch := range c.Changes {
		if !filter.keepChange(ch) {
			continue
		}
		detail.Rows = append(detail.Rows, []string{
			productName, releaseLabel(base), releaseLabel(other),
			string(ch.Type), ch.Artifact.ArtifactKey(), string(ch.ArtifactChange),
			ch.Artifact.Tag, ch.Artifact.Digest,
			ch.CVE, ch.ID, string(ch.Severity), string(ch.FromSeverity), string(ch.ToSeverity),
			strconv.FormatBool(ch.Fixable), strings.Join(ch.FixedIn, " "),
			ch.Component.Name, ch.Component.Version, ch.Component.Type,
			strconv.FormatBool(ch.ViaRemoval), ch.Provider, ch.Summary,
		})
	}

	artifacts := export.Sheet{
		Name: "Artifacts",
		Headers: []string{
			"Artifact", "Change", "Base digest", "New digest", "Base status", "New status",
			"Comparable", "Base vulnerabilities", "New vulnerabilities",
			"Introduced", "Resolved", "Unchanged", "Severity changed",
		},
	}
	for _, d := range c.Artifacts {
		artifacts.Rows = append(artifacts.Rows, []string{
			d.Key, string(d.Change), digestOf(d.A), digestOf(d.B),
			string(d.StatusA), string(d.StatusB), strconv.FormatBool(d.Comparable),
			strconv.Itoa(d.CountsA.Total), strconv.Itoa(d.CountsB.Total),
			strconv.Itoa(d.Introduced), strconv.Itoa(d.Resolved),
			strconv.Itoa(d.Unchanged), strconv.Itoa(d.SeverityChanged),
		})
	}

	// Changes last, so the CSV encoding - which writes the last sheet - hands
	// back the rows somebody exporting a comparison to CSV came for.
	book.Sheets = append(book.Sheets, artifacts, detail)
	book.JSON = toAPISecurityComparison(productName, base, other, scopeA, scopeB, res)
	return book
}

// searchBook projects search results into one sheet.
func searchBook(payload v1.SecuritySearchResponse) export.Book {
	sheet := export.Sheet{
		Name: "Search results",
		Headers: []string{
			"CVE", "Issue ID", "Severity", "Fixable", "Fixed in", "Package", "Package version",
			"Package type", "Image", "Image tag", "Image digest", "Repository", "Releases",
			"Scanner", "Scanned at", "Summary",
		},
	}
	for _, h := range payload.Hits {
		releases := make([]string, 0, len(h.Releases))
		for _, rel := range h.Releases {
			label := rel.DisplayTag
			if label == "" {
				label = rel.Tag
			}
			releases = append(releases, label)
		}
		sheet.Rows = append(sheet.Rows, []string{
			h.CVE, h.IssueID, h.Severity, strconv.FormatBool(h.Fixable), h.FixedIn,
			h.Component.Name, h.Component.Version, h.Component.Type,
			h.Artifact.Name, h.Artifact.Tag, h.Artifact.Digest, h.Repository,
			strings.Join(releases, " "), h.Provider, h.ScannedAt, h.Summary,
		})
	}
	return export.Book{Sheets: []export.Sheet{sheet}, JSON: payload}
}

// summaryOnly strips the change rows from a comparison for a summary export.
func summaryOnly(res security.CompareResult) security.CompareResult {
	res.Comparison.Changes = nil
	res.Comparison.Artifacts = nil
	return res
}

func summaryJSON(productName string, pkg store.PackageRow, scope security.Scope, res security.Result) any {
	out := toAPIPackageSecurity(productName, pkg, scope, res, false)
	for i := range out.Reports {
		out.Reports[i].Findings = nil
	}
	return out
}

func detailJSON(
	productName string, pkg store.PackageRow, scope security.Scope,
	res security.Result, filter findingFilter,
) any {
	filtered := res
	filtered.Posture.Reports = nil
	for _, report := range res.Posture.Reports {
		if !filter.keepReport(report) {
			continue
		}
		kept := report
		kept.Findings = nil
		for _, f := range report.Findings {
			if filter.keepFinding(f) {
				kept.Findings = append(kept.Findings, f)
			}
		}
		filtered.Posture.Reports = append(filtered.Posture.Reports, kept)
	}
	return toAPIPackageSecurity(productName, pkg, scope, filtered, true)
}

func digestOf(a *security.ArtifactRef) string {
	if a == nil {
		return ""
	}
	return a.Digest
}

func formatScore(score float64) string {
	if score <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1f", score)
}

func formatPublished(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}
