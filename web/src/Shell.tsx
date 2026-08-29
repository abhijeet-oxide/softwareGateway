import { useState, type ReactNode } from 'react'
import { Badge, Button, Tooltip } from 'antd'
import {
  BarChartOutlined, BellOutlined, DashboardOutlined, DatabaseOutlined, HistoryOutlined,
  InboxOutlined, PackageOutlined, ProductOutlined, QuestionCircleOutlined,
  SettingOutlined,
} from './icons'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { useIdentity } from './auth/permissions'
import { useTransfers, useVersion } from './api/queries'
import brand from './brand'
import {
  AppShell, c, envHex, SideNav, ThemeToggleButton, TopBar, withAlpha,
  type NavItem, type NavProfile,
} from './uikit'

// Whether the navigation is collapsed is a per-person preference, not state the
// server has an opinion about, so it lives on the device - the same place the
// sibling tool keeps it, and the same place the theme lives.
const COLLAPSE_KEY = 'gateway.nav.collapsed.v1'

function loadCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === '1'
  } catch {
    // Private browsing, storage disabled by policy: expanded is the right
    // default and a preference failing to load must never stop a page painting.
    return false
  }
}

/**
 * The application shell.
 *
 * Nine nav items, in this order, and nothing else ever. Detail views are
 * drill-downs, not extra entries (UI brief §3).
 *
 * The chrome itself - the navigation's width and item heights, its hover and
 * active language, the collapse behaviour, the profile card at its foot, the
 * bar's height and how its two ends are arranged - is `SideNav`/`TopBar` from
 * the shared design system, byte-identical to what the sibling tool mounts.
 * What is this application's is only WHICH entries there are and what belongs
 * in its bar.
 */
const NAV = [
  { key: '/', icon: <DashboardOutlined />, label: 'Overview' },
  { key: '/products', icon: <ProductOutlined />, label: 'Products' },
  { key: '/packages', icon: <PackageOutlined />, label: 'Packages' },
  { key: '/downloads', icon: <InboxOutlined />, label: 'Downloads' },
  // Hidden for now. Re-enabling it is this block plus `SafetyOutlined` back in
  // the import above - the icon is in the registry, it is just not imported
  // while nothing renders it (the build refuses an unused import).
  // {
  //   key: '/security',
  //   icon: <SafetyOutlined />,
  //   // "Security" rather than "Vulnerabilities": the page answers questions
  //   // about packages and images that have none as readily as about ones that
  //   // do, and a nav entry named after the bad news is one people avoid.
  //   label: 'Security',
  // },
  { key: '/repositories', icon: <DatabaseOutlined />, label: 'Repositories' },
  { key: '/activity', icon: <HistoryOutlined />, label: 'Activity' },
  { key: '/reports', icon: <BarChartOutlined />, label: 'Reports' },
  { key: '/settings', icon: <SettingOutlined />, label: 'Settings' },
]

/**
 * What the system is doing right now, in one line.
 *
 * Three states and no more: something is failing, something is running, or
 * everything has settled. The settled case is stated rather than left blank -
 * a bar that says nothing when all is well is a bar a reader learns to ignore,
 * and then does not notice on the day it has something to say.
 */
function ActivityPill({ running, failing }: { running: number; failing: number }) {
  const [tone, text] = failing > 0
    ? [c.danger, `${failing} download${failing === 1 ? '' : 's'} failed`]
    : running > 0
      ? [c.brand, `${running} download${running === 1 ? '' : 's'} running`]
      : [c.ok, 'All downloads settled']

  return (
    <Link to="/downloads" style={{ textDecoration: 'none' }}>
      <span
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 8,
          padding: '4px 12px 4px 10px', borderRadius: 999,
          background: c.surface2, border: `1px solid ${c.border}`,
          fontSize: 12.5, lineHeight: 1.4, color: c.text2, whiteSpace: 'nowrap',
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        <span
          aria-hidden
          style={{
            width: 7, height: 7, borderRadius: '50%', background: tone,
            // The pulse marks work in flight and stops the moment it settles,
            // so movement in this bar always means something is moving.
            //
            // withAlpha rather than two more hex digits: `tone` is a var() now,
            // and `var(--brand)22` is not a colour - the rule would simply be
            // dropped and the ring never appear.
            boxShadow: running > 0 && failing === 0 ? `0 0 0 3px ${withAlpha(tone, 0.13)}` : undefined,
          }}
        />
        {text}
      </span>
    </Link>
  )
}

