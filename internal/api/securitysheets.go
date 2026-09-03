package api

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/export"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The tables an export contains, and why they are the tables that are on screen.
//
// # What this replaced
//
// One "Summary" sheet of field/value pairs, and one flat "Findings" sheet. The
// summary was the first tab of every workbook and the whole content of every
// summary-view export - twenty-six rows saying what the cards on the page
// already say, in a file somebody opened to get the DATA. Nobody exports a
// spreadsheet to read a headline.
//
// So a detailed export is now the tables themselves, in the same shape the
// interface shows them, because that shape is what a reader has already learned
// to read:
//
//	Unique CVEs        one row per advisory, with where it turns up
//	All findings       one row per (image, advisory, package)
//	Images             one row per image, with its counts and its status
//	Malware            what must not ship
//	Policy violations  what the gate says
//	Problems           what could not be scanned, and why
//
// The summary survives as a sheet only in the summary view, which exists for
// the person pasting one number into a release note.
//
// # Why every row carries its whole address
//
// Because a spreadsheet row is read on its own, out of order, filtered, and
// pasted into a ticket. A row saying "CVE-2026-31789, critical" without saying
// which image it is in is a row nobody can act on, and it is exactly what a
// naive flattening produces.

// uniqueCVESheet is one row per advisory: the table the page opens on.
//
// The severity is the WORST any image graded it and fixable is true if any
// affected package has a fix - the same roll-up the interface does, because an
// export that disagreed with the screen it was taken from is worse than no
// export.
func uniqueCVESheet(productName, release string, reports []security.Report, filter findingFilter) export.Sheet {
	type group struct {
		cve, id     string
		severity    security.Severity
		fixable     bool
		fixedIn     []string
		cvss        float64
		published   string
		summary     string
		sources     []string
		packages    []string
		images      []string
		occurrences int
		// kev and epss are the two facts a reader sorts this file by before
		// anything else. On the group rather than the row, because being
		// exploited is a property of the advisory: an advisory Anchore flagged
		// in one image is exploited wherever it appears.
		kev        bool
		kevSource  string
		epss       float64
		willNotFix bool
	}

	order := []string{}
	byKey := map[string]*group{}

	for _, report := range reports {
		if !filter.keepReport(report) {
			continue
		}
		for _, f := range report.Findings {
			if !filter.keepFinding(f) {
				continue
			}
			key := f.Identifier()
			if key == "" {
				key = "unknown"
			}
			g, ok := byKey[key]
			if !ok {
				g = &group{cve: f.CVE, id: f.ID, severity: f.Severity, summary: f.Summary}
				byKey[key] = g
				order = append(order, key)
			}
			if f.Severity.Rank() > g.severity.Rank() {
				g.severity = f.Severity
			}
			if f.Fixable {
				g.fixable = true
			}
			if f.CVSSScore > g.cvss {
				g.cvss = f.CVSSScore
			}
			if f.KEV {
				g.kev = true
				if g.kevSource == "" {
					g.kevSource = f.KEVSource
				}
			}
			if f.EPSS != nil && f.EPSS.Score > g.epss {
				g.epss = f.EPSS.Score
			}
			if f.WillNotFix {
				g.willNotFix = true
			}
			if g.published == "" {
				g.published = formatPublished(f.Published)
			}
			if g.summary == "" {
				g.summary = f.Summary
			}
			g.fixedIn = appendUnique(g.fixedIn, f.FixedIn...)
			g.sources = appendUnique(g.sources, f.SourceSet()...)
			if name := f.Component.Display(); name != "" {
				g.packages = appendUnique(g.packages, name)
			}
			g.images = appendUnique(g.images, report.Artifact.Display())
			g.occurrences++
		}
	}

	sheet := export.Sheet{
		Name: "Unique CVEs",
		Headers: []string{
			// Exploited FIRST among the grades, before severity, because that
			// is the order the rows are sorted in and a spreadsheet whose
			// first grading column is not the one it is sorted by teaches the
			// reader to re-sort it.
			"Product", "Release", "CVE", "Issue ID", "Exploited", "Exploited source",
			"Severity", "CVSS", "EPSS", "Fixable", "Fixed in", "Will not fix",
			"Findings", "Images affected", "Packages affected", "Reported by",
			"Images", "Packages", "Advisory published", "Summary",
		},
		// Sized for what the column HOLDS, not for its heading. A CVE is
		// eighteen characters and was being shown in eight; the two list
		// columns are wide because a reader opens this sheet to read them.
		Widths: []int{
			18, 22, 18, 16, 10, 17,
			11, 8, 9, 9, 18, 12,
			10, 15, 17, 16, 46, 46, 18, 70,
		},
	}

	// The same order the page uses and the same order the server stores in:
	// exploited, then severity, then fixable, then blast radius. A file that
	// ordered its rows differently from the page it came from would make
	// somebody check which was right.
	sort.SliceStable(order, func(i, j int) bool {
		a, b := byKey[order[i]], byKey[order[j]]
		if a.kev != b.kev {
			return a.kev
		}
		if a.severity.Rank() != b.severity.Rank() {
			return a.severity.Rank() > b.severity.Rank()
		}
		if a.fixable != b.fixable {
			return a.fixable
		}
		if len(a.images) != len(b.images) {
			return len(a.images) > len(b.images)
		}
		return order[i] < order[j]
	})

	for _, key := range order {
		g := byKey[key]
		sheet.Rows = append(sheet.Rows, []string{
			productName, release, g.cve, g.id,
			exploitedCell(g.kev), providerLabelOrBlank(g.kevSource),
			string(g.severity), formatScore(g.cvss), formatEPSS(g.epss),
			strconv.FormatBool(g.fixable), strings.Join(g.fixedIn, " "),
			strconv.FormatBool(g.willNotFix),
			strconv.Itoa(g.occurrences), strconv.Itoa(len(g.images)), strconv.Itoa(len(g.packages)),
			strings.Join(g.sources, " "),
			// The lists are joined rather than truncated. A cell holding forty
			// image names is unwieldy on screen and exactly right in a
			// spreadsheet, where the reader is about to split it or search it -
			// and "and 37 more" is a cell nothing can do anything with.
			strings.Join(g.images, ", "), strings.Join(g.packages, ", "),
			g.published, g.summary,
		})
	}
	return sheet
}

