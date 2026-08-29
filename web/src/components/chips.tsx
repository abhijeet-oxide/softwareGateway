import type { ReactNode } from 'react'
import { Badge, Space, Tag, Tooltip, Typography } from 'antd'
import {
  CheckCircleOutlined, ExportOutlined, ExclamationCircleOutlined,
  CloseCircleOutlined, LoadingOutlined, QuestionCircleOutlined,
} from '../icons'
import { Icon, locationIcon, repositoryIcon, type IconComponent } from './icons'
import { Link } from 'react-router-dom'
import { NA } from './value'
import { releaseHref, type SoftwareStatus, type VerificationState, type Location } from '../domain/derive'
import type { Package, Repository } from '../api/types'
import { formatAbsolute, formatRelative } from '../domain/format'
import { c, mono, StatusPill, type PillTone } from '../uikit'

/**
 * The reusable vocabulary (UI brief §7). Designed once, used literally.
 *
 * Two rules run through all of them:
 *   - every status is stated in WORDS; colour only reinforces;
 *   - no bare icon stands in for a verb, and every icon carries a label or a
 *     tooltip.
 */

/**
 * A product's name.
 *
 * No icon and no link. The icon said "product" beside a column already headed
 * Product, and the link took a reader looking at one download to a page about
 * everything that product has ever done - a way out of the thing they were
 * reading, offered in every row.
 */
export function ProductChip({ name, display }: { name: string; display?: string }) {
  return <span style={{ fontWeight: 500 }}>{display || name}</span>
}

/** A version, always monospace. Clicks through to the release. */
export function VersionChip({
  product, version, pkg, showRepository = true,
}: {
  product: string
  version: string
  /** The release itself, so the link can carry the repository that identifies it. */
  pkg: Pick<Package, 'tag' | 'sourceRepository' | 'displayRepository'>
  /**
   * Whether to repeat the repository under the version. False where the table
   * gives the name a column of its own - the same value in two adjacent cells
   * reads as two different facts.
   */
  showRepository?: boolean
}) {
  // The repository is shown, not just linked. A vendor publishes one version
  // tag into every repository of a product, so a listing shows the same
  // version many times and the rows are only telling apart by where each one
  // came from - without it the page reads as duplicates.
  const repository = showRepository ? pkg.displayRepository || pkg.sourceRepository : undefined

  return (
    <Space direction="vertical" size={0}>
      <Link to={releaseHref(product, pkg)} style={{ fontFamily: mono }}>
        <Typography.Text ellipsis={{ tooltip: version }} style={{ maxWidth: 150 }}>
          {version}
        </Typography.Text>
      </Link>
      {repository && (
        <Tooltip title={pkg.sourceRepository}>
          <Typography.Text type="secondary" style={{ fontSize: 11 }} className="mono">
            {repository}
          </Typography.Text>
        </Tooltip>
      )}
    </Space>
  )
}

/**
 * Where each status sits in the shared design system's tone scale.
 *
 * These used to be the component library's preset tag colours - blue, cyan,
 * purple, green, error - which belong to no palette and follow no theme: the
 * cyan and the purple were the only two of their hue anywhere in the product,
 * and none of the five moved when the palette did.
 *
 * The scale is deliberately narrower than the status list, and the words carry
 * what the colours no longer do:
 *
 *   - AVAILABLE keeps a colour of its own rather than going grey. A release
 *     sitting at the vendor is a real, actionable state - it is the thing
 *     somebody downloads - and grey reads as disabled next to the blue of NEW.
 *   - READY FOR PRODUCTION shares green with PRODUCTION because it has the same
 *     destiny; what says it has not arrived yet is the sentence, not the hue.
 *   - The two in-flight states share one tone AND spin, because being in motion
 *     is the fact they have in common and the spinner is what states it.
 */
const STATUS_TONE: Record<SoftwareStatus, PillTone> = {
  NEW: 'review',
  AVAILABLE: 'accent',
  DOWNLOADING: 'review',
  DOWNLOADED: 'ok',
  'READY FOR PRODUCTION': 'ok',
  PROMOTING: 'review',
  PRODUCTION: 'ok',
  'DOWNLOAD FAILED': 'danger',
  'PROMOTION FAILED': 'danger',
  'VERIFICATION FAILED': 'danger',
}

/**
 * The same statuses, in sentences.
 *
 * The type is SCREAMING CAPS because that is what the lifecycle constants are
 * called in the domain, and renaming them would touch every comparison in the
 * application. What a person reads is a different question from what the code
 * calls it: a row of shouted fragments is what dates an interface fastest, and
 * "Ready for production" is the same fact said the way anybody would say it.
 */