export function Shell({ children }: { children: ReactNode }) {
  const location = useLocation()
  const navigate = useNavigate()
  const { who } = useIdentity()

  const [collapsed, setCollapsed] = useState(loadCollapsed)
  const version = useVersion()

  const { data: transfers } = useTransfers({ pageSize: 100 })
  const running = (transfers?.transfers ?? []).filter(
    (t) => t.state === 'RUNNING' || t.state === 'PLANNING' || t.state === 'READY',
  ).length
  const failing = (transfers?.transfers ?? []).filter((t) => t.state === 'FAILED').length

  const selected = NAV.map((n) => n.key)
    .filter((k) => (k === '/' ? location.pathname === '/' : location.pathname.startsWith(k)))
    .sort((a, b) => b.length - a.length)[0] ?? '/'

  const section = NAV.find((n) => n.key === selected)?.label ?? brand.appName

  const items: NavItem[] = NAV.map((n) => ({
    key: n.key,
    label: n.label,
    icon: n.icon,
    ...(n.key === '/downloads' && failing > 0 ? { badge: failing } : {}),
    onClick: () => navigate(n.key),
  }))

  // The person at the navigation's foot, in the same card the sibling tool
  // uses: who you are, and one click to Settings, where every personal
  // preference lives.
  const profile: NavProfile = {
    name: who?.subject ?? 'Not signed in',
    sub: who?.authenticated ? (who.roles?.join(', ') || 'Product Owner') : 'Not signed in',
    active: selected === '/settings',
    onClick: () => navigate('/settings'),
  }

  return (
    <AppShell
      nav={
        <SideNav
          brand={brand}
          items={items}
          activeKey={selected}
          collapsed={collapsed}
          onToggleCollapse={() => {
            const next = !collapsed
            setCollapsed(next)
            try {
              localStorage.setItem(COLLAPSE_KEY, next ? '1' : '0')
            } catch {
              // The session keeps the choice; it just will not be remembered.
            }
          }}
          profile={profile}
          footer={<DeploymentNote version={version.data?.version} />}
        />
      }
      header={
        /*
          The top bar carries WHERE YOU ARE and WHAT THE SYSTEM IS DOING.

          It was an empty white band with two icons pushed against the right
          edge - fourteen hundred pixels of nothing above every page. The two
          facts now in it were both already computed and both hidden: the
          section name existed only as a highlighted nav item on the other side
          of the window, and the number of downloads currently running lived
          inside the tooltip of a bell, which is to say nowhere. An operations
          console should say what it is doing without being asked.
        */
        <TopBar
          title={section}
          right={
            <>
              <ActivityPill running={running} failing={failing} />
              {/*
                Light and dark, in the one place a person looks for it. The
                control comes from the shared kit, so both tools put the same
                button in the same corner.
              */}
              <ThemeToggleButton />
              <Tooltip title="Documentation">
                <Button type="text" icon={<QuestionCircleOutlined />} aria-label="Help" />
              </Tooltip>
              <Tooltip
                title={failing ? `${failing} download${failing === 1 ? '' : 's'} failed` : 'No notifications'}
              >
                <Badge count={failing} size="small">
                  <Button type="text" icon={<BellOutlined />} aria-label="Notifications" />
                </Badge>
              </Tooltip>
            </>
          }
        />
      }
    >
      {children}
    </AppShell>
  )
}

/**
 * Which installation this is, at the navigation's foot.
 *
 * A support conversation or a screenshot that does not say which Coordinator
 * it came from costs a round trip to establish, every time. The dot carries the
 * environment's own colour - deliberately not a status colour, because a
 * healthy production deployment is not a warning.
 */
function DeploymentNote({ version }: { version?: string }) {
  if (!version) return null
  return (
    <div className="ui-nav-note">
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: 3,
          flexShrink: 0,
          background: envHex('production'),
        }}
      />
      Coordinator {version}
    </div>
  )
}
