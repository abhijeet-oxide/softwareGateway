// Package export writes tabular data as CSV, JSON and Excel.
//
// # Why there is no library here
//
// Excel is the format people asked for, and the obvious answer is a
// spreadsheet library. This writes the file directly instead, for two reasons
// that outlast the convenience:
//
//  1. An xlsx is a zip of half a dozen small XML parts, and the subset needed
//     for "a grid of strings and numbers with a header row" is the file below.
//     A dependency that renders charts, formulas and pivot tables to write a
//     table of CVEs is a large supply-chain surface for a small job - on a
//     product whose entire purpose is telling people what is in their supply
//     chain.
//  2. Exports are streamed to an HTTP response while a user waits. Writing
//     directly means one pass and no intermediate document in memory, which
//     matters when the detailed export of a large release is a hundred thousand
//     rows.
//
// The trade is that this writes a PLAIN grid: no formatting, no column widths,
// no frozen header. Excel, LibreOffice and Sheets all open it.
package export

import (
	"archive/zip"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Format is an export encoding.
type Format string

const (
	FormatCSV  Format = "csv"
	FormatXLSX Format = "xlsx"
	FormatJSON Format = "json"
	// FormatZIP is the whole evidence bundle: the tables as CSV, and the
	// scanner's own bodies laid out one directory per image.
	//
	// # Why a fourth format rather than a second endpoint
	//
	// Because it answers the same question - "give me this release's security
	// state" - for a different reader. A spreadsheet is for somebody working
	// through the findings; the bundle is for somebody FORWARDING them, to a
	// customer, an auditor, or a vendor support case, who needs the scanner's
	// own words rather than this platform's summary of them.
	FormatZIP Format = "zip"
)

// ParseFormat maps what a caller asked for onto a format.
//
// `excel` and `xls` are accepted for xlsx because that is what people type, and
// refusing them teaches nobody anything.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(FormatCSV):
		return FormatCSV, nil
	case string(FormatXLSX), "excel", "xls":
		return FormatXLSX, nil
	case string(FormatJSON):
		return FormatJSON, nil
	case string(FormatZIP), "bundle", "archive":
		return FormatZIP, nil
	default:
		return "", fmt.Errorf("format must be one of csv, xlsx, json, zip (got %q)", s)
	}
}

// ContentType is the MIME type to serve a format as.
func (f Format) ContentType() string {
	switch f {
	case FormatXLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case FormatJSON:
		return "application/json"
	case FormatZIP:
		return "application/zip"
	default:
		return "text/csv; charset=utf-8"
	}
}

// Extension is the filename suffix for a format.
func (f Format) Extension() string { return "." + string(f) }

// Sheet is one named table.
//
// A slice of sheets rather than one table, because the shape the requirement
// asks for is a summary AND a detailed breakdown, and in a spreadsheet those
// are two tabs. CSV cannot hold two tables, so it writes the last sheet and
// says so in the caller's filename - see WriteCSV.
type Sheet struct {
	Name    string
	Headers []string
	Rows    [][]string
	// Primary marks the sheet a single-table format should write.
	//
	// It used to be "the last one", which worked while a book was summary-then-
	// detail and broke the moment a book had four data sheets: a CSV export of
	// a release's vulnerabilities handed back the Problems tab, because that
	// happened to be built last. The sheet somebody means is a property of the
	// book, not of the order it was assembled in, so the book says which.
	Primary bool
	// Note is one line above the header, for a sheet whose rows need a caveat
	// that would be a lie repeated on every row - "these are the 1,000 highest
	// severity of 3,111". Omitted from CSV, which has nowhere to put it.
	Note string
	// Widths are column widths in characters, in column order. Short lists are
	// fine: anything not named gets a width derived from its header.
	//
	// # Why widths are worth carrying at all
	//
	// Because the default is eight characters, and a workbook whose every
	// column reads `########` or `CVE-202…` is one the reader has to fix before
	// they can look at it. Twenty-two columns of hand-resizing is the reason
	// people say a generated spreadsheet is not usable.
	Widths []int
	// Title, when set, is a heading row above everything else - bigger, bold,
	// and spanning. Used by the summary sheet, which is read rather than
	// filtered.
	Title string
	// Wrap names the columns whose cells wrap and sit at the top of the row,
	// by zero-based index.
	//
	// # Why a column has to ask for it
	//
	// Because a cell of eighty characters, or one holding three addresses on
	// three lines, renders as ONE line clipped at the column edge - and the
	// three-line cell renders as its first line with nothing saying there are
	// two more. Excel does not wrap by default and a reader cannot discover
	// that the rest is there.
	//
	// Not every column, because wrapping a chart name or a digest makes a row
	// four lines tall to no purpose. Only the prose: a finding, a remediation,
	// a renderer's message.
	Wrap []int
}

