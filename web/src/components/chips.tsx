import type { ReactNode } from 'react'
import { Badge, Space, Tag, Tooltip, Typography } from 'antd'
import {
  AppstoreOutlined, CheckCircleOutlined, CloudServerOutlined, DatabaseOutlined,
  ExportOutlined, ExclamationCircleOutlined, CloseCircleOutlined, QuestionCircleOutlined,
  ShopOutlined, DeploymentUnitOutlined,
} from '@ant-design/icons'
import { Link } from 'react-router-dom'
import type { SoftwareStatus, VerificationState, Location } from '../domain/derive'
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
        <AppstoreOutlined style={{ color: '#0057B8' }} />
        <span style={{ fontWeight: 500 }}>{display || name}</span>
      </Space>
    </Link>
  )
}

/** A version, always monospace. Clicks through to the release. */
export function VersionChip({
  product, version, reference,
}: { product: string; version: string; reference?: string }) {
  const ref = reference ?? version
  return (
    <Link
      to={`/software/${encodeURIComponent(product)}/${encodeURIComponent(ref)}`}
      style={{ fontFamily: mono }}
    >
      {version}
    </Link>
  )
}

const STATUS_COLOUR: Record<SoftwareStatus, string> = {
  NEW: 'blue',
  DOWNLOADING: 'processing',
  DOWNLOADED: 'green',
  'READY FOR PRODUCTION': 'purple',
  PRODUCTION: 'success',
  'VERIFICATION FAILED': 'error',
}

/** The six statuses and no others. */
export function StatusBadge({ status }: { status: SoftwareStatus }) {
  return <Tag color={STATUS_COLOUR[status]} style={{ marginInlineEnd: 0 }}>{status}</Tag>
}

const VERIFICATION: Record<VerificationState, { label: string; colour: string; icon: ReactNode; help: string }> = {
  SIGNED: {
    label: 'Signed',
    colour: 'success',
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

const LOCATION_ICON = {
  vendor: <ShopOutlined />,
  storage: <DatabaseOutlined />,
  mirror: <CloudServerOutlined />,
  production: <DeploymentUnitOutlined />,
}

/**
 * Where a release is, reading as a chain: `Vendor: Nokia`, `JFrog + Quay`.
 *
 * Every repository URL is clickable and opens in a new tab, because the brief
 * requires external repositories to always be reachable.
 */
export function LocationChip({ locations }: { locations: Location[] }) {
  if (locations.length === 0) {
    return <Typography.Text type="secondary">—</Typography.Text>
  }

  // Once a release has been downloaded, where it CAME from is no longer the
  // interesting fact. The vendor stays visible only while it is the only place.
  const shown = locations.length > 1 ? locations.slice(1) : locations

  return (
    <Space size={4} wrap>
      {shown.map((loc, i) => (
        <span key={`${loc.name}-${i}`}>
          {i > 0 && <span style={{ color: '#98A2B3', marginInlineEnd: 4 }}>+</span>}
          {loc.url ? (
            <a href={loc.url} target="_blank" rel="noreferrer">
              <Space size={4}>
                {LOCATION_ICON[loc.kind]}
                {locations.length === 1 ? `Vendor: ${loc.name}` : loc.name}
                <ExportOutlined style={{ fontSize: 10 }} />
              </Space>
            </a>
          ) : (
            <Space size={4}>
              {LOCATION_ICON[loc.kind]}
              {locations.length === 1 ? `Vendor: ${loc.name}` : loc.name}
            </Space>
          )}
        </span>
      ))}
    </Space>
  )
}

/** A monospace URL that opens in a new tab. */
export function RepoLink({ url, label }: { url?: string; label?: string }) {
  if (!url) return <Typography.Text type="secondary">—</Typography.Text>
  return (
    <a href={url} target="_blank" rel="noreferrer" style={{ fontFamily: mono, fontSize: 13 }}>
      {label ?? url.replace(/^https?:\/\//, '')} <ExportOutlined style={{ fontSize: 10 }} />
    </a>
  )
}

/** Relative time, with the absolute form on hover. */
export function TimeAgo({ at }: { at?: string | null }) {
  if (!at) return <Typography.Text type="secondary">—</Typography.Text>
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
