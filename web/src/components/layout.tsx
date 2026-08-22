import { useState } from 'react'
import type { ReactNode } from 'react'
import {
  Alert, Button, Card, Empty, Input, Popover, Skeleton, Space, Tag, Timeline, Tooltip,
  Typography,
} from 'antd'
import {
  ArrowLeftOutlined, CheckCircleOutlined, ClockCircleOutlined, LoadingOutlined, RocketOutlined,
  SearchOutlined, ShopOutlined,
} from '@ant-design/icons'
import PartialIcon from '@iconify-react/oui/partial';
import { Link } from 'react-router-dom'
import type { ContentGroup, PresentComponent } from '../api/types'
import { kindName, type LifecycleStep } from '../domain/derive'
import { bytes, formatAbsolute, formatBytes, formatCount } from '../domain/format'
import { ARTIFACT_ICONS, Icon } from './icons'
import { usePresentComponents } from '../api/queries'
import { mono, palette, semantic } from '../theme'
import { NA } from './value'

/**
 * Page furniture shared by all ten pages.
 *
 * The rules these encode, from the brief §4:
 *   - every page answers ONE question, and its primary action is top right;
 *   - an empty state explains what will appear and offers the button that
 *     makes it appear - never an illustration and a shrug;
 *   - failures surface in the SAME BAND on every page, so there is one place
 *     to learn to look.
 */