// File is one member of a bundle: a body, at a path, as the scanner produced it.
type File struct {
	// Path is the entry's location inside the archive, with forward slashes -
	// "vulnerabilities/cbur-cbur-agent/1.5.7-alpine-24/xray.json".
	Path string
	// Body is the content, unaltered. This is the whole point of the bundle:
	// what the scanner said, not what this platform made of it.
	Body []byte
}

// Book is a whole export.
type Book struct {
	Sheets []Sheet
	// JSON is what the JSON encoding emits instead of the grid.
	//
	// Deliberately a separate field rather than a serialization of the rows. A
	// JSON export exists so a machine can consume the RELATIONSHIPS - which
	// finding belongs to which artifact in which release - and a flattened grid
	// has already thrown those away. Flattening for a spreadsheet is a lossy
	// projection, and offering it as the JSON would be offering the loss as the
	// feature.
	JSON any
	// Files are the bundle's members, for the ZIP encoding. Ignored by every
	// other format - a spreadsheet has nowhere to put a forty-megabyte SBOM.
	Files []File
}

// Write encodes a book in one format.
func Write(w io.Writer, format Format, book Book) error {
	switch format {
	case FormatJSON:
		return WriteJSON(w, book)
	case FormatXLSX:
		return WriteXLSX(w, book)
	case FormatZIP:
		return WriteZIP(w, book)
	default:
		return WriteCSV(w, book)
	}
}

// WriteJSON emits the structured payload, or the grid when there is none.
func WriteJSON(w io.Writer, book Book) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if book.JSON != nil {
		return enc.Encode(book.JSON)
	}
	// Fallback: the grid as objects, so a JSON export is never empty just
	// because a caller did not supply a structured payload.
	out := make(map[string][]map[string]string, len(book.Sheets))
	for _, sheet := range book.Sheets {
		rows := make([]map[string]string, 0, len(sheet.Rows))
		for _, row := range sheet.Rows {
			obj := make(map[string]string, len(sheet.Headers))
			for i, h := range sheet.Headers {
				if i < len(row) {
					obj[h] = row[i]
				}
			}
			rows = append(rows, obj)
		}
		out[sheet.Name] = rows
	}
	return enc.Encode(out)
}

