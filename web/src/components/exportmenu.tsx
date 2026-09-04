import { useState } from 'react'
import type { ReactNode } from 'react'
import { App, Button, Dropdown, Space, Typography } from 'antd'
import { download } from '../api/client'
import { DownloadOutlined, LoadingOutlined } from '../icons'

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
  /** The URL to fetch. Built by the caller, which owns the filters. */
  href: string
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
  const { message } = App.useApp()

  const start = async (choice: ExportChoice) => {
    if (running) return
    setRunning(choice.key)
    try {
      await download(choice.href)
    } catch (err) {
      message.error(
        err instanceof Error
          ? `${choice.noun} could not be exported: ${err.message}`
          : `${choice.noun} could not be exported`,
      )
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