// findingsSheet is one row per (image, advisory, package): the whole grid.
func findingsSheet(
	productName, release, releaseDigest string, reports []security.Report, filter findingFilter,
) export.Sheet {
	sheet := export.Sheet{
		// The default single-table export. Unique CVEs is the table the page
		// OPENS on, and it is the wrong default for a file: its "images" and
		// "packages" cells are comma-joined lists, which is right for reading
		// and useless for the pivot table the person who exported a CSV is
		// about to build. `?table=unique` gets the other one.
		Name:    "All findings",
		Primary: true,
		Headers: []string{
			"Product", "Release", "Release digest", "Image", "Image tag", "Image digest",
			"Image kind", "Scan status", "CVE", "Issue ID",
			"Exploited", "Exploited source", "Severity", "Fixable", "Fixed in", "Will not fix",
			"Package", "Package version", "Package type", "CVSS", "CVSS vector", "EPSS",
			"Advisory published", "Reported by", "Policy", "Summary",
		},
		Widths: []int{
			18, 22, 20, 28, 20, 20,
			11, 13, 18, 16,
			10, 17, 11, 9, 18, 12,
			24, 18, 13, 8, 30, 9,
			18, 16, 16, 70,
		},
	}
	for _, report := range reports {
		if !filter.keepReport(report) || !scannable(report) {
			continue
		}
		// An image with no findings still gets a row when it was NOT scanned.
		// An export whose absent rows meant both "clean" and "nobody looked"
		// would be the whole failure of this feature, in a file.
		if len(report.Findings) == 0 {
			if report.Status == security.StatusScanned {
				continue
			}
			sheet.Rows = append(sheet.Rows, padTo([]string{
				productName, release, releaseDigest,
				report.Artifact.ArtifactKey(), report.Artifact.Tag, report.Artifact.Digest,
				report.Artifact.Kind, string(report.Status),
			}, len(sheet.Headers), report.Message))
			continue
		}
		for _, f := range report.Findings {
			if !filter.keepFinding(f) {
				continue
			}
			sheet.Rows = append(sheet.Rows, []string{
				productName, release, releaseDigest,
				report.Artifact.ArtifactKey(), report.Artifact.Tag, report.Artifact.Digest,
				report.Artifact.Kind, string(report.Status),
				f.CVE, f.ID,
				exploitedCell(f.KEV), providerLabelOrBlank(f.KEVSource),
				string(f.Severity), strconv.FormatBool(f.Fixable),
				strings.Join(f.FixedIn, " "), strconv.FormatBool(f.WillNotFix),
				f.Component.Name, f.Component.Version, f.Component.Type,
				formatScore(f.CVSSScore), f.CVSSVector, formatEPSSOf(f.EPSS),
				formatPublished(f.Published), strings.Join(f.SourceSet(), " "),
				f.Policy, f.Summary,
			})
		}
	}
	return sheet
}

