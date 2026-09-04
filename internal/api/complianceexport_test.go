package api

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"
)

const exportBase = "/api/v1/products/vendor-a/packages/25.7.2131/compliance/export"

// The byte order mark the CSV writer leads with, so Excel opens a UTF-8 file as
// UTF-8. Named rather than written literally: a BOM in a Go source file is a
// compile error, and one pasted into a string is invisible to the next reader.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// The workbook holds both tables, and neither is the other's summary.
//
// "Five rules are broken" is what a vendor gets a meeting about; "here are the
// 171 places" is what they work through afterwards. Shipping one and calling it
// the report means somebody re-derives the other by hand in the same
// spreadsheet.
func TestComplianceReportHoldsEveryTable(t *testing.T) {
	h := newEvidenceHarness(t)

	body, header := h.getRaw(exportBase + "?format=xlsx")
	if !strings.Contains(header.Get("Content-Type"), "spreadsheetml") {
		t.Fatalf("content type = %q", header.Get("Content-Type"))
	}
	if !strings.Contains(header.Get("Content-Disposition"), "compliance") {
		t.Errorf("the file is not named for what it is: %q", header.Get("Content-Disposition"))
	}

	workbook := workbookXMLOf(t, body)
	for _, sheet := range []string{
		"Summary", "Unique findings", "All findings", "Unchecked", "Charts", "Rulebook",
	} {
		if !strings.Contains(workbook, sheet) {
			t.Errorf("the workbook has no %q sheet", sheet)
		}
	}
}

// Every row carries its whole address, and that is not redundancy.
//
// This file is opened by somebody with no access to this platform - a vendor
// engineer sent a spreadsheet, an auditor shown that a release was checked - so
// "SEC-01, critical" without the chart, the template, the LINE and the field is
// a row nobody can act on. It is exactly what a naive flattening produces.
func TestComplianceFindingsCarryTheWholeAddress(t *testing.T) {
	h := newEvidenceHarness(t)

	body, _ := h.getRaw(exportBase + "?format=csv&table=findings")
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, utf8BOM))).ReadAll()
	if err != nil {
		t.Fatalf("not a csv: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("the findings table has %d rows", len(rows))
	}
	index := map[string]int{}
	for i, h := range rows[0] {
		index[h] = i
	}
	for _, column := range []string{
		"Check", "Title", "Severity", "Chart", "Chart version",
		"Template file", "Line", "Kind", "Resource", "Container", "Field",
		"Observed", "Finding", "Product", "Release", "Release digest",
		// The triage columns. A severity says how much this organization
		// cares; these say what the reader should do about it, and without
		// them the file is a list of complaints rather than a plan. "Who
		// fixes it" replaces a column that was headed "Owner" and held the
		// determinacy, which is now "Value is" and says what it means.
		"Who fixes it", "Effort", "When it bites", "Confidence", "Value is",
		"Example fix",
	} {
		if _, ok := index[column]; !ok {
			t.Errorf("the findings table has no %q column", column)
		}
	}

	found := false
	for _, row := range rows[1:] {
		if row[index["Check"]] != "SEC-01" {
			continue
		}
		found = true
		// The line is the column somebody opens the template at, and it was
		// the one most easily lost: it is not on the screen's row, only in the
		// evidence panel.
		if row[index["Line"]] != "2" {
			t.Errorf("Line = %q, want 2", row[index["Line"]])
		}
		if row[index["Template file"]] != "alpha/templates/deployment.yaml" {
			t.Errorf("Template file = %q", row[index["Template file"]])
		}
		if row[index["Field"]] == "" || row[index["Container"]] != "main" {
			t.Errorf("the row does not say where in the object: %v", row)
		}
		if row[index["Release digest"]] == "" || row[index["Product"]] != "vendor-a" {
			t.Errorf("the row does not say which release it is about: %v", row)
		}
	}
	if !found {
		t.Error("SEC-01 is not in the findings table")
	}
}

