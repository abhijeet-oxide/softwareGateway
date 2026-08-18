import type { ReactNode } from 'react'
import { Badge, Space, Tag, Tooltip, Typography } from 'antd'
import {
  CheckCircleOutlined, ExportOutlined, ExclamationCircleOutlined,
  CloseCircleOutlined, QuestionCircleOutlined,
} from '@ant-design/icons'
import { Icon, locationIcon, repositoryIcon, PackageIcon } from './icons'
import { Link } from 'react-router-dom'
import { NA } from './value'
import { releaseHref, type SoftwareStatus, type VerificationState, type Location } from '../domain/derive'
import type { Package, Repository } from '../api/types'
import { formatAbsolute, formatRelative } from '../domain/format'
import { mono, semantic } from '../theme'

/**
 * The reusable vocabulary (UI brief §7). Designed once, used literally.
 *
 * Two rules run through all of them:
 *   - every status is stated in WORDS; colour only reinforces;
 *   - no bare icon stands in for a verb, and every icon carries a label or a
 *     tooltip.
 */

/** Product icon and name. Clicks through to the product. */
export function ProductChip({ name, display }: { name: string; display?: string }) {
  return (
    <Link to={`/products/${encodeURIComponent(name)}`}>
      <Space size={6}>
        <Icon as={PackageIcon} colour="#0057B8" title="Product" />
        <span style={{ fontWeight: 500 }}>{display || name}</span>
      </Space>
    </Link>
  )
}

/** A version, always monospace. Clicks through to the release. */
export function VersionChip({
  product, version, pkg,
}: {
  product: string
  version: string
  /** The release itself, so the link can carry the repository that identifies it. */
  pkg: Pick<Package, 'tag' | 'sourceRepository' | 'displayRepository'>
}) {
  // The repository is shown, not just linked. A vendor publishes one version
  // tag into every repository of a product, so a listing shows the same
  // version many times and the rows are only telling apart by where each one
  // came from — without it the page reads as duplicates.
  const repository = pkg.displayRepository || pkg.sourceRepository

  return (
    <Space direction="vertical" size={0}>
      <Link to={releaseHref(product, pkg)} style={{ fontFamily: mono }}>
        {version}
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

const STATUS_COLOUR: Record<SoftwareStatus, string> = {
  NEW: 'blue',
  DOWNLOADING: 'processing',
  DOWNLOADED: 'green',
  'READY FOR PRODUCTION': 'purple',
  PRODUCTION: 'green',
  'VERIFICATION FAILED': 'error',
}

/** The six statuses and no others. */
export function StatusBadge({ status }: { status: SoftwareStatus }) {
  return <Tag color={STATUS_COLOUR[status]} style={{ marginInlineEnd: 0 }}>{status}</Tag>
}

const VERIFICATION: Record<VerificationState, { label: string; colour: string; icon: ReactNode; help: string }> = {
  SIGNED: {
    label: 'Signed',
    colour: 'green',
    icon: <CheckCircleOutlined />,
    help: 'The vendor signed this release and the signature was found.',
  },
  NOT_SIGNED: {
    label: 'Not Signed',
    colour: 'warning',
    icon: <ExclamationCircleOutlined />,
    help: 'We looked for a vendor signature and found none.',
  },
  VERIFICATION_FAILED: {
    label: 'Verification Failed',
    colour: 'error',
    icon: <CloseCircleOutlined />,
    help: 'A signature was found but did not verify. Do not use this release until it is explained.',
  },
  UNKNOWN: {
    label: 'Not Checked',
    colour: 'default',
    icon: <QuestionCircleOutlined />,
    help: 'Nobody has looked for a signature on this release. That is not the same as it being unsigned.',
  },
}

export function VerificationBadge({ state }: { state: VerificationState }) {
  const v = VERIFICATION[state]
  return (
    <Tooltip title={v.help}>
      <Tag icon={v.icon} color={v.colour} style={{ marginInlineEnd: 0 }}>
        {v.label}
      </Tag>
    </Tooltip>
  )
}

/**
 * Where a release is, reading as a chain: `Vendor: Nokia`, `JFrog + Quay`.
 *
 * Every repository URL is clickable and opens in a new tab, because the brief
 * requires external repositories to always be reachable.
 */
export function LocationChip({ locations }: { locations: Location[] }) {
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
        const label = loc.name
        return (
          <span key={`${loc.name}-${i}`}>
            {i > 0 && <span style={{ color: '#98A2B3', marginInlineEnd: 4 }}>+</span>}
            {loc.url ? (
              <a href={loc.url} target="_blank" rel="noreferrer">
                <Space size={4}>
                  <Icon as={Mark} title={loc.kind} />
                  {label}
                  <ExportOutlined style={{ fontSize: 10 }} />
                </Space>
              </a>
            ) : (
              <Space size={4}>
                <Icon as={Mark} title={loc.kind} />
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
      <span>{formatRelative(at)}</span>
    </Tooltip>
  )
}

/** A count with a coloured dot, for "3 new" style figures. */
export function CountBadge({ count, colour }: { count: number; colour?: string }) {
  if (!count) return <Typography.Text type="secondary">0</Typography.Text>
  return <Badge count={count} color={colour ?? semantic.error} overflowCount={999} />
}

/** Configuration this page can show but never change (docs/design/19 §4). */
export function ManagedInGit({ url }: { url?: string }) {
  return (
    <Tooltip title="This is defined in Git and reconciled into the cluster. The interface shows it and never edits it — a change made here would be silently reverted.">
      <Tag color="default" style={{ marginInlineEnd: 0 }}>
        Managed in Git{url ? ' ↗' : ''}
      </Tag>
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
 * AT&T green — so the status tags rendered heavy while every preset-coloured
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