// imagesSheet is one row per image, with its counts and - as importantly - its
// status and the sentence explaining it.
// scannable reports whether an artifact is one the scanner could have an
// opinion about.
//
// # Why `unsupported` rows leave the export entirely
//
// Because they are not a gap, and every other absent-findings row in these
// sheets is. "Not scanned" and "the scanner would not answer" are work
// somebody has to do; "this is a Helm chart, and Xray scans container images"
// is a fact about what a release contains, not about what nobody looked at.
//
// A real release is 260 artifacts of which 103 are charts, signatures and
// files, so the distinction is not academic: those 103 were a third of the
// Images sheet and a block of blank rows in All findings, and a reader
// scanning either for something to act on had to learn to skip them. The
// platform already treats them this way - coverage's denominator excludes them
// (Coverage.Scannable), and the Problems tab never listed them - so this makes
// the export agree with the two places that were already right.
//
// The COUNT survives, on the Summary sheet, which is where "this release
// contains 103 things the scanner does not cover" belongs: one line, not a
// hundred rows.
func scannable(r security.Report) bool {
	return r.Status != security.StatusUnsupported
}

func imagesSheet(productName, release string, reports []security.Report) export.Sheet {
	sheet := export.Sheet{
		Name: "Images",
		Headers: []string{
			"Product", "Release", "Image", "Tag", "Digest", "Kind", "Status",
			"Vulnerabilities", "Fixable", "Critical", "High", "Medium", "Low", "Unknown",
			"Malware", "Policy violations", "Scanner", "Scanned at", "Note",
		},
		Widths: []int{18, 22, 30, 22, 20, 10, 14, 15, 9, 9, 8, 9, 8, 9, 9, 17, 14, 20, 70},
	}
	for _, r := range reports {
		// A sheet called Images, holding the artifacts that are not images,
		// was a third of its own rows saying "not applicable".
		if !scannable(r) {
			continue
		}
		c := r.Counts
		sheet.Rows = append(sheet.Rows, []string{
			productName, release,
			r.Artifact.ArtifactKey(), r.Artifact.Tag, r.Artifact.Digest, r.Artifact.Kind,
			statusForExport(r),
			strconv.Itoa(c.Total), strconv.Itoa(c.Fixable),
			strconv.Itoa(c.BySeverity.Critical), strconv.Itoa(c.BySeverity.High),
			strconv.Itoa(c.BySeverity.Medium), strconv.Itoa(c.BySeverity.Low),
			strconv.Itoa(c.BySeverity.Unknown),
			strconv.Itoa(len(r.Malware)), strconv.Itoa(len(r.Violations)),
			r.Provider, formatTimePtr(r.ScannedAt), r.Message,
		})
	}
	return sheet
}