// The grouped table counts the PLACES, and the flat one lists them.
func TestComplianceUniqueTableCountsThePlaces(t *testing.T) {
	h := newEvidenceHarness(t)

	rows := csvRowsOf(t, h, exportBase+"?format=csv&table=unique")
	index := map[string]int{}
	for i, h := range rows[0] {
		index[h] = i
	}
	for _, column := range []string{
		"Check", "Places", "Charts", "Examples", "Remediation",
		"Who fixes it", "Effort", "When it bites", "Value is", "Example fix",
	} {
		if _, ok := index[column]; !ok {
			t.Fatalf("the unique table has no %q column: %v", column, rows[0])
		}
	}
	// The harness has SEC-01 failing once and reporting undecided once. The
	// undecided one is NOT a place the rule is broken, and counting it here
	// would report a defect against a chart nothing was read from.
	for _, row := range rows[1:] {
		if row[index["Check"]] != "SEC-01" {
			continue
		}
		if row[index["Places"]] != "1" {
			t.Errorf("SEC-01 places = %q, want 1: an undecided check is not a place a rule was broken",
				row[index["Places"]])
		}
	}
}

// A check that could not be decided is not a defect, and putting it in the
// findings table would report one against a chart nothing was read from.
func TestComplianceUncheckedIsItsOwnTable(t *testing.T) {
	h := newEvidenceHarness(t)

	findings := csvRowsOf(t, h, exportBase+"?format=csv&table=findings")
	for _, row := range findings[1:] {
		if strings.EqualFold(row[3], "Could not be checked") {
			t.Errorf("an undecided check is in the findings table: %v", row)
		}
	}

	unchecked := csvRowsOf(t, h, exportBase+"?format=csv&table=unchecked")
	if len(unchecked) < 2 {
		t.Fatalf("the unchecked table is empty, over a run with one: %v", unchecked)
	}
	if !strings.Contains(strings.Join(unchecked[1], " "), "beta") {
		t.Errorf("the unchecked row does not name the chart it is about: %v", unchecked[1])
	}
}

// The coverage table says WHY a chart did not render, not only that it did not.
//
// Everything below it is a smaller denominator than it looks unless this is
// read, and a column of undifferentiated renderer messages is how thirteen
// failures become one complaint about the tool.
func TestComplianceChartsTableSaysWhy(t *testing.T) {
	h := newEvidenceHarness(t)

	rows := csvRowsOf(t, h, exportBase+"?format=csv&table=charts")
	index := map[string]int{}
	for i, h := range rows[0] {
		index[h] = i
	}
	for _, column := range []string{
		"Chart", "Rendered", "Objects", "Reason", "Value required",
		"In a helm test hook", "Renderer message",
	} {
		if _, ok := index[column]; !ok {
			t.Errorf("the charts table has no %q column", column)
		}
	}
	// Failures first: on ninety-five charts the ones that did not render are
	// otherwise rows behind eighty-two others.
	if rows[1][index["Chart"]] != "beta" {
		t.Errorf("the first chart row is %q, want the one that failed", rows[1][index["Chart"]])
	}
	if rows[1][index["Rendered"]] != "no" || rows[1][index["Reason"]] == "" {
		t.Errorf("the failed chart's row does not say it failed or why: %v", rows[1])
	}
}

// A release nobody has checked has no report, and an empty workbook would read
// as a pass - which is the one thing this whole feature exists to prevent.
func TestComplianceReportOnAnUncheckedRelease(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) { d.ComplianceStore = d.Packages })
	h.seedPackage("25.7.2131", digestA)

	var problem struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if code := h.get(exportBase+"?format=xlsx", &problem); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: a workbook here would read as a pass", code)
	}
	if !strings.Contains(problem.Detail, "has not been checked") {
		t.Errorf("the refusal does not say why: %q", problem.Detail)
	}
}

// ---------------------------------------------------------------------------

func csvRowsOf(t *testing.T, h *evidenceHarness, path string) [][]string {
	t.Helper()
	body, _ := h.getRaw(path)
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, utf8BOM))).ReadAll()
	if err != nil {
		t.Fatalf("GET %s: not a csv: %v", path, err)
	}
	if len(rows) == 0 {
		t.Fatalf("GET %s returned no rows", path)
	}
	return rows
}

func workbookXMLOf(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "xl/workbook.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		out, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	t.Fatal("the workbook has no xl/workbook.xml")
	return ""
}
