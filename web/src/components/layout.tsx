import type { ReactNode } from 'react'
import {
  Alert, Button, Card, Empty, Input, Popover, Space, Steps, Tag, Timeline, Tooltip, Typography,
} from 'antd'
import {
  ArrowLeftOutlined, CheckCircleOutlined, ClockCircleOutlined, LoadingOutlined, RocketOutlined,
  SearchOutlined, ShopOutlined,
} from '@ant-design/icons'
import { Link } from 'react-router-dom'
import type { LifecycleStep } from '../domain/derive'
import { formatAbsolute } from '../domain/format'
import { semantic } from '../theme'
import { NA } from './value'

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
  title, description, extra, meta, back,
}: {
  title: string
  description: string
  extra?: ReactNode
  meta?: ReactNode
  /**
   * Where a drill-down came from.
   *
   * Detail pages are reached from a list and have no nav entry of their own, so
   * without this the only way back is the browser button — and a page opened
   * from a link has no back to press. One link, above the title, naming the
   * list rather than saying "back".
   */
  back?: { to: string; label: string }
}) {
  return (
    <div
      style={{
        display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between',
        gap: 16, marginBottom: 16, flexWrap: 'wrap',
      }}
    >
      <div>
        {back && (
          <Link to={back.to} style={{ fontSize: 13 }}>
            <ArrowLeftOutlined style={{ fontSize: 11, marginInlineEnd: 6 }} />
            {back.label}
          </Link>
        )}
        <Typography.Title level={3} style={{ margin: 0 }}>{title}</Typography.Title>
        <Typography.Text type="secondary">{description}</Typography.Text>
      </div>
      {/*
        Pinned right even after wrapping. `space-between` puts this block on the
        right while both fit on one line and hard against the LEFT margin the
        moment they do not — so a page with a long title had its elapsed, ETA
        and speed jump to the opposite side at one particular window width.
      */}
      <Space size={12} wrap style={{ marginInlineStart: 'auto' }}>
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
 * The same lifecycle, as ONE CELL.
 *
 * A stepper is the right shape on a release page, where the reader is looking
 * at one thing and the stages are the subject. In a table it is four columns'
 * worth of furniture repeated down the page: it makes every row look busy and
 * the column that actually differs — which stage this release is AT — is the
 * hardest part of it to read.
 *
 * So the cell states the stage, and the timeline moves to a popover. The dates
 * are still one gesture away, and they arrive with more room than a table cell
 * ever had: every stage, whether it was reached, and when.
 */
export function LifecycleCell({ steps }: { steps: LifecycleStep[] }) {
  const reached = steps.filter((s) => s.reached)
  const stage = steps.find((s) => s.current) ?? reached[reached.length - 1] ?? steps[0]
  if (!stage) return <NA reason="This release has no lifecycle recorded." />
  const mark = STAGE_MARKS[stage.stage] ?? { icon: <ClockCircleOutlined />, colour: 'default' }

  return (
    <Popover
      placement="left"
      title={`Lifecycle — ${stage.stage}`}
      content={
        <div style={{ minWidth: 260 }}>
          <Timeline
            style={{ marginTop: 8 }}
            items={steps.map((s) => ({
              color: s.current ? 'blue' : s.reached ? 'green' : 'gray',
              children: (
                <Space direction="vertical" size={0}>
                  <span style={{ fontWeight: s.current ? 600 : 400 }}>{s.stage}</span>
                  {s.at ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {formatAbsolute(s.at)}
                    </Typography.Text>
                  ) : (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }} italic>
                      {s.reached ? 'no time recorded' : 'not reached'}
                    </Typography.Text>
                  )}
                </Space>
              ),
            }))}
          />
        </div>
      }
    >
      <Tag color={mark.colour} style={{ marginInlineEnd: 0, cursor: 'default' }}>
        <Space size={4}>
          {mark.icon}
          {stage.stage}
        </Space>
      </Tag>
    </Popover>
  )
}

/**
 * One mark per stage. The icon is the stage's meaning, not decoration: a
 * release at the vendor has not moved, one downloading is in motion, one
 * downloaded is at rest with us, one in production has shipped.
 */
const STAGE_MARKS: Record<string, { icon: ReactNode; colour: string }> = {
  Vendor: { icon: <ShopOutlined />, colour: 'default' },
  Downloading: { icon: <LoadingOutlined />, colour: 'processing' },
  Downloaded: { icon: <CheckCircleOutlined />, colour: 'green' },
  Production: { icon: <RocketOutlined />, colour: 'purple' },
}

/**
 * The filter that needs no explanation.
 *
 * Deliberately CLIENT-SIDE, and deliberately over one page of results. The API
 * filters packages by tag exactly, which is the wrong shape for somebody typing
 * `2604` into a list of forty releases — and a server round trip per keystroke
 * would be the wrong shape for a table already in the browser.
 *
 * It sits between the subtitle and the table, where the eye lands after reading
 * what the page is for, and it says what it searches so nobody wonders why a
 * match they expected is missing.
 */
export function SearchBar({
  value, onChange, placeholder, matched, total, width = 320,
}: {
  value: string
  onChange: (value: string) => void
  placeholder: string
  /** Rows after filtering, so the bar can say what it did. */
  matched?: number
  total?: number
  width?: number
}) {
  const filtering = value.trim().length > 0
  return (
    <Space size={12} style={{ marginBottom: 12 }} wrap>
      <Input
        allowClear
        style={{ width }}
        prefix={<SearchOutlined style={{ color: 'rgba(0,0,0,.35)' }} />}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      {filtering && matched !== undefined && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {matched} of {total} shown
        </Typography.Text>
      )}
    </Space>
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