const STATUS_LABEL: Record<SoftwareStatus, string> = {
  NEW: 'New',
  AVAILABLE: 'Available',
  DOWNLOADING: 'Downloading',
  DOWNLOADED: 'Downloaded',
  'READY FOR PRODUCTION': 'Ready for production',
  PROMOTING: 'Promoting',
  PRODUCTION: 'In production',
  'DOWNLOAD FAILED': 'Download failed',
  'PROMOTION FAILED': 'Promotion failed',
  'VERIFICATION FAILED': 'Verification failed',
}

/**
 * What the vendor calls this package - the repository it publishes into.
 *
 * Ellipsised, because a vendor path is long and arbitrary and the end of it is
 * rarely the part that identifies anything; the full value is on the tooltip,
 * so it can still be read in full.
 */
export function PackageName({
  pkg, width = 240,
}: {
  pkg: Pick<Package, 'sourceRepository' | 'displayRepository'>
  width?: number
}) {
  const name = pkg.displayRepository || pkg.sourceRepository
  if (!name) return <NA reason="This package records no source repository." />
  return (
    <Tooltip title={pkg.sourceRepository}>
      <Typography.Text
        style={{ fontFamily: mono, fontSize: 12, maxWidth: width }}
        ellipsis={{ tooltip: false }}
      >
        {name}
      </Typography.Text>
    </Tooltip>
  )
}

/**
 * The statuses and no others.
 *
 * DOWNLOADING and PROMOTING spin, because they are the only ones that describe
 * work happening right now. Everything else is a resting place.
 */
export function StatusBadge({
  status, reason,
}: {
  status: SoftwareStatus
  /**
   * Why, for a status that has a why. The reason is the whole value of a
   * failure: `401 unauthorized` and `the vendor withdrew this component` are
   * the same tag and completely different afternoons.
   */
  reason?: string
}) {
  const moving = status === 'DOWNLOADING' || status === 'PROMOTING'
  const tag = (
    <StatusPill
      tone={STATUS_TONE[status]}
      icon={moving ? <LoadingOutlined /> : undefined}
      style={{ marginInlineEnd: 0 }}
    >
      {STATUS_LABEL[status]}
    </StatusPill>
  )
  return reason ? <Tooltip title={reason}>{tag}</Tooltip> : tag
}

/**
 * Whether anybody is looking INSIDE this release right now.
 *
 * # Why it is a second tag and not a status
 *
 * Being analysed is not a lifecycle stage. A release is NEW or AVAILABLE or
 * DOWNLOADING whether or not somebody happens to be walking its manifest tree
 * this minute, and folding the walk into that word would make a release change
 * status because a background job started.
 *
 * So it sits beside the status and disappears when the walk finishes. What
 * says a release HAS been analysed is its contents being there, which is on the
 * release's own page and is a better answer than a tag.
 *
 * A failure keeps its reason on hover. "Analysis failed" that cannot say why is
 * a dead end for whoever reads it.
 */
export function AnalysisTag({ pkg }: { pkg: { analysisState?: string; analysisError?: string } }) {
  if (pkg.analysisState === 'analyzing') {
    return (
      <Tooltip title="Reading this release's manifest tree from the vendor registry, so its contents, sizes and files can be shown without asking again.">
        <StatusPill tone="review" icon={<LoadingOutlined />} style={{ marginInlineEnd: 0 }}>
          Analyzing
        </StatusPill>
      </Tooltip>
    )
  }
  if (pkg.analysisState === 'failed') {
    return (
      <Tooltip title={pkg.analysisError || 'The walk did not finish and gave no reason.'}>
        <StatusPill tone="danger" style={{ marginInlineEnd: 0 }}>Analysis failed</StatusPill>
      </Tooltip>
    )
  }
  return null
}

/**
 * A transfer's state, and whether it is MOVING.
 *
 * The spinner is the point. `RUNNING` and `SUCCEEDED` are two words of similar
 * length in two similar tags, and on a page somebody is watching precisely
 * because they want to know whether anything is happening, the difference has
 * to be visible without reading. A spinning mark says "this is live" from
 * across the room; a static one says the opposite just as clearly.
 *
 * `CANCELLING` spins too, and deliberately: it is not finished stopping. Jobs
 * a worker holds run to their next checkpoint, and a still tag there would
 * report a transfer as settled while bytes were still moving.
 */