export function PageHeader({
  title, description, extra, meta, back,
}: {
  /*
   * OPTIONAL, and absent on every list page.
   *
   * A page called Packages, reached by clicking Packages, under a heading that
   * says Packages, with a sentence under that explaining what packages are, is
   * three restatements of the nav item that got somebody here. The pages that
   * keep a title are the ones whose title is an IDENTITY - which release, which
   * download - because that is a fact about the thing on screen rather than a
   * label for the screen.
   */
  title?: string
  description?: string
  extra?: ReactNode
  meta?: ReactNode
  /**
   * Where a drill-down came from.
   *
   * Detail pages are reached from a list and have no nav entry of their own, so
   * without this the only way back is the browser button - and a page opened
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
        {title && <Typography.Title level={3} style={{ margin: 0 }}>{title}</Typography.Title>}
        {description && <Typography.Text type="secondary">{description}</Typography.Text>}
      </div>
      {/*
        Pinned right even after wrapping. `space-between` puts this block on the
        right while both fit on one line and hard against the LEFT margin the
        moment they do not - so a page with a long title had its elapsed, ETA
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
  /** What it means and what to do - never just what broke. */
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
 * The two dates a release actually has.
 *
 * # Why this is two entries and not four
 *
 * It was a four-stage stepper - Vendor, Downloading, Downloaded, Production -
 * across the full width of the page, and it was wrong in both directions at
 * once. It took a band of the release page to say almost nothing, and it
 * reported "Downloaded" as reached on releases nobody had downloaded, because
 * two of its four stages are STATES rather than events and had no date to
 * stand on. A stage with no timestamp cannot be checked or unchecked honestly;
 * it can only be guessed at.
 *
 * So the page keeps the two moments that are facts with instants attached:
 * when the vendor published it, and when it finished arriving here. Everything
 * else the stepper was gesturing at - is it downloading, is it in production -
 * is already said, once, by the status badge in the header.
 *
 * A release nobody has downloaded shows ONE date and stops there. It was
 * showing an empty second entry, which drew the eye to a stage that has not
 * happened and reads as a gap in the record rather than as the ordinary state
 * of most of a catalogue. There is one date; the timeline says one date.
 */
export function ReleaseTimeline({
  publishedAt, downloadedAt, downloading,
}: {
  publishedAt?: string
  downloadedAt?: string
  /** A download is running right now, so the second date is coming. */
  downloading?: boolean
}) {
  const arriving = Boolean(downloadedAt) || Boolean(downloading)

  return (
    <Space size={12} align="center" wrap>
      <Moment label="Published" at={publishedAt} done />
      {arriving && (
        <>
          <span
            aria-hidden
            style={{
              width: 48, height: 1, background: 'rgba(0,0,0,0.15)', display: 'inline-block',
            }}
          />
          <Moment
            label="Downloaded"
            at={downloadedAt}
            done={Boolean(downloadedAt)}
            pending={downloading}
          />
        </>
      )}
    </Space>
  )
}

/** One dated moment: a mark, what happened, and when. */
function Moment({
  label, at, done, pending,
}: {
  label: string
  at?: string
  done?: boolean
  pending?: boolean
}) {
  const colour = done ? semantic.success : pending ? palette.primary : 'rgba(0,0,0,0.25)'

  return (
    <Space size={8} align="center">
      {pending && !done ? (
        <LoadingOutlined style={{ color: colour, fontSize: 12 }} />
      ) : (
        <span
          aria-hidden
          style={{
            width: 8, height: 8, borderRadius: '50%', display: 'inline-block',
            background: done ? colour : 'transparent',
            border: done ? 'none' : `1px solid ${colour}`,
          }}
        />
      )}
      <Space size={6} align="center">
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>{label}</Typography.Text>
        {at ? (
          <Typography.Text style={{ fontSize: 13 }}>{formatAbsolute(at)}</Typography.Text>
        ) : (
          <Typography.Text type="secondary" style={{ fontSize: 13 }} italic>
            {pending ? 'in progress' : 'not recorded'}
          </Typography.Text>
        )}
      </Space>
    </Space>
  )
}

/**
 * The same lifecycle, as ONE CELL.
 *
 * A stepper is four columns' worth of furniture repeated down the page: it makes every row look busy and
 * the column that actually differs - which stage this release is AT - is the
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
      title={`Lifecycle - ${stage.stage}`}
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
 * `2604` into a list of forty releases - and a server round trip per keystroke
 * would be the wrong shape for a table already in the browser.
 *
 * It sits between the subtitle and the table, where the eye lands after reading
 * what the page is for, and it says what it searches so nobody wonders why a
 * match they expected is missing.
 */
export function SearchBar({
  value, onChange, placeholder, matched, total, width = 320, style,
}: {
  value: string
  onChange: (value: string) => void
  placeholder: string
  /** Rows after filtering, so the bar can say what it did. */
  matched?: number
  total?: number
  width?: number
  /** Override the default bottom margin, e.g. when sharing a row with other filters. */
  style?: React.CSSProperties
}) {
  const filtering = value.trim().length > 0
  return (
    <Space size={12} style={{ marginBottom: 12, ...style }} wrap>
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
 * "Saved (already present)" - the system optimising on the user's behalf.
 *
 * Lives on the release and download pages and deliberately NOT on Home: it is
 * a fact about a release, not about the estate.
 */
export function SavedPanel({
  savedBytes, totalBytes, artifactsSkipped, estimated, content, transferId,
}: {
  savedBytes: string
  totalBytes?: string
  artifactsSkipped?: number
  /** Before a download this is a projection and must say so. */
  estimated?: boolean
  /** What the transfer is made of, which is what the saving is broken down by. */
  content?: ContentGroup[]
  /** Whose components to name. Absent before a download exists. */
  transferId?: string
}) {
  return (
    <Card size="small" style={{ background: '#F3F8F4', borderColor: '#CFE4D6' }}>
      <Space direction="vertical" size={4} style={{ width: '100%' }}>
        <Space size={8}>
          <SavedBreakdown transferId={transferId} content={content}>
            <Typography.Text
              strong
              style={{ color: semantic.success, borderBottom: '1px dotted #9BC7A9' }}
            >
              Saved {savedBytes}
            </Typography.Text>
          </SavedBreakdown>
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

/**
 * WHAT was already there - by name, because the number alone is not checkable.
 *
 * # The question
 *
 * "Saved 56.5 GB" is the system's best claim about itself and an operator
 * cannot check it. What they ask next is which things were already at the
 * destination - and that is answerable exactly, because every skipped job is
 * attached to the artifact it belongs to.
 *
 * So this names them: `cfx-5000-product/bgcf:2511.174.0`, what it weighs, what
 * kind of thing it is. Grouped by kind because that is how somebody reads it -
 * a hundred and fifty images already there is one fact, not a hundred and
 * fifty - and the largest first, because they are the explanation.
 *
 * # Why it is fetched only on hover
 *
 * The transfer itself is polled every couple of seconds while a download runs.
 * This is hundreds of rows that change only when a job settles, so it is a
 * separate request made when somebody actually asks.
 */
export function SavedBreakdown({
  transferId, content, children,
}: {
  transferId?: string
  /** The per-kind totals, which are on the transfer already and need no fetch. */
  content?: ContentGroup[]
  children: ReactNode
}) {
  const [open, setOpen] = useState(false)
  const present = usePresentComponents(transferId, open)

  const kinds = (content ?? [])
    .map((g) => ({ group: g, saved: bytes(g.savedBytes) ?? 0 }))
    .filter((k) => k.saved > 0)
    .sort((a, b) => b.saved - a.saved)

  if (kinds.length === 0 && !transferId) return <>{children}</>

  return (
    <Popover
      placement="bottomLeft"
      open={open}
      onOpenChange={setOpen}
      title="Already at the destination"
      styles={{ body: { maxHeight: 420, overflow: 'auto' } }}
      content={
        <Space
          direction="vertical"
          size={12}
          style={{ width: 'min(520px, calc(100vw - 32px))', maxWidth: '100%' }}
        >
          {kinds.length > 0 && (
            <Space direction="vertical" size={4} style={{ width: '100%' }}>
              {kinds.map(({ group, saved }) => (
                <KindLine key={group.kind} group={group} saved={saved} />
              ))}
            </Space>
          )}

          {present.isLoading ? (
            <Skeleton active paragraph={{ rows: 3 }} title={false} />
          ) : (
            <PresentList components={present.data?.components ?? []} />
          )}
        </Space>
      }
    >
      <span style={{ cursor: 'help' }}>{children}</span>
    </Popover>
  )
}

/** One kind's line: what it is, how much of it was there, and what that saved. */
function KindLine({ group, saved }: { group: ContentGroup; saved: number }) {
  const name = kindName(group.kind)
  const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      {icon && <Icon as={icon} size={14} title={name} />}
      <Typography.Text style={{ fontSize: 12, flex: 1 }}>{name}</Typography.Text>
      {/*
        COMPONENTS on the left, BYTES on the right, and they do not have to
        agree. A kind can save bytes with no component wholly present - a
        release that changed five images still shares most of their layers with
        the release before it.
      */}
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {group.present > 0
          ? `${group.present} of ${group.total} already there`
          : 'shared layers'}
      </Typography.Text>
      <Typography.Text strong style={{ fontSize: 12, color: semantic.success }}>
        {formatBytes(saved)}
      </Typography.Text>
    </div>
  )
}

/** The components themselves, named, biggest first. */
function PresentList({ components }: { components: PresentComponent[] }) {
  if (components.length === 0) {
    return (
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        The destination has none of this release's content yet. What it holds is
        established while the download plans and runs.
      </Typography.Text>
    )
  }

  // The whole list is not the point - a release is two hundred and sixty
  // components and a hover is not a table. The heaviest are the explanation,
  // and the rest are counted rather than hidden.
  const shown = components.slice(0, 12)
  const rest = components.slice(12)
  const restBytes = rest.reduce((n, c) => n + (bytes(c.bytes) ?? 0), 0)

  return (
    <Space direction="vertical" size={2} style={{ width: '100%' }}>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        The destination already holds these
      </Typography.Text>
      {shown.map((c) => (
        <div
          key={c.digest}
          style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}
        >
          <Typography.Text
            style={{ minWidth: 0, flex: 1, fontSize: 12, fontFamily: c.name ? undefined : mono }}
            ellipsis={{ tooltip: c.name || c.digest }}
          >
            {c.name || c.digest.slice(0, 19)}
          </Typography.Text>
          {c.partial && (
            <Tooltip title="Part of this component was already there; the rest is still being moved.">
              <Tag
                style={{ marginInlineEnd: 0, flex: '0 0 auto', fontSize: 10 }}
                icon={<PartialIcon style={{ width: '1em', height: '1em' }} />}
              >
                partly
              </Tag>
            </Tooltip>
          )}
          <Typography.Text type="secondary" style={{ flex: '0 0 auto', fontSize: 11 }}>
            {formatBytes(bytes(c.bytes))}
          </Typography.Text>
        </div>
      ))}
      {rest.length > 0 && (
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          and {formatCount(rest.length)} more, {formatBytes(restBytes)}
        </Typography.Text>
      )}
    </Space>
  )
}