// statusForExport reports "not_found" for an image that is not in the scanned
// repository at all.
//
// The same distinction the interface draws, and for the same reason: "nobody
// has scanned this" is a job for whoever owns scanning and "this was never
// replicated here" is a transfer, and Xray reports them with one sentence.
func statusForExport(r security.Report) string {
	if r.Status == security.StatusNotScanned && r.Missing {
		return "not_found"
	}
	return string(r.Status)
}

// malwareSheet is what must not ship.
//
// Its own sheet rather than a severity filter on the findings, because it is
// not a severity: a malicious package is a different KIND of answer, and a
// reader who filters the findings sheet to "critical" would not find it.
func malwareSheet(productName, release string, reports []security.Report) export.Sheet {
	sheet := export.Sheet{
		Name: "Malware",
		Headers: []string{
			"Product", "Release", "Image", "Tag", "Image digest",
			"Identifier", "Severity", "Package", "Package version", "Package type",
			"Fixed in", "Reported by", "Policy", "Summary",
		},
		Widths: []int{18, 22, 30, 22, 20, 18, 11, 26, 18, 13, 18, 16, 18, 70},
	}
	for _, r := range reports {
		for _, f := range r.Malware {
			sheet.Rows = append(sheet.Rows, []string{
				productName, release,
				r.Artifact.ArtifactKey(), r.Artifact.Tag, r.Artifact.Digest,
				firstNonEmpty(f.CVE, f.ID), string(f.Severity),
				f.Component.Name, f.Component.Version, f.Component.Type,
				strings.Join(f.FixedIn, " "), strings.Join(f.SourceSet(), " "),
				f.Policy, f.Summary,
			})
		}
	}
	return sheet
}

// policySheet is what the configured watches say.
func policySheet(productName, release string, reports []security.Report) export.Sheet {
	sheet := export.Sheet{
		Name: "Policy violations",
		Headers: []string{
			"Product", "Release", "Image", "Tag", "Image digest",
			"Violation ID", "Type", "Severity", "Watch", "Policy", "Rule",
			"CVE", "Package", "Package version", "Fixed in", "Created", "Scanner", "Description",
		},
		Widths: []int{18, 22, 30, 22, 20, 18, 14, 11, 22, 22, 22, 18, 26, 18, 18, 20, 14, 70},
	}
	for _, r := range reports {
		for _, v := range r.Violations {
			sheet.Rows = append(sheet.Rows, []string{
				productName, release,
				r.Artifact.ArtifactKey(), r.Artifact.Tag, r.Artifact.Digest,
				v.ID, v.Type, string(v.Severity), v.Watch, v.Policy, v.Rule,
				v.CVE, v.Component.Name, v.Component.Version,
				strings.Join(v.FixedIn, " "), formatTimePtr(v.Created), v.Provider, v.Summary,
			})
		}
	}
	return sheet
}

// problemsSheet is what could not be scanned, grouped by the reason.
//
// Grouped, because a release of 260 images against an unreachable scanner
// produces 260 rows and three reasons, and three reasons with their counts is
// something a reader acts on where 260 rows is something they scroll past. The
// images are named in a cell so the grouping loses nothing.
func problemsSheet(productName, release string, reports []security.Report) export.Sheet {
	type problem struct {
		status string
		reason string
		images []string
	}
	order := []string{}
	byKey := map[string]*problem{}

	for _, r := range reports {
		if r.Status == security.StatusScanned || r.Status == security.StatusUnsupported {
			continue
		}
		reason := strings.TrimSpace(r.Message)
		if reason == "" {
			reason = "The scanner gave no reason."
		}
		key := statusForExport(r) + "|" + reason
		p, ok := byKey[key]
		if !ok {
			p = &problem{status: statusForExport(r), reason: reason}
			byKey[key] = p
			order = append(order, key)
		}
		p.images = append(p.images, r.Artifact.Display())
	}

	sheet := export.Sheet{
		Name:    "Problems",
		Headers: []string{"Product", "Release", "Status", "Images", "Reason", "Affected images"},
		Widths:  []int{18, 22, 14, 9, 70, 60},
	}
	sort.SliceStable(order, func(i, j int) bool {
		return len(byKey[order[i]].images) > len(byKey[order[j]].images)
	})
	for _, key := range order {
		p := byKey[key]
		sheet.Rows = append(sheet.Rows, []string{
			productName, release, p.status, strconv.Itoa(len(p.images)),
			p.reason, strings.Join(p.images, ", "),
		})
	}
	return sheet
}