// WriteCSV emits the PRIMARY sheet.
//
// One sheet, because a CSV holding two tables one after another is a file no
// tool can read. The primary one rather than the last, because a book now
// carries four data tables and "the last one assembled" is not a thing a reader
// asked for - see Sheet.Primary.
func WriteCSV(w io.Writer, book Book) error {
	sheet, ok := primarySheet(book)
	if !ok {
		return nil
	}

	// A UTF-8 BOM, because Excel opens a BOM-less CSV as the system code page
	// and renders every non-ASCII package name as mojibake. Harmless everywhere
	// else - Go, Python, pandas and every spreadsheet skip it.
	if _, err := io.WriteString(w, "\xef\xbb\xbf"); err != nil {
		return err
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(sheet.Headers); err != nil {
		return err
	}
	for _, row := range sheet.Rows {
		if err := cw.Write(pad(row, len(sheet.Headers))); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// primarySheet is the sheet a single-table format should write.
func primarySheet(book Book) (Sheet, bool) {
	if len(book.Sheets) == 0 {
		return Sheet{}, false
	}
	for _, sheet := range book.Sheets {
		if sheet.Primary {
			return sheet, true
		}
	}
	// No sheet claimed it. The first is the better guess than the last: a book
	// is assembled with the table somebody asked for at the front.
	return book.Sheets[0], true
}

// WriteZIP emits the evidence bundle.
//
// # The layout, and why it is by KIND then by image
//
//	README.txt
//	tables/unique-cves.csv, all-findings.csv, images.csv, ...
//	vulnerabilities/<image>__<tag>/<scanner>.json
//	malware/<image>__<tag>/<scanner>.json
//	policy/<image>__<tag>/<scanner>.json
//	sbom/<image>__<tag>/<scanner>.json
//
// Kind first because that is how the bundle is consumed: somebody forwarding a
// vulnerability report to a customer sends `vulnerabilities/`, and somebody
// answering "is there malware in this release" opens one directory and reads
// four files. Image first would put the answer to that question in 157 places.
//
// The tables come too, as CSV rather than as one spreadsheet, because a
// directory of CSVs is diffable, greppable and openable by everything - and the
// person who wants a workbook exports a workbook.
func WriteZIP(w io.Writer, book Book) error {
	zw := zip.NewWriter(w)

	for _, sheet := range book.Sheets {
		name := "tables/" + slugify(sheet.Name) + ".csv"
		f, err := zw.Create(name)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if err := WriteCSV(f, Book{Sheets: []Sheet{sheet}}); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	for _, file := range book.Files {
		f, err := zw.Create(file.Path)
		if err != nil {
			return fmt.Errorf("create %s: %w", file.Path, err)
		}
		if _, err := f.Write(file.Body); err != nil {
			return fmt.Errorf("write %s: %w", file.Path, err)
		}
	}

	return zw.Close()
}

// slugify turns a sheet name into a filename that survives every filesystem.
func slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "table"
	}
	return out
}

// WriteXLSX emits a minimal Office Open XML workbook.
func WriteXLSX(w io.Writer, book Book) error {
	sheets := book.Sheets
	if len(sheets) == 0 {
		sheets = []Sheet{{Name: "Export"}}
	}

	zw := zip.NewWriter(w)

	if err := writeZipFile(zw, "[Content_Types].xml", contentTypes(len(sheets))); err != nil {
		return err
	}
	if err := writeZipFile(zw, "_rels/.rels", rootRels); err != nil {
		return err
	}
	if err := writeZipFile(zw, "xl/workbook.xml", workbookXML(sheets)); err != nil {
		return err
	}
	if err := writeZipFile(zw, "xl/_rels/workbook.xml.rels", workbookRels(len(sheets))); err != nil {
		return err
	}
	if err := writeZipFile(zw, "xl/styles.xml", stylesXML); err != nil {
		return err
	}
	for i, sheet := range sheets {
		name := fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		if err := writeZipSheet(zw, name, sheet); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeZipFile(zw *zip.Writer, name, body string) error {
	f, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	_, err = io.WriteString(f, body)
	return err
}

// writeZipSheet streams one worksheet rather than building it in memory, which
// is the whole reason this file exists rather than a library call.
func writeZipSheet(zw *zip.Writer, name string, sheet Sheet) error {
	f, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}

	// The rows above the header, decided first, because the FREEZE has to name
	// the row below them and a pane frozen at the wrong row is worse than none.
	preamble := 0
	if sheet.Title != "" {
		preamble += 2
	}
	if sheet.Note != "" {
		preamble += 2
	}
	headerRow := preamble + 1

	if _, err := io.WriteString(f, xmlHeader+
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`); err != nil {
		return err
	}
	// Order matters to Excel: sheetPr, dimension, sheetViews, sheetFormatPr,
	// cols, sheetData. A part out of order is a workbook Excel offers to repair.
	if len(sheet.Headers) > 0 {
		// The header stays on screen at row 400 of a findings sheet. Without
		// it, scrolling a real export means losing which column is which -
		// which is the single most common thing people fix by hand.
		if _, err := fmt.Fprintf(f,
			`<sheetViews><sheetView workbookViewId="0"><pane ySplit="%d" topLeftCell="A%d" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`,
			headerRow, headerRow+1); err != nil {
			return err
		}
	}
	if cols := columnsXML(sheet); cols != "" {
		if _, err := io.WriteString(f, cols); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(f, `<sheetData>`); err != nil {
		return err
	}

	rowNum := 1
	if sheet.Title != "" {
		if err := writeStyledRow(f, rowNum, []string{sheet.Title}, styleTitle, styleTitle, nil); err != nil {
			return err
		}
		rowNum += 2
	}
	if sheet.Note != "" {
		if err := writeRow(f, rowNum, []string{sheet.Note}); err != nil {
			return err
		}
		// A blank row under it, so a reader's "select the header row" reflex
		// and every importer's header sniffing both land on the headers rather
		// than on a sentence.
		rowNum += 2
	}
	if len(sheet.Headers) > 0 {
		if err := writeStyledRow(f, rowNum, sheet.Headers, styleHeader, styleHeader, nil); err != nil {
			return err
		}
		rowNum++
	}
	// A field/value grid gets its labels in bold. Every other sheet is a table
	// somebody sorts, and bolding its first column would imply a hierarchy the
	// rows do not have.
	firstColumn := styleGeneral
	if isFieldValue(sheet) {
		firstColumn = styleLabel
	}
	wrapped := map[int]bool{}
	for _, col := range sheet.Wrap {
		wrapped[col] = true
	}
	for _, row := range sheet.Rows {
		if err := writeStyledRow(f, rowNum, pad(row, len(sheet.Headers)),
			firstColumn, styleGeneral, wrapped); err != nil {
			return err
		}
		rowNum++
	}

	_, err = io.WriteString(f, `</sheetData>`+autoFilterXML(sheet, headerRow)+`</worksheet>`)
	return err
}

// isFieldValue recognises a two-column Field/Value grid, which is read rather
// than filtered and so is laid out differently.
func isFieldValue(sheet Sheet) bool {
	return len(sheet.Headers) == 2 &&
		strings.EqualFold(sheet.Headers[0], "Field") &&
		strings.EqualFold(sheet.Headers[1], "Value")
}

// autoFilterXML puts the filter dropdowns on the header row.
//
// Not on a field/value grid: a filter on a column of labels is a control that
// can only hide the thing the reader came to read.
func autoFilterXML(sheet Sheet, headerRow int) string {
	if len(sheet.Headers) == 0 || isFieldValue(sheet) || len(sheet.Rows) == 0 {
		return ""
	}
	return fmt.Sprintf(`<autoFilter ref="A%d:%s%d"/>`,
		headerRow, columnName(len(sheet.Headers)-1), headerRow+len(sheet.Rows))
}

// columnsXML sets the column widths.
//
// The default is eight characters, which renders a digest as `#######` and
// every CVE as `CVE-202…`. A width per column that is named, and one derived
// from the header for the rest - bounded, because a Description column sized to
// its longest cell would be four hundred characters wide.
func columnsXML(sheet Sheet) string {
	if len(sheet.Headers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<cols>`)
	for i, header := range sheet.Headers {
		width := 0
		if i < len(sheet.Widths) {
			width = sheet.Widths[i]
		}
		if width <= 0 {
			// Room for the header plus its filter arrow. Wrong for the content
			// as often as not, and still an enormous improvement on eight.
			width = min(max(len(header)+6, 12), 40)
		}
		fmt.Fprintf(&b, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, width)
	}
	b.WriteString(`</cols>`)
	return b.String()
}

// columnName renders a zero-based column index as "A", "Z", "AA".
func columnName(col int) string {
	name := ""
	for col >= 0 {
		name = string(rune('A'+col%26)) + name
		col = col/26 - 1
	}
	return name
}

// writeRow emits one unstyled row.
func writeRow(w io.Writer, rowNum int, cells []string) error {
	return writeStyledRow(w, rowNum, cells, styleGeneral, styleGeneral, nil)
}

// writeStyledRow emits one row, choosing a numeric cell where the value is a
// number, and a style for the first cell that may differ from the rest.
//
// The numeric choice matters: a count written as a string sorts lexically in
// Excel, so 10 comes before 9 and every "sort by vulnerabilities" gives the
// wrong answer.
func writeStyledRow(
	w io.Writer, rowNum int, cells []string, firstStyle, restStyle int, wrapped map[int]bool,
) error {
	// A header row is two lines tall so a wrapped heading is readable; every
	// other row keeps Excel's own height.
	height := ""
	if firstStyle == styleHeader {
		height = ` ht="28" customHeight="1"`
	}
	if _, err := fmt.Fprintf(w, `<row r="%d"%s>`, rowNum, height); err != nil {
		return err
	}
	for i, value := range cells {
		ref := cellRef(i, rowNum)
		style := restStyle
		if i == 0 {
			style = firstStyle
		}
		// Prose wraps, and only where the sheet asked. A header keeps its own
		// style - it already wraps - and so does a title.
		if wrapped[i] && style == styleGeneral {
			style = styleWrapped
		}
		attr := ""
		if style != styleGeneral {
			attr = fmt.Sprintf(` s="%d"`, style)
		}
		// A number keeps its style but never the header's fill: a header cell
		// holding a year would otherwise be right-aligned white-on-slate text
		// among left-aligned headings.
		if isNumeric(value) && style != styleHeader {
			if _, err := fmt.Fprintf(w, `<c r="%s"%s><v>%s</v></c>`, ref, attr, value); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w,
			`<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
			ref, attr, escapeXML(value)); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, `</row>`)
	return err
}

// isNumeric reports whether a value should be written as a number.
//
// Deliberately narrow. A CVE id, a digest and a version string all contain
// digits, and a rule that guessed at them would turn "2024" into a number and
// "1.1.1n" into text in the same column - which is worse than everything being
// text.
func isNumeric(s string) bool {
	if s == "" || len(s) > 18 {
		return false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	return false
}

// cellRef renders a zero-based column index and one-based row as "AB12".
func cellRef(col, row int) string {
	name := ""
	for col >= 0 {
		name = string(rune('A'+col%26)) + name
		col = col/26 - 1
	}
	return name + strconv.Itoa(row)
}

func escapeXML(s string) string {
	var b strings.Builder
	// The control characters below 0x20 are illegal in XML 1.0 even escaped,
	// and a scanner's description occasionally carries one. Dropping them beats
	// producing a workbook Excel refuses to open.
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, s)
	if err := xml.EscapeText(&b, []byte(clean)); err != nil {
		return ""
	}
	return b.String()
}

func pad(row []string, n int) []string {
	if len(row) >= n {
		return row[:max(n, len(row))]
	}
	out := make([]string, n)
	copy(out, row)
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`

const rootRels = xmlHeader + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

func contentTypes(sheets int) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= sheets; i++ {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

func workbookXML(sheets []Sheet) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, sheet := range sheets {
		fmt.Fprintf(&b, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`,
			escapeXML(sheetName(sheet.Name, i)), i+1, i+1)
	}
	b.WriteString(`</sheets></workbook>`)
	return b.String()
}

// sheetName enforces Excel's rules so a workbook opens rather than being
// repaired: at most 31 characters, and none of []:*?/\.
func sheetName(name string, index int) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		name = fmt.Sprintf("Sheet%d", index+1)
	}
	if len(name) > 31 {
		name = name[:31]
	}
	return name
}

