/**
 * Turning what is on screen into a file, without asking the server.
 *
 * # Why this exists beside the export on Security and Compliance
 *
 * Those two build a REPORT: the server assembles a workbook or an evidence
 * bundle from more than the screen is showing, so the browser asks for a URL
 * and saves what comes back. A release's contents, the policy catalogue and
 * the source list are different - the rows are already here, already filtered
 * by whatever the reader typed, and a round trip could only return the same
 * table with a different provenance.
 *
 * So the file is written here, and `ExportMenu` renders it with the same
 * button, the same menu and the same spoken failure as the server-backed ones.
 * A reader should not be able to tell which kind they are using.
 *
 * # Why the same three formats every time
 *
 * They are not variations on one file. A CSV is one table for somebody pasting
 * it into their own sheet, a workbook is for somebody working through it, and
 * JSON is for whatever comes next - a script, a ticket, another tool. Offering
 * one and picking wrong is worse than offering three.
 */

/** One column of an exported table: its heading, and how a row fills it. */
export interface RowColumn<T> {
  title: string
  value: (row: T) => string | number | null | undefined
}

export type RowFormat = 'csv' | 'xlsx' | 'json'

function cell<T>(column: RowColumn<T>, row: T): string {
  const v = column.value(row)
  return v === null || v === undefined ? '' : String(v)
}

/** RFC 4180: quote anything containing a comma, a quote or a newline. */
function csvEscape(value: string): string {
  return /[",\n\r]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value
}

function htmlEscape(value: string): string {
  return value
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;')
}

function save(content: BlobPart, fileName: string, type: string): void {
  const url = URL.createObjectURL(new Blob([content], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = fileName
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  // Revoked on the next turn of the loop: revoking synchronously races the
  // browser's own read of the URL and produces an empty file in some builds.
  setTimeout(() => URL.revokeObjectURL(url), 0)
}

/**
 * Write `rows` as `format` and hand it to the browser.
 *
 * The workbook is an HTML table saved as `.xls`, which is what the shared
 * table component already produces: Excel and every other spreadsheet opens
 * it, and it costs no dependency. A real .xlsx would mean shipping a zip
 * writer to every page for a file nobody edits in the browser.
 */
export function exportRows<T>(
  rows: readonly T[],
  columns: RowColumn<T>[],
  fileName: string,
  format: RowFormat,
): void {
  if (format === 'json') {
    const out = rows.map((row) => {
      const o: Record<string, string> = {}
      for (const column of columns) o[column.title] = cell(column, row)
      return o
    })
    save(JSON.stringify(out, null, 2), `${fileName}.json`, 'application/json;charset=utf-8;')
    return
  }

  if (format === 'csv') {
    const lines = [
      columns.map((column) => csvEscape(column.title)).join(','),
      ...rows.map((row) => columns.map((column) => csvEscape(cell(column, row))).join(',')),
    ]
    // A BOM, so Excel opens a UTF-8 CSV as UTF-8 rather than as Latin-1 - which
    // is what turns a vendor's name into mojibake on somebody else's machine.
    save('﻿' + lines.join('\r\n'), `${fileName}.csv`, 'text/csv;charset=utf-8;')
    return
  }

  const html = `<html><head><meta charset="UTF-8" /></head><body><table>`
    + `<thead><tr>${columns.map((c) => `<th>${htmlEscape(c.title)}</th>`).join('')}</tr></thead>`
    + `<tbody>${rows.map((row) =>
      `<tr>${columns.map((c) => `<td>${htmlEscape(cell(c, row))}</td>`).join('')}</tr>`).join('')}</tbody>`
    + `</table></body></html>`
  save(html, `${fileName}.xls`, 'application/vnd.ms-excel;charset=utf-8;')
}
