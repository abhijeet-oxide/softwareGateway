import { useState } from 'react'
import type { ReactNode } from 'react'
import { Button, Dropdown, Space, Typography } from 'antd'
import { download } from '../api/client'
import { exportRows, type RowColumn, type RowFormat } from '../domain/exportrows'
import { DownloadOutlined, FileCodeOutlined, FileCsvOutlined, FileTextOutlined, LoadingOutlined } from '../icons'
import { reportFailure } from './feedback'

/**
 * The control that turns a screen into a file.
 *
 * # Why one component for two tabs
 *
 * Because a download is the same interaction wherever it appears - pick a
 * shape, wait, get a named file - and it was written twice: once for the
 * security export and once for the compliance report. Two copies is two places
 * for the loading state to behave differently, and the reader who learns one is
 * the reader who meets the other one tab later.
 *
 * # Why the format is a menu and not a button
 *
 * Because the formats are not variations on one file, they are different files
 * for different readers. A workbook is for somebody working through the
 * findings; a CSV is one table for somebody pasting it into their own sheet; a
 * bundle is for somebody FORWARDING the evidence. A single button would have to
 * pick one and be wrong two thirds of the time.
 *
 * # Why the failure is spoken
 *
 * A download that fails silently is indistinguishable from one the browser is
 * still thinking about, and the reader waits.
 */
export interface ExportChoice {
  /** The `format` the endpoint takes, and this item's identity in the menu. */
  key: string
  icon: ReactNode
  label: string
  /** What this file actually holds, in one line under the label. */
  note: string
  /**
   * The URL to fetch. Built by the caller, which owns the filters.
   *
   * Omitted when the file is written in the browser instead - see `run`.
   */
  href?: string
  /**
   * Write the file HERE rather than fetching one.
   *
   * Some tables already hold everything the file should contain, filtered the
   * way the reader filtered them, so a round trip could only return the same
   * rows with a different provenance. Those pass `run` and no `href`; see
   * domain/exportrows.ts. Everything else about the control is identical,
   * deliberately: which side of the wire a file is written on is not something
   * a reader should have to notice.
   */
  run?: () => void | Promise<void>
  /** How the failure names the file: "The workbook could not be exported". */
  noun: string
}

export function ExportMenu({ choices, label = 'Export', disabled, icon }: {
  choices: ExportChoice[]
  label?: string
  disabled?: boolean
  icon?: ReactNode
}) {
  const [running, setRunning] = useState<string | null>(null)

  const start = async (choice: ExportChoice) => {
    if (running) return
    setRunning(choice.key)
    try {
      if (choice.run) await choice.run()
      else if (choice.href) await download(choice.href)
    } catch (err) {
      // Through the one reporting path, so an export refused for want of
      // `security_report.export` reads as a refusal naming the permission
      // rather than as "could not be exported".
      reportFailure(err, `Export ${choice.noun.toLowerCase()}`)
    } finally {
      setRunning(null)
    }
  }

  const items = choices.map((choice) => ({
    key: choice.key,
    icon: running === choice.key ? <LoadingOutlined /> : choice.icon,
    label: (
      <Space direction="vertical" size={0}>
        <Typography.Text>{choice.label}</Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          {choice.note}
        </Typography.Text>
      </Space>
    ),
    onClick: () => void start(choice),
  }))

  return (
    <Dropdown menu={{ items }} disabled={disabled || Boolean(running)} trigger={['click']}>
      <Button icon={running ? <LoadingOutlined /> : icon ?? <DownloadOutlined />} disabled={disabled}>
        {running ? 'Preparing…' : label}
      </Button>
    </Dropdown>
  )
}

/**
 * The three formats, worded and iconed the same way everywhere.
 *
 * Built once so the Contents tab, the policy catalogue and the source list
 * cannot drift into three slightly different menus of the same three files -
 * which is exactly what happened to the export before there was a shared
 * component at all.
 *
 * `noun` is what the failure calls the file: "The spreadsheet could not be
 * exported". `subject` is what it holds - "the 148 images in this release" -
 * and it goes in the small line under each label, because "CSV" says the
 * format and nothing about the contents.
 */
export function rowExportChoices<T>(
  rows: readonly T[],
  columns: RowColumn<T>[],
  fileName: string,
  subject: string,
): ExportChoice[] {
  const run = (format: RowFormat) => () => exportRows(rows, columns, fileName, format)
  return [
    {
      key: 'csv',
      icon: <FileCsvOutlined />,
      label: 'CSV',
      note: `${subject}, one row each`,
      noun: 'The CSV',
      run: run('csv'),
    },
    {
      key: 'xlsx',
      icon: <FileTextOutlined />,
      label: 'Excel workbook',
      note: 'The same table, ready to open in a spreadsheet',
      noun: 'The workbook',
      run: run('xlsx'),
    },
    {
      key: 'json',
      icon: <FileCodeOutlined />,
      label: 'JSON',
      note: 'The same table, for a script or another tool',
      noun: 'The JSON file',
      run: run('json'),
    },
  ]
}
