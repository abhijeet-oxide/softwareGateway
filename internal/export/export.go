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
	default:
		return "", fmt.Errorf("format must be one of csv, xlsx, json (got %q)", s)
	}
}

// ContentType is the MIME type to serve a format as.
func (f Format) ContentType() string {
	switch f {
	case FormatXLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case FormatJSON:
		return "application/json"
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
}

// Write encodes a book in one format.
func Write(w io.Writer, format Format, book Book) error {
	switch format {
	case FormatJSON:
		return WriteJSON(w, book)
	case FormatXLSX:
		return WriteXLSX(w, book)
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

// WriteCSV emits the LAST sheet.
//
// The last rather than the first, because a book is built summary-first and the
// detailed sheet is the one somebody exporting to CSV wants to work with. A CSV
// holding two tables one after another is a file that no tool can read.
func WriteCSV(w io.Writer, book Book) error {
	if len(book.Sheets) == 0 {
		return nil
	}
	sheet := book.Sheets[len(book.Sheets)-1]

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

	if _, err := io.WriteString(f, xmlHeader+
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`); err != nil {
		return err
	}

	rowNum := 1
	if len(sheet.Headers) > 0 {
		if err := writeRow(f, rowNum, sheet.Headers); err != nil {
			return err
		}
		rowNum++
	}
	for _, row := range sheet.Rows {
		if err := writeRow(f, rowNum, pad(row, len(sheet.Headers))); err != nil {
			return err
		}
		rowNum++
	}

	_, err = io.WriteString(f, `</sheetData></worksheet>`)
	return err
}

// writeRow emits one row, choosing a numeric cell where the value is a number.
//
// The choice matters: a count written as a string sorts lexically in Excel, so
// 10 comes before 9 and every "sort by vulnerabilities" gives the wrong answer.
func writeRow(w io.Writer, rowNum int, cells []string) error {
	if _, err := fmt.Fprintf(w, `<row r="%d">`, rowNum); err != nil {
		return err
	}
	for i, value := range cells {
		ref := cellRef(i, rowNum)
		if isNumeric(value) {
			if _, err := fmt.Fprintf(w, `<c r="%s"><v>%s</v></c>`, ref, value); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
			ref, escapeXML(value)); err != nil {
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
	b.WriteString(`</Relationships>`)
	return b.String()
}

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