func workbookRels(sheets int) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= sheets; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	// The styles part goes last, after the sheets, so the sheet ids stay 1..n
	// and the relationship ids stay aligned with them. Excel does not require
	// that alignment; a human reading the XML very much does.
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, sheets+1)
	b.WriteString(`</Relationships>`)
	return b.String()
}

// The style table, and why there is exactly one of it.
//
// # What a plain grid cost
//
// The workbook opened with every column eight characters wide, the header row
// indistinguishable from the data, and nothing frozen - so scrolling to row 400
// of a findings sheet lost the header, and the first thing anybody did with an
// export was spend two minutes making it readable. That is the difference
// between a file somebody works in and one they copy out of.
//
// # Why the styles are written by hand
//
// Same argument as the rest of this package: an xlsx style table is a hundred
// lines of XML with a fixed shape, and the alternative is a spreadsheet library
// on a product whose purpose is telling people what is in their supply chain.
//
// Five styles, and no more, because every one of them has to earn a number that
// appears in the cell XML:
//
//	0  general        the default, for data
//	1  header         bold, white on slate, for a header row
//	2  title          14pt bold, for a summary sheet's heading
//	3  label          bold, for the left column of a field/value grid
//	4  wrapped        top-aligned and wrapping, for a description column
const stylesXML = xmlHeader + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
	`<fonts count="4">` +
	`<font><sz val="11"/><name val="Calibri"/></font>` +
	`<font><b/><sz val="11"/><color rgb="FFFFFFFF"/><name val="Calibri"/></font>` +
	`<font><b/><sz val="14"/><color rgb="FF1F2933"/><name val="Calibri"/></font>` +
	`<font><b/><sz val="11"/><color rgb="FF1F2933"/><name val="Calibri"/></font>` +
	`</fonts>` +
	`<fills count="3">` +
	`<fill><patternFill patternType="none"/></fill>` +
	`<fill><patternFill patternType="gray125"/></fill>` +
	`<fill><patternFill patternType="solid"><fgColor rgb="FF33475B"/><bgColor indexed="64"/></patternFill></fill>` +
	`</fills>` +
	`<borders count="2">` +
	`<border><left/><right/><top/><bottom/><diagonal/></border>` +
	`<border><left/><right/><top/><bottom style="thin"><color rgb="FFD9DEE3"/></bottom><diagonal/></border>` +
	`</borders>` +
	`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
	`<cellXfs count="5">` +
	`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
	`<xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1" applyAlignment="1">` +
	`<alignment vertical="center" wrapText="1"/></xf>` +
	`<xf numFmtId="0" fontId="2" fillId="0" borderId="0" xfId="0" applyFont="1"/>` +
	`<xf numFmtId="0" fontId="3" fillId="0" borderId="1" xfId="0" applyFont="1" applyBorder="1"/>` +
	`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1">` +
	`<alignment vertical="top" wrapText="1"/></xf>` +
	`</cellXfs>` +
	`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
	`</styleSheet>`

// The style indexes, matching the cellXfs above.
const (
	styleGeneral = 0
	styleHeader  = 1
	styleTitle   = 2
	styleLabel   = 3
	styleWrapped = 4
)

// Filename builds a download name that says what the file is and when it was
// taken, because a directory of `export.csv` files is a directory of one file.
func Filename(parts []string, format Format, at time.Time) string {
	cleaned := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		p = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				return r
			default:
				return '-'
			}
		}, strings.TrimSpace(p))
		p = strings.Trim(p, "-")
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	cleaned = append(cleaned, at.UTC().Format("20060102-150405"))
	return strings.Join(cleaned, "_") + format.Extension()
}