// sourcesSheet is which scanner reported what, and what only one of them saw.
//
// Present only where more than one scanner contributed. On a single-scanner
// deployment it would be the release's own numbers restated under a heading
// that implies a comparison, which is a sheet that teaches a reader to expect
// something that is not there.
func sourcesSheet(
	productName, release string, posture security.Posture, cmp security.SourceComparison,
) (export.Sheet, bool) {
	if len(posture.BySource) < 2 {
		return export.Sheet{}, false
	}
	sheet := export.Sheet{
		Name: "By source",
		Headers: []string{
			"Product", "Release", "Scanner", "Images answered", "Findings", "Fixable",
			"Critical", "High", "Medium", "Low", "Unknown",
			"Distinct CVEs", "Only this scanner", "Exploited", "Exploited only here", "Enriched",
		},
		Widths: []int{18, 22, 16, 15, 11, 9, 9, 8, 9, 8, 9, 14, 18, 10, 19, 10},
	}
	for _, src := range posture.BySource {
		c := src.Counts
		agreement := cmp.Counts[src.Provider]
		sheet.Rows = append(sheet.Rows, []string{
			productName, release, providerLabel(src.Provider), strconv.Itoa(src.Artifacts),
			strconv.Itoa(c.Total), strconv.Itoa(c.Fixable),
			strconv.Itoa(c.BySeverity.Critical), strconv.Itoa(c.BySeverity.High),
			strconv.Itoa(c.BySeverity.Medium), strconv.Itoa(c.BySeverity.Low),
			strconv.Itoa(c.BySeverity.Unknown),
			strconv.Itoa(src.UniqueCVEs), strconv.Itoa(src.OnlyHere),
			strconv.Itoa(src.KEVs), strconv.Itoa(agreement.KEVOnly), strconv.Itoa(agreement.Enriched),
		})
	}
	return sheet, true
}

// disagreementSheet lists the advisories the scanners did not agree on.
//
// # Why this is a sheet and not a number
//
// Because "Anchore found 402 Xray did not" is the headline and this is the
// evidence. A reader deciding whether to keep two scanners needs to be able to
// look at what the disagreement actually consists of - and on the page that
// list is capped at two hundred, because a browser rendering four thousand
// identifiers is a page that takes a second to open. A spreadsheet has no such
// problem, so this is the ONE place the full set exists.
//
// Present only where more than one scanner contributed, on the same argument
// as the sheet above.
func disagreementSheet(
	productName, release string, cmp security.SourceComparison, kevOnly map[string]bool,
) (export.Sheet, bool) {
	if len(cmp.Providers) < 2 {
		return export.Sheet{}, false
	}
	sheet := export.Sheet{
		Name: "Scanner disagreement",
		Headers: []string{
			"Product", "Release", "Advisory", "Reported by", "Exploited",
		},
		Widths: []int{18, 22, 18, 22, 10},
	}

	// Exploited first, then the rest alphabetically. Same argument as
	// everywhere else: a truncated read of this sheet must not truncate away
	// the rows that matter, and somebody who opens it and reads the first
	// screen should have seen those.
	type row struct {
		id       string
		provider string
		kev      bool
	}
	var rows []row
	for _, provider := range cmp.Providers {
		for _, id := range cmp.OnlyIn[provider] {
			rows = append(rows, row{id: id, provider: provider, kev: kevOnly[id]})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].kev != rows[j].kev {
			return rows[i].kev
		}
		if rows[i].provider != rows[j].provider {
			return rows[i].provider < rows[j].provider
		}
		return rows[i].id < rows[j].id
	})
	for _, r := range rows {
		sheet.Rows = append(sheet.Rows, []string{
			productName, release, r.id, "Only " + providerLabel(r.provider), exploitedCell(r.kev),
		})
	}
	if len(sheet.Rows) == 0 {
		// Every scanner agreed on everything, which is a real and worth-saying
		// result - and an empty sheet with a header row says it badly. The
		// summary sheet carries the number; this one goes away.
		return export.Sheet{}, false
	}
	return sheet, true
}