const LIVE_STATES = ['PENDING', 'PLANNING', 'READY', 'RUNNING', 'PROMOTING', 'VERIFYING', 'CANCELLING']

const TRANSFER_STATE_TONE: Record<string, PillTone> = {
  SUCCEEDED: 'ok',
  FAILED: 'danger',
  CANCELLED: 'neutral',
  PAUSED: 'pending',
  RUNNING: 'review',
  VERIFYING: 'review',
  PLANNING: 'review',
  PROMOTING: 'review',
  CANCELLING: 'pending',
  READY: 'accent',
  PENDING: 'accent',
}

/** The same states, in sentences. See STATUS_LABEL below for why the constants
 *  stay shouted and the labels do not. */
const TRANSFER_STATE_LABEL: Record<string, string> = {
  PENDING: 'Pending',
  PLANNING: 'Planning',
  READY: 'Ready',
  RUNNING: 'Running',
  PAUSED: 'Paused',
  PROMOTING: 'Promoting',
  VERIFYING: 'Verifying',
  CANCELLING: 'Cancelling',
  CANCELLED: 'Cancelled',
  SUCCEEDED: 'Succeeded',
  FAILED: 'Failed',
}

const TRANSFER_STATE_HELP: Record<string, string> = {
  PENDING: 'Requested. Nothing has been planned yet.',
  PLANNING: 'Working out what has to move, by walking the release.',
  READY: 'Planned and waiting for a worker to take the first job.',
  RUNNING: 'Workers are moving bytes right now.',
  PAUSED: 'Nothing new is being handed out. Work already in flight finishes.',
  PROMOTING: 'The registry is relocating the release between two of its own repositories. No bytes cross the wire.',
  VERIFYING: 'The content is at the destination; its signature is being checked.',
  CANCELLING: 'Stopping. Jobs a worker already holds run to their next checkpoint.',
  CANCELLED: 'Stopped. What already reached the destination stayed there.',
  SUCCEEDED: 'Everything planned reached the destination.',
  FAILED: 'It stopped before finishing. Open it to see what failed.',
}

export function TransferStateTag({ state }: { state: string }) {
  const live = LIVE_STATES.includes(state)
  return (
    <Tooltip title={TRANSFER_STATE_HELP[state]}>
      <StatusPill
        tone={TRANSFER_STATE_TONE[state] ?? 'neutral'}
        icon={live ? <LoadingOutlined /> : undefined}
        style={{ marginInlineEnd: 0 }}
      >
        {TRANSFER_STATE_LABEL[state] ?? state}
      </StatusPill>
    </Tooltip>
  )
}

const VERIFICATION: Record<VerificationState, { label: string; tone: PillTone; icon: ReactNode; help: string }> = {
  SIGNED: {
    label: 'Signed',
    tone: 'ok',
    icon: <CheckCircleOutlined />,
    help: 'The vendor signed this release and the signature was found.',
  },
  NOT_SIGNED: {
    label: 'Not signed',
    tone: 'pending',
    icon: <ExclamationCircleOutlined />,
    help: 'Vendor signatures were not found for this release. Caution: it may have been tampered with or withdrawn by the vendor.',
  },
  VERIFICATION_FAILED: {
    label: 'Verification failed',
    tone: 'danger',
    icon: <CloseCircleOutlined />,
    help: 'A signature was found but did not verify. Do not use this release until it is explained.',
  },
  UNKNOWN: {
    // The one state that genuinely IS "nothing recorded", so it is the one that
    // takes the neutral tone. It is also the one whose tooltip matters most:
    // not checked is not the same as not signed.
    label: 'Not checked',
    tone: 'neutral',
    icon: <QuestionCircleOutlined />,
    help: 'Nobody has looked for a signature on this release. That is not the same as it being unsigned.',
  },
}

export function VerificationBadge({ state }: { state: VerificationState }) {
  const v = VERIFICATION[state]
  return (
    <Tooltip title={v.help}>
      <StatusPill tone={v.tone} icon={v.icon} style={{ marginInlineEnd: 0 }}>
        {v.label}
      </StatusPill>
    </Tooltip>
  )
}

/**
 * Where a release is, reading as a chain: `Vendor: Nokia`, `JFrog + Quay`.
 *
 * Every repository URL is clickable and opens in a new tab, because the brief
 * requires external repositories to always be reachable.
 */
