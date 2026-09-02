package export

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func sampleBook() Book {
	return Book{
		Sheets: []Sheet{
			{Name: "Summary", Headers: []string{"Field", "Value"}, Rows: [][]string{
				{"Verdict", "better"}, {"Resolved", "8"},
			}},
			{Name: "Findings", Primary: true, Headers: []string{"CVE", "Severity", "Count"}, Rows: [][]string{
				{"CVE-2024-3094", "critical", "34"},
				{"CVE-2024-21887", "medium", "9"},
			}},
		},
	}
}

func TestParseFormatAcceptsWhatPeopleType(t *testing.T) {
	for in, want := range map[string]Format{
		"": FormatCSV, "csv": FormatCSV, "CSV": FormatCSV,
		"xlsx": FormatXLSX, "excel": FormatXLSX, "xls": FormatXLSX,
		"json": FormatJSON,
		"zip":  FormatZIP, "bundle": FormatZIP,
	} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseFormat("pdf"); err == nil {
		t.Error("accepted an unsupported format")
	}
}

// CSV holds one table, so it writes the PRIMARY sheet - the one somebody
// exporting to CSV came for.
//
// It used to be "the last sheet", and that broke as soon as a book carried four
// data tables: a CSV export of a release's vulnerabilities handed back whichever
// table happened to be assembled last.
func TestWriteCSVEmitsThePrimarySheet(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, FormatCSV, sampleBook()); err != nil {
		t.Fatal(err)
	}

	body := buf.String()
	if !strings.HasPrefix(body, "\xef\xbb\xbf") {
		t.Error("no UTF-8 BOM: Excel opens a BOM-less CSV as the system code page")
	}

	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header and two findings", len(rows))
	}
	if rows[0][0] != "CVE" {
		t.Errorf("a sheet other than the primary one was written: %v", rows[0])
	}
}

func TestWriteJSONKeepsTheStructuredPayload(t *testing.T) {
	book := sampleBook()
	book.JSON = map[string]any{"verdict": "better", "changes": []string{"a", "b"}}

	var buf bytes.Buffer
	if err := Write(&buf, FormatJSON, book); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	// The tree, not the flattened grid: a machine consuming an export wants
	// the relationships the grid throws away.
	if got["verdict"] != "better" {
		t.Errorf("payload = %v", got)
	}

	// With no structured payload the grid is still emitted, so a JSON export
	// is never empty just because a caller supplied none.
	book.JSON = nil
	buf.Reset()
	if err := Write(&buf, FormatJSON, book); err != nil {
		t.Fatal(err)
	}
	var fallback map[string][]map[string]string
	if err := json.Unmarshal(buf.Bytes(), &fallback); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(fallback["Findings"]) != 2 {
		t.Errorf("fallback = %v", fallback)
	}
}

func TestWriteXLSXProducesAnOpenableWorkbook(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, FormatXLSX, sampleBook()); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}

	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		parts[f.Name] = string(body)
	}

	for _, required := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml",
	} {
		if _, ok := parts[required]; !ok {
			t.Fatalf("missing %s: a spreadsheet will refuse this file", required)
		}
	}

	// Every part must be well-formed XML, or Excel offers to "repair" the file
	// - which is what a stray control character in a scanner's description
	// would otherwise cause.
	for name, body := range parts {
		if !strings.HasSuffix(name, ".xml") && !strings.HasSuffix(name, ".rels") {
			continue
		}
		if err := xml.Unmarshal([]byte(body), new(any)); err != nil {
			t.Errorf("%s is not well-formed: %v", name, err)
		}
	}

	sheet := parts["xl/worksheets/sheet2.xml"]
	// A count written as a string sorts lexically in Excel, so 10 comes before
	// 9 and every "sort by vulnerabilities" gives the wrong answer.
	if !strings.Contains(sheet, `<c r="C2"><v>34</v></c>`) {
		t.Errorf("the count column was not written as a number:\n%s", sheet)
	}
	// A CVE id contains digits and must stay text.
	if !strings.Contains(sheet, `t="inlineStr"`) || strings.Contains(sheet, `<v>CVE`) {
		t.Errorf("a CVE id was written as a number:\n%s", sheet)
	}
}

func TestXLSXSurvivesHostileText(t *testing.T) {
	book := Book{Sheets: []Sheet{{
		Name:    "Bad/Name:With*Chars-and-a-very-long-tail-that-excel-refuses",
		Headers: []string{"Text"},
		Rows:    [][]string{{"a <tag> & an \x07 escape \"quoted\""}},
	}}}

	var buf bytes.Buffer
	if err := Write(&buf, FormatXLSX, book); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		if err := xml.Unmarshal(body, new(any)); err != nil {
			t.Errorf("%s is not well-formed: %v", f.Name, err)
		}
		if bytes.Contains(body, []byte{0x07}) {
			t.Errorf("%s carries a control character XML 1.0 forbids", f.Name)
		}
	}
}

