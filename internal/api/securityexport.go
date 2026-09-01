package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

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
	if s.deps.Packages == nil || s.deps.SecurityStore == nil {
		Error(w, r, v1.CodeUnavailable, "security storage is not configured on this Coordinator")
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
	// Which table a single-table format writes. See markPrimary: a workbook
	// carries all of them and a CSV carries one, and which one is a question
	// only the reader can answer.
	table := strings.ToLower(strings.TrimSpace(q.Get("table")))

	// Read, not retrieve. An export of a release nobody synced is an export of
	// nothing, and it says so in the file rather than quietly starting a
	// multi-minute scan behind a download link.
	side, err := s.securitySide(r.Context(), productName, pkg, security.WithProse)
	if err != nil {
		s.internal(w, r, "read security for export", err)
		return
	}

	book := packageSecurityBook(productName, pkg, side, view, filter, s.deps.SecurityFreshness)
	markPrimary(&book, table)
	if format == export.FormatZIP {
		book.Files = s.bundleFor(r.Context(), productName, pkg, side)
	}
	s.writeExport(w, r, format, book, []string{productName, releaseLabel(pkg), "security", view})
}

// markPrimary moves the single-table formats' choice of sheet.
//
// # Why the caller gets to choose at all
//
// Because a workbook holds every table and a CSV holds one, and which one
// depends entirely on what the reader is about to do. Somebody pivoting on
// packages wants All findings; somebody handing a list of advisories to a
// vendor wants Unique CVEs; somebody chasing coverage wants Images. Guessing
// produced the complaint this parameter answers - the file came back with a
// table that was not the one on screen.
//
// An unrecognised name leaves the book's own default in place rather than
// erroring: the download is already a link somebody clicked, and a 400 in a new
// tab is a worse answer than the sensible table.
func markPrimary(book *export.Book, table string) {
	if table == "" {
		return
	}
	want := map[string]string{
		"unique":    "Unique CVEs",
		"cves":      "Unique CVEs",
		"findings":  "All findings",
		"all":       "All findings",
		"images":    "Images",
		"artifacts": "Images",
		"malware":   "Malware",
		"policy":    "Policy violations",
		"problems":  "Problems",
		"sources":   "By source",
		"summary":   "Summary",
	}[table]
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

// bundleFor assembles the scanner's own bodies for a ZIP export.
//
// # Why this reads storage and never a scanner
//
// Because a download link that starts a fifteen-minute retrieval is a download
// link that times out somewhere between here and the browser, and the user's
// only evidence is a truncated file. The bodies were captured by the sync that
// was going to make those requests anyway (see security.ScanOptions.Sink), so
// this is a table read.
//
// A release whose documents have been evicted, or which was synced before this
// existed, gets a bundle of tables and a README that says which directories are
// empty and why. That is a worse bundle and an honest one; silently shipping a
// vulnerabilities/ directory with four of a hundred and fifty-seven files in it
// is neither.
func (s *Server) bundleFor(
	ctx context.Context, productName string, pkg store.PackageRow, side securitySide,
) []export.File {
	if len(side.reports) == 0 {
		return nil
	}
	refs := make([]security.ArtifactRef, 0, len(side.reports))
	for _, r := range side.reports {
		refs = append(refs, r.Artifact)
	}

	docs, err := s.deps.SecurityStore.LoadDocuments(ctx, side.target.Scope, refs, nil)
	if err != nil {
		// The tables are the export's substance and they are already built.
		// Failing the whole download because the raw half could not be read
		// would hand back nothing where something useful was in hand.
		s.deps.Logger.Warn("security bundle: could not read stored documents",
			"product", productName, "package", pkg.ID, "error", err)
		docs = map[string]map[security.DocumentKind]security.Document{}
	}

	files := bundleFiles(docs, side.reports)
	return append([]export.File{bundleReadme(productName, pkg, side, docs)}, files...)
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
	if s.deps.Packages == nil || s.deps.SecurityStore == nil {
		Error(w, r, v1.CodeUnavailable, "security storage is not configured on this Coordinator")
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

	// WithProse, unlike the page: an export is a document somebody reads away
	// from this tool, and the paragraph is most of why they exported it. Both
	// sides at once, for the reason the comparison handler gives.
	var sideA, sideB securitySide
	group, gctx := errgroup.WithContext(r.Context())
	group.Go(func() error {
		var err error
		sideA, err = s.securitySide(gctx, productName, base, security.WithProse)
		return err
	})
	group.Go(func() error {
		var err error
		sideB, err = s.securitySide(gctx, productName, other, security.WithProse)
		return err
	})
	if err := group.Wait(); err != nil {
		s.internal(w, r, "read security for export", err)
		return
	}

	cmp := security.Compare(security.CompareInput{
		A: sideA.reports, B: sideB.reports,
		NameA: releaseLabel(base), NameB: releaseLabel(other),
	})

	book := comparisonBook(productName, base, other, sideA, sideB, cmp, view, filter)
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

// describe renders the active filter as a sentence, or "" when nothing is
// filtered.
//
// # Why an export has to say this on its own first page
//
// Because a filtered export is indistinguishable from a complete one once it
// has been emailed. A file holding 402 of 90,808 findings, with nothing in it
// naming the filter, is the file somebody forwards as "the vulnerabilities in
// this release" - and every number a reader takes from it is wrong in the one
// direction that matters.
func (f findingFilter) describe() string {
	var parts []string
	if len(f.severities) > 0 {
		names := make([]string, 0, len(f.severities))
		for sev := range f.severities {
			names = append(names, string(sev))
		}
		sort.Strings(names)
		parts = append(parts, "severity "+strings.Join(names, ", "))
	}
	if f.fixable != nil {
		if *f.fixable {
			parts = append(parts, "fixable only")
		} else {
			parts = append(parts, "not-fixable only")
		}
	}
	if len(f.statuses) > 0 {
		names := make([]string, 0, len(f.statuses))
		for st := range f.statuses {
			names = append(names, string(st))
		}
		sort.Strings(names)
		parts = append(parts, "scan status "+strings.Join(names, ", "))
	}
	if f.query != "" {
		parts = append(parts, "search "+strconv.Quote(f.query))
	}
	return strings.Join(parts, "; ")
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

// packageSecurityBook projects a release's posture into the tables the
// interface shows.
//
// See securitysheets.go for what those are and why. The short version: an
// export of a release's security is the DATA, and the twenty-six-row field/value
// summary that used to be the first tab of every workbook - and the entire
// content of every summary export - is not it.
func packageSecurityBook(
	productName string, pkg store.PackageRow, side securitySide,
	view string, filter findingFilter, fresh security.Freshness,
) export.Book {
	release := releaseLabel(pkg)

	if view == v1.ExportViewSummary {
		// The summary view is the same page on its own, for the person pasting
		// one number into a release note.
		book := export.Book{Sheets: []export.Sheet{summarySheet(productName, pkg, side, filter)}}
		book.Sheets[0].Primary = true
		book.JSON = toAPIPackageSecurity(productName, pkg, side.row, side.target, false, fresh)
		return book
	}

	// The summary FIRST, and as a page rather than a grid.
	//
	// It was removed from the detailed export because the old one was
	// twenty-six field/value rows restating the cards - and removing it went
	// one step too far. A workbook that opens on row one of ninety thousand
	// findings gives a reader nowhere to stand: what release is this, how much
	// of it was scanned, how bad is it, when was it measured. That page is
	// worth one tab; it is not worth being the whole export.
	book := export.Book{Sheets: []export.Sheet{
		summarySheet(productName, pkg, side, filter),
		uniqueCVESheet(productName, release, side.reports, filter),
		findingsSheet(productName, release, pkg.ManifestDigest, side.reports, filter),
		imagesSheet(productName, release, side.reports),
	}}
	// The three conditional sheets. A workbook with an empty "Malware" tab
	// reads as a scanner that was asked and found none, which is a claim this
	// platform can only make where malware was actually retrieved - so the tab
	// appears when there is something in it, and the Images sheet's count
	// column carries the zero otherwise.
	if sheet := malwareSheet(productName, release, side.reports); len(sheet.Rows) > 0 {
		book.Sheets = append(book.Sheets, sheet)
	}
	if sheet := policySheet(productName, release, side.reports); len(sheet.Rows) > 0 {
		book.Sheets = append(book.Sheets, sheet)
	}
	if sheet, ok := sourcesSheet(productName, release, side.posture); ok {
		book.Sheets = append(book.Sheets, sheet)
	}
	if sheet := problemsSheet(productName, release, side.reports); len(sheet.Rows) > 0 {
		book.Sheets = append(book.Sheets, sheet)
	}

	book.JSON = detailJSON(productName, pkg, side, filter, fresh)
	return book
}

// summarySheet is the page a workbook opens on: what this is, how much of it
// was looked at, and how bad it is.
//
// # Why it is grouped rather than a flat list of fields
//
// Because the twenty-six rows it used to be were in the order somebody wrote
// them, and a reader looking for "how many images were scanned" had to read all
// of them to find out. Four headings - what this is, what was scanned, what was
// found, what is held - are the four questions somebody opens a security export
// with, in the order they ask them.
func summarySheet(
	productName string, pkg store.PackageRow, side securitySide, filter findingFilter,
) export.Sheet {
	state, message := securityState(side.row, side.target)
	row := side.row
	cov := row.Coverage

	// The malware count is the one number here that is not in the stored row -
	// it lives on the per-image reports - and it is the one that most needs to
	// be on the first page a reader sees.
	malware, violations, images := 0, 0, 0
	for _, r := range side.reports {
		malware += len(r.Malware)
		violations += len(r.Violations)
		if len(r.Malware) > 0 {
			images++
		}
	}

	sheet := export.Sheet{
		Name:    "Summary",
		Title:   productName + " - " + releaseLabel(pkg) + " - security summary",
		Headers: []string{"Field", "Value"},
		Widths:  []int{38, 78},
	}
	add := func(field, value string) {
		sheet.Rows = append(sheet.Rows, []string{field, value})
	}
	// A blank field is a spacer row. Cheap, and it is what turns a wall of
	// twenty-six rows into four things a reader can find at a glance.
	group := func(heading string) {
		sheet.Rows = append(sheet.Rows, []string{"", ""}, []string{heading, ""})
	}

	add("Product", productName)
	add("Release", releaseLabel(pkg))
	add("Release digest", pkg.ManifestDigest)
	add("Repository", pkg.SourceRepository)
	add("Exported at", nowRFC3339())

	group("SCAN")
	add("Scanner", providerLabel(providerOr(row.Provider, side.target)))
	add("Scanner repository", repositoryOr(row.Repository, side.target))
	add("Last synced", formatTimePtr(row.SyncedAt))
	add("Oldest scan result", formatTimePtr(row.ScannedAt))
	add("Sync state", string(orNever(row.State)))
	add("Result", state)
	if message != "" {
		add("Note", message)
	}

	group("COVERAGE")
	add("Artifacts in release", strconv.Itoa(cov.Artifacts))
	add("Images scanned", fmt.Sprintf("%d of %d", cov.Scanned, cov.Scannable()))
	add("Images not indexed yet", strconv.Itoa(cov.NotScanned))
	add("Images not in the scanner's repository", strconv.Itoa(cov.Missing))
	add("Images the scanner would not answer for", strconv.Itoa(cov.Unavailable))
	add("Artifacts the scanner does not cover", strconv.Itoa(cov.Unsupported))
	add("Coverage complete", yesNo(cov.Complete()))

	group("FINDINGS")
	add("Vulnerabilities", strconv.Itoa(row.Counts.Total))
	add("Distinct advisories", strconv.Itoa(row.DistinctCVEs))
	add("Distinct advisory and package pairs", strconv.Itoa(row.DistinctTotal))
	add("Fixable", strconv.Itoa(row.Counts.Fixable))
	add("Not fixable", strconv.Itoa(row.Counts.NonFixable))
	add("Critical", strconv.Itoa(row.Counts.BySeverity.Critical))
	add("High", strconv.Itoa(row.Counts.BySeverity.High))
	add("Medium", strconv.Itoa(row.Counts.BySeverity.Medium))
	add("Low", strconv.Itoa(row.Counts.BySeverity.Low))
	add("Unknown severity", strconv.Itoa(row.Counts.BySeverity.Unknown))
	add("Malicious packages", strconv.Itoa(malware))
	if malware > 0 {
		add("Images carrying malware", strconv.Itoa(images))
	}
	add("Policy violations", strconv.Itoa(violations))

	if len(side.posture.BySource) > 1 {
		group("BY SCANNER")
		for _, src := range side.posture.BySource {
			add(providerLabel(src.Provider), fmt.Sprintf(
				"%d findings, %d distinct advisories, %d reported by no other scanner",
				src.Counts.Total, src.UniqueCVEs, src.OnlyHere))
		}
	}

	if described := filter.describe(); described != "" {
		// An export taken through a filter has to say so on its first page.
		// A file holding 402 of 90,808 findings, with nothing on it to say
		// which 402, is the export that gets forwarded as if it were the lot.
		group("FILTER")
		add("Rows in this export were filtered by", described)
	}

	group("SHEETS")
	add("Unique CVEs", "One row per advisory, with every image and package it appears in")
	add("All findings", "One row per image, advisory and package")
	add("Images", "One row per image, with its counts and its scan status")
	if malware > 0 {
		add("Malware", "Malicious packages - these have no version to upgrade to")
	}
	if violations > 0 {
		add("Policy violations", "What the scanner's configured watches raised")
	}
	return sheet
}

func yesNo(v bool) string {
	if v {
		return "Yes"
	}
	return "No"
}

// comparisonBook projects a comparison into sheets.
func comparisonBook(
	productName string, base, other store.PackageRow,
	sideA, sideB securitySide, c security.Comparison, view string, filter changeFilter,
) export.Book {

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
		book.JSON = toAPISecurityComparison(productName, base, other, sideA, sideB, summaryOnly(c))
		return book
	}

	detail := export.Sheet{
		Name:    "Changes",
		Primary: true,
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
	book.JSON = toAPISecurityComparison(productName, base, other, sideA, sideB, c)
	return book
}

// searchBook projects search results into one sheet.
func searchBook(payload v1.SecuritySearchResponse) export.Book {
	sheet := export.Sheet{
		Name:    "Search results",
		Primary: true,
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
func summaryOnly(c security.Comparison) security.Comparison {
	c.Changes = nil
	c.Artifacts = nil
	return c
}

// detailJSON is the tree, filtered the same way the grid was.
//
// A JSON export exists so a machine can consume the RELATIONSHIPS - which
// finding belongs to which artifact in which release - and the flattened grid
// has already thrown those away. Offering the grid as the JSON would be
// offering the loss as the feature.
func detailJSON(
	productName string, pkg store.PackageRow, side securitySide, filter findingFilter,
	fresh security.Freshness,
) any {
	out := toAPIPackageSecurity(productName, pkg, side.row, side.target, true, fresh)
	for _, report := range side.reports {
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
		out.Reports = append(out.Reports, toAPIReport(kept))
	}
	return out
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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
