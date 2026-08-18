import type { ReactNode } from 'react'
import { Alert, Button, Card, Empty, Space, Steps, Tooltip, Typography } from 'antd'
import { Link } from 'react-router-dom'
import type { LifecycleStep } from '../domain/derive'
import { formatAbsolute } from '../domain/format'
import { semantic } from '../theme'

/**
 * Page furniture shared by all ten pages.
 *
 * The rules these encode, from the brief §4:
 *   - every page answers ONE question, and its primary action is top right;
 *   - an empty state explains what will appear and offers the button that
 *     makes it appear — never an illustration and a shrug;
 *   - failures surface in the SAME BAND on every page, so there is one place
 *     to learn to look.
 */

export function PageHeader({
  title, description, extra, meta,
}: {
  title: string
  description: string
  extra?: ReactNode
  meta?: ReactNode
}) {
  return (
    <div
      style={{
        display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between',
        gap: 16, marginBottom: 16, flexWrap: 'wrap',
      }}
    >
      <div>
        <Typography.Title level={3} style={{ margin: 0 }}>{title}</Typography.Title>
        <Typography.Text type="secondary">{description}</Typography.Text>
      </div>
      <Space size={12} wrap>
        {meta}
        {extra}
      </Space>
    </div>
  )
}

export interface Attention {
  key: string
  message: string
  /** What it means and what to do — never just what broke. */
  detail: string
  action?: { label: string; to?: string; onClick?: () => void }
  severity?: 'error' | 'warning'
}

/**
 * The attention band, directly under the header and shown only when something
 * is failing. At most three items, each with its fix inline.
 */
export function AttentionBand({ items }: { items: Attention[] }) {
  if (items.length === 0) return null
  const shown = items.slice(0, 3)

  return (
    <Space direction="vertical" size={8} style={{ width: '100%', marginBottom: 16 }}>
      {shown.map((item) => (
        <Alert
          key={item.key}
          type={item.severity ?? 'error'}
          showIcon
          message={item.message}
          description={item.detail}
          action={
            item.action &&
            (item.action.to ? (
              <Link to={item.action.to}>
                <Button size="small">{item.action.label}</Button>
              </Link>
            ) : (
              <Button size="small" onClick={item.action.onClick}>{item.action.label}</Button>
            ))
          }
        />
      ))}
      {items.length > shown.length && (
        <Typography.Text type="secondary">
          {items.length - shown.length} more item{items.length - shown.length === 1 ? '' : 's'} need attention.
        </Typography.Text>
      )}
    </Space>
  )
}

/**
 * An empty state that explains and acts.
 *
 * Both props are required on purpose: the brief forbids an empty state with no
 * explanation and no action, and a required prop is a cheaper way to enforce
 * that than a review comment.
 */
export function EmptyStateCard({
  explanation, action, title,
}: {
  title?: string
  explanation: string
  action: ReactNode
}) {
  return (
    <Card>
      <Empty
        image={Empty.PRESENTED_IMAGE_SIMPLE}
        description={
          <Space direction="vertical" size={4}>
            {title && <Typography.Text strong>{title}</Typography.Text>}
            <Typography.Text type="secondary">{explanation}</Typography.Text>
          </Space>
        }
      >
        {action}
      </Empty>
    </Card>
  )
}

/** An error that says what happened, what it means, and what to do. */
export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  const message = error instanceof Error ? error.message : 'Something went wrong.'
  return (
    <Alert
      type="error"
      showIcon
      message="This could not be loaded"
      description={message}
      action={retry && <Button size="small" onClick={retry}>Try again</Button>}
    />
  )
}

/**
 * `Vendor → Downloading → Downloaded → Production`.
 *
 * Compact in tables, expanded with timestamps on the release page — one
 * component, because two would drift.
 */
export function LifecycleIndicator({
  steps, size = 'small',
}: { steps: LifecycleStep[]; size?: 'small' | 'default' }) {
  const current = Math.max(0, steps.findIndex((s) => s.current))

  return (
    <Steps
      size={size === 'small' ? 'small' : 'default'}
      current={current}
      items={steps.map((s) => ({
        title: s.stage,
        status: s.current ? 'process' : s.reached ? 'finish' : 'wait',
        description:
          size === 'default' && s.at ? (
            <Tooltip title={formatAbsolute(s.at)}>
              <span style={{ fontSize: 12 }}>{formatAbsolute(s.at)}</span>
            </Tooltip>
          ) : undefined,
      }))}
    />
  )
}

/**
 * "Saved (already present)" — the system optimising on the user's behalf.
 *
 * Lives on the release and download pages and deliberately NOT on Home: it is
 * a fact about a release, not about the estate.
 */
export function SavedPanel({
  savedBytes, totalBytes, artifactsSkipped, estimated,
}: {
  savedBytes: string
  totalBytes?: string
  artifactsSkipped?: number
  /** Before a download this is a projection and must say so. */
  estimated?: boolean
}) {
  return (
    <Card size="small" style={{ background: '#F3F8F4', borderColor: '#CFE4D6' }}>
      <Space direction="vertical" size={4} style={{ width: '100%' }}>
        <Space size={8}>
          <Typography.Text strong style={{ color: semantic.success }}>
            Saved {savedBytes}
          </Typography.Text>
          {totalBytes && <Typography.Text type="secondary">of {totalBytes}</Typography.Text>}
          {artifactsSkipped !== undefined && (
            <Typography.Text type="secondary">· {artifactsSkipped} artifacts skipped</Typography.Text>
          )}
          {estimated && <Typography.Text type="secondary">· estimate</Typography.Text>}
        </Space>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {estimated
            ? 'Existing artifacts will be detected and skipped, so the download moves less than the release weighs.'
            : 'Existing artifacts are automatically detected and skipped to reduce download time.'}
        </Typography.Text>
      </Space>
    </Card>
  )
}