func TestCellRef(t *testing.T) {
	for _, tc := range []struct {
		col, row int
		want     string
	}{{0, 1, "A1"}, {25, 1, "Z1"}, {26, 2, "AA2"}, {27, 10, "AB10"}, {51, 1, "AZ1"}, {52, 1, "BA1"}} {
		if got := cellRef(tc.col, tc.row); got != tc.want {
			t.Errorf("cellRef(%d,%d) = %q, want %q", tc.col, tc.row, got, tc.want)
		}
	}
}

func TestFilenameIsSafeAndDated(t *testing.T) {
	at := time.Date(2026, 6, 29, 1, 14, 0, 0, time.UTC)
	got := Filename([]string{"cfx 5000", "25.7/2131", "security"}, FormatCSV, at)
	want := "cfx-5000_25-7-2131_security_20260629-011400.csv"
	if got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
}

// A row shorter than the header must not shift every later column left.
func TestShortRowsArePadded(t *testing.T) {
	book := Book{Sheets: []Sheet{{
		Headers: []string{"A", "B", "C"},
		Rows:    [][]string{{"1"}, {"1", "2", "3"}},
	}}}
	var buf bytes.Buffer
	if err := Write(&buf, FormatCSV, book); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows[1]) != 3 {
		t.Errorf("short row was not padded: %v", rows[1])
	}
}

// A cell longer than Excel holds is a workbook Excel calls damaged.
//
// This is not a hypothetical rounding of a limit. The compliance report carries
// helm's own stderr - `error validating data:` can hand back a whole rejected
// object on one line - and Excel's answer to a cell over 32,767 characters is
// to open the file with "we found a problem with some content" and repair it by
// throwing the string away:
//
//	Repaired Records: String properties from /xl/worksheets/sheet4.xml part
//
// The reader is then told the report is corrupt, which is worse than a long
// cell by every measure.
func TestXLSXCellsStayInsideExcelsLimit(t *testing.T) {
	huge := strings.Repeat("helm template failed: ", 6000) // ~132,000 characters
	book := Book{Sheets: []Sheet{{
		Name:    "Charts",
		Headers: []string{"Chart", "Renderer message"},
		Rows:    [][]string{{"cfx-nssaaf-chart", huge}},
		Wrap:    []int{1},
	}}}

	var buf bytes.Buffer
	if err := Write(&buf, FormatXLSX, book); err != nil {
		t.Fatal(err)
	}
	sheet := xlsxPart(t, buf.Bytes(), "xl/worksheets/sheet1.xml")

	for _, text := range inlineStrings(t, sheet) {
		if n := len([]rune(text)); n > 32767 {
			t.Errorf("a cell holds %d characters: Excel repairs the workbook rather than opening it", n)
		}
	}
	if !strings.Contains(sheet, "more characters") {
		t.Error("the cell was cut with nothing saying so, which reads as the whole message")
	}
}

// The limit is Excel's, so a CSV keeps the whole thing.
//
// Somebody exporting a table to grep it should not be handed the spreadsheet's
// constraint, and the CSV is the format they reach for when the workbook's cell
// was not big enough.
func TestCSVKeepsWhatExcelCannotHold(t *testing.T) {
	huge := strings.Repeat("x", 40000)
	book := Book{Sheets: []Sheet{{
		Name: "Charts", Primary: true,
		Headers: []string{"Message"}, Rows: [][]string{{huge}},
	}}}

	var buf bytes.Buffer
	if err := Write(&buf, FormatCSV, book); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), huge) {
		t.Error("the CSV was cut to a spreadsheet's limit")
	}
}

// A sheet name is cut on a character, not on a byte.
//
// Half a multi-byte rune in xl/workbook.xml is invalid UTF-8 in the part that
// names every sheet - which is the whole workbook, not one cell.
func TestSheetNameIsCutOnARuneBoundary(t *testing.T) {
	// 32 three-byte runes: a byte cut lands inside the 11th one.
	book := Book{Sheets: []Sheet{{Name: strings.Repeat("三", 32), Headers: []string{"A"}}}}

	var buf bytes.Buffer
	if err := Write(&buf, FormatXLSX, book); err != nil {
		t.Fatal(err)
	}
	workbook := xlsxPart(t, buf.Bytes(), "xl/workbook.xml")
	if !utf8.ValidString(workbook) {
		t.Fatal("xl/workbook.xml is not valid UTF-8")
	}
	if strings.Contains(workbook, "�") {
		t.Error("a sheet name was cut through a character")
	}
}

func xlsxPart(t *testing.T, body []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	t.Fatalf("the workbook has no %s", name)
	return ""
}

// inlineStrings returns every cell's text, unescaped, from a worksheet part.
func inlineStrings(t *testing.T, sheet string) []string {
	t.Helper()
	var out []string
	dec := xml.NewDecoder(strings.NewReader(sheet))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "t" {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			t.Fatalf("decoding a cell: %v", err)
		}
		out = append(out, text)
	}
	return out
}