// providerLabel is a scanner's name as a person writes it.
func providerLabel(provider string) string {
	switch provider {
	case "jfrog-xray":
		return "JFrog Xray"
	case "anchore":
		return "Anchore"
	case "astra":
		return "Astra"
	case "":
		return "-"
	default:
		return provider
	}
}

// ---------------------------------------------------------------------------
// The bundle
// ---------------------------------------------------------------------------

// bundleFiles lays the scanner's own bodies out as a directory tree.
//
// One directory per KIND, then ONE per image-and-tag - see export.WriteZIP for
// why kind comes first. The filename names the scanner, so a bundle from a
// deployment running two of them does not have one overwrite the other.
//
// # Why the tag is not a directory of its own
//
// It was, and it bought nothing: a release holds one tag per image, so every
// image directory contained exactly one tag directory containing one file, and
// opening a bundle meant three clicks to reach every document. `cbur-cbur-agent`
// and `1.5.7-alpine-24` name one thing here, so they are one directory.
//
// The separator is `__` rather than the `:` that reads most naturally. A colon
// is a reserved character on Windows - it is the drive separator - and Explorer
// refuses to extract an archive containing one. These bundles exist to be
// FORWARDED, and an archive that fails to open on the recipient's laptop is a
// worse outcome than a separator nobody would have chosen.
func bundleFiles(
	docs map[string]map[security.DocumentKind]security.Document,
	reports []security.Report,
	fallbackProvider string,
) []export.File {
	var out []export.File
	for _, r := range reports {
		held, ok := docs[r.Artifact.Ref()]
		if !ok {
			continue
		}
		for _, kind := range security.AllDocumentKinds {
			doc, ok := held[kind]
			if !ok || len(doc.Payload) == 0 {
				continue
			}
			// The scanner in the filename, which is what keeps two scanners'
			// answers about one image from overwriting each other in the ZIP.
			// The document's own stamp first, the scope it was read from
			// second, the report's third - a stored row written before
			// provider was stamped on documents has only the last.
			provider := doc.Provider
			if provider == "" {
				provider = fallbackProvider
			}
			if provider == "" {
				provider = r.Provider
			}
			out = append(out, export.File{
				Path: strings.Join([]string{
					kind.Folder(),
					bundleSegment(r.Artifact.ArtifactKey()) + "__" +
						bundleSegment(tagOrDigest(r.Artifact)),
					bundleSegment(provider) + doc.Extension(),
				}, "/"),
				Body: doc.Payload,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// tagOrDigest names the directory one image's documents live in.
//
// The tag where there is one, because that is what a person calls the image;
// the short digest otherwise, because an untagged image still needs a folder
// and "untagged" would collide across every untagged image in the release.
func tagOrDigest(ref security.ArtifactRef) string {
	if ref.Tag != "" {
		return ref.Tag
	}
	return shortDigest(ref.Digest)
}

// bundleSegment makes one path component safe on every filesystem a bundle gets
// unzipped on.
//
// Windows refuses `:` and `*` in a filename and a tag routinely contains the
// first, so an unsanitised bundle is one that extracts on Linux and fails on
// the laptop of the person it was sent to.
func bundleSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
	mapped = strings.Trim(mapped, "-.")
	if mapped == "" {
		return "unknown"
	}
	if len(mapped) > 80 {
		mapped = mapped[:80]
	}
	return mapped
}

// bundleReadme is the file that stops the bundle needing an accompanying email.
//
// A ZIP of 600 JSON files with no explanation is a thing the recipient forwards
// back asking what it is. Six lines of plain text is the difference between an
// artefact somebody can act on and one they have to ask about.
func bundleReadme(
	productName string, pkg store.PackageRow, side securitySide,
	docs map[string]map[security.DocumentKind]security.Document,
) export.File {
	var b strings.Builder
	fmt.Fprintf(&b, "Security evidence bundle\n")
	fmt.Fprintf(&b, "========================\n\n")
	fmt.Fprintf(&b, "Product:   %s\n", productName)
	fmt.Fprintf(&b, "Release:   %s\n", releaseLabel(pkg))
	fmt.Fprintf(&b, "Digest:    %s\n", pkg.ManifestDigest)
	fmt.Fprintf(&b, "Scanner:   %s\n", providerLabel(providerOr(side.row.Provider, side.target)))
	fmt.Fprintf(&b, "Repository:%s\n", " "+repositoryOr(side.row.Repository, side.target))
	fmt.Fprintf(&b, "Synced:    %s\n", formatTimePtr(side.row.SyncedAt))
	fmt.Fprintf(&b, "Exported:  %s\n\n", nowRFC3339())

	fmt.Fprintf(&b, "Contents\n--------\n")
	fmt.Fprintf(&b, "tables/            The same tables the interface shows, as CSV.\n")
	for _, kind := range security.AllDocumentKinds {
		n := 0
		for _, held := range docs {
			if doc, ok := held[kind]; ok && len(doc.Payload) > 0 {
				n++
			}
		}
		fmt.Fprintf(&b, "%-18s %s, one file per image (%d).\n",
			kind.Folder()+"/", kind.Label(), n)
	}
	fmt.Fprintf(&b, "\nThe files under the four directories above are the SCANNER'S OWN\n")
	fmt.Fprintf(&b, "responses, unaltered. The tables are this platform's reading of them.\n")
	fmt.Fprintf(&b, "Where the two disagree, the scanner's response is the record.\n")

	return export.File{Path: "README.txt", Body: []byte(b.String())}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// nowRFC3339 is the export's own timestamp.
func nowRFC3339() string { return time.Now().UTC().Format(rfc3339) }

func appendUnique(list []string, values ...string) []string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		found := false
		for _, existing := range list {
			if existing == v {
				found = true
				break
			}
		}
		if !found {
			list = append(list, v)
		}
	}
	return list
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// padTo grows a row to the sheet's width, putting `last` in the final column.
func padTo(row []string, width int, last string) []string {
	out := make([]string, width)
	copy(out, row)
	if width > 0 {
		out[width-1] = last
	}
	return out
}

// exploitedCell is the known-exploited flag as a spreadsheet reads it.
//
// # Why "yes" and "" rather than TRUE and FALSE
//
// Because the whole point of this column is that a reader's eye finds the few
// rows that have it, in a file of ninety thousand. A column of FALSE with four
// TRUEs in it is a column of noise; a mostly-empty column with four "yes" cells
// is a column somebody can see from across the sheet, and it sorts and filters
// exactly the same way.
func exploitedCell(kev bool) string {
	if kev {
		return "yes"
	}
	return ""
}

// providerLabelOrBlank names a scanner, or nothing where there is none to name.
//
// Blank rather than "-", because a spreadsheet's empty cell already means "not
// applicable" and a dash is a value somebody's filter has to know to exclude.
func providerLabelOrBlank(provider string) string {
	if provider == "" {
		return ""
	}
	return providerLabel(provider)
}

// formatEPSS renders an exploit-prediction score as a percentage.
//
// A percentage rather than the raw probability, for the reason the interface
// gives it one: 0.00042 is a number almost nobody can act on. Two decimal
// places, because the interesting range is a fraction of a percent and rounding
// to whole numbers would render every score below 0.5% as zero.
func formatEPSS(score float64) string {
	if score <= 0 {
		return ""
	}
	return strconv.FormatFloat(score*100, 'f', 2, 64) + "%"
}

func formatEPSSOf(epss *security.EPSS) string {
	if epss == nil {
		return ""
	}
	return formatEPSS(epss.Score)
}