export function LocationChip({
  locations, vendorLabel, vendorIcon,
}: {
  locations: Location[]
  vendorLabel?: string
  vendorIcon?: IconComponent
}) {
  if (locations.length === 0) {
    return <NA reason="This release has not been recorded in any location yet." />
  }

  // Once a release has been downloaded, where it CAME from is no longer the
  // interesting fact. The vendor stays visible only while it is the only place.
  const shown = locations.length > 1 ? locations.slice(1) : locations

  return (
    <Space size={4} wrap>
      {shown.map((loc, i) => {
        const Mark = locationIcon(loc.kind, loc.name)
        // Just the name. The icon already says which kind of place it is, and
        // prefixing "Vendor:" only widened the column.
        const isVendor = loc.kind === 'vendor'
        const label = isVendor ? vendorLabel || loc.name : loc.name
        const mark = isVendor && vendorIcon
          ? <Icon as={vendorIcon} title={loc.kind} />
          : <Icon as={Mark} title={loc.kind} />
        return (
          <span key={`${loc.name}-${i}`}>
            {i > 0 && <span style={{ color: c.text3, marginInlineEnd: 4 }}>+</span>}
            {loc.url ? (
              <a href={loc.url} target="_blank" rel="noreferrer">
                <Space size={4}>
                  {mark}
                  {label}
                  <ExportOutlined style={{ fontSize: 10 }} />
                </Space>
              </a>
            ) : (
              <Space size={4}>
                {mark}
                {label}
              </Space>
            )}
          </span>
        )
      })}
    </Space>
  )
}

/** A monospace URL that opens in a new tab. */
export function RepoLink({ url, label }: { url?: string; label?: string }) {
  if (!url) return <NA reason="This repository declares no address we can link to." />
  return (
    <a href={url} target="_blank" rel="noreferrer" style={{ fontFamily: mono, fontSize: 13 }}>
      {label ?? url.replace(/^https?:\/\//, '')} <ExportOutlined style={{ fontSize: 10 }} />
    </a>
  )
}

/** Relative time, with the absolute form on hover. */
export function TimeAgo({ at }: { at?: string | null }) {
  if (!at) return <NA />
  return (
    <Tooltip title={formatAbsolute(at)}>
      {/* One line, always: a column of dates that wrap is a table of rows
          that are all different heights. The exact minute is in the tooltip. */}
      <span style={{ whiteSpace: 'nowrap', fontVariantNumeric: 'tabular-nums' }}>
        {formatRelative(at)}
      </span>
    </Tooltip>
  )
}

/** A count with a coloured dot, for "3 new" style figures. */
export function CountBadge({ count, colour }: { count: number; colour?: string }) {
  if (!count) return <Typography.Text type="secondary">0</Typography.Text>
  return <Badge count={count} color={colour ?? c.danger} overflowCount={999} />
}

/** Configuration this page can show but never change (docs/design/19 §4). */
export function ManagedInGit({ url }: { url?: string }) {
  return (
    <Tooltip title="This is defined in Git and reconciled into the cluster. The interface shows it and never edits it - a change made here would be silently reverted.">
      <StatusPill tone="neutral" dot={false} style={{ marginInlineEnd: 0 }}>
        Managed in Git{url ? ' ↗' : ''}
      </StatusPill>
    </Tooltip>
  )
}


/**
 * A target, as a tag.
 *
 * # Two things this fixes
 *
 * It read `production · production`. The environment was appended
 * unconditionally, and most deployments name a target after the environment it
 * represents, so the common case was the word twice. It is now shown only when
 * it says something the name does not.
 *
 * And it was a filled block of dark green. Ant Design's `success` tag derives
 * its background from `colorSuccess`, which this theme overrides with a dark
 * AT&T green - so the status tags rendered heavy while every preset-coloured
 * tag beside them rendered light. Preset palettes are used throughout instead,
 * so a production target and an enabled rule look like they belong on the same
 * page.
 */
export function TargetTag({ target }: { target: Repository }) {
  const Mark = repositoryIcon(target)
  const environment = target.environment?.toLowerCase()
  const name = target.name.toLowerCase()

  // Only when it adds something. `production · production` is noise.
  const qualifier =
    target.environment && environment !== name && !name.includes(environment ?? '\u0000')
      ? target.environment
      : undefined

  return (
    <Tooltip title={`${target.registry}${target.repository ? `/${target.repository}` : ''}`}>
      <Tag
        color={environment === 'production' ? 'green' : 'default'}
        style={{ marginInlineEnd: 0 }}
      >
        <Space size={5}>
          <Icon as={Mark} title={target.type} />
          {target.name}
          {qualifier && (
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {qualifier}
            </Typography.Text>
          )}
        </Space>
      </Tag>
    </Tooltip>
  )
}
