import type { ReactNode } from 'react'
import { Avatar, Badge, Button, Layout, Menu, Space, Tooltip, Typography } from 'antd'
import {
  BarChartOutlined, BellOutlined,
  DatabaseOutlined, HistoryOutlined, SafetyOutlined,
  QuestionCircleOutlined, SettingOutlined,
} from '@ant-design/icons'
import DashboardOutlineIcon from '@iconify-react/material-symbols/dashboard-outline';
import ProductCatalogIcon from '@iconify-react/fluent-mdl2/product-catalog';
import { DownloadIcon, Icon, PackageIcon } from './components/icons'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { useIdentity } from './auth/permissions'
import { useTransfers } from './api/queries'
import { branding, palette, semantic } from './theme'
import { BrandMark } from './components/icons'

const { Sider, Header, Content } = Layout

/**
 * The application shell.
 *
 * Eight nav items, in this order, and nothing else ever. Detail views are
 * drill-downs, not extra entries (UI brief §3).
 */
const NAV = [
  { key: '/', icon: <DashboardOutlineIcon height="1em" />, label: 'Overview' },
  { key: '/products', icon: <ProductCatalogIcon height="1em" />, label: 'Products' },
  {
    key: '/packages',
    icon: <Icon as={PackageIcon} title="Packages" className="anticon" />,
    label: 'Packages',
  },
  {
    key: '/downloads',
    icon: <Icon as={DownloadIcon} title="Downloads" className="anticon" />,
    label: 'Downloads',
  },
  {
    key: '/security',
    icon: <SafetyOutlined />,
    // "Security" rather than "Vulnerabilities": the page answers questions
    // about packages and images that have none as readily as about ones that
    // do, and a nav entry named after the bad news is one people avoid.
    label: 'Security',
  },
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
    ? [semantic.error, `${failing} download${failing === 1 ? '' : 's'} failed`]
    : running > 0
      ? [palette.primary, `${running} download${running === 1 ? '' : 's'} running`]
      : [semantic.success, 'All downloads settled']

  return (
    <Link to="/downloads" style={{ textDecoration: 'none' }}>
      <span
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 8,
          padding: '4px 12px 4px 10px', borderRadius: 999,
          background: palette.sunken, border: `1px solid ${palette.hairline}`,
          fontSize: 12.5, lineHeight: 1.4, color: semantic.neutral, whiteSpace: 'nowrap',
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        <span
          aria-hidden
          style={{
            width: 7, height: 7, borderRadius: '50%', background: tone,
            // The pulse marks work in flight and stops the moment it settles,
            // so movement in this bar always means something is moving.
            boxShadow: running > 0 && failing === 0 ? `0 0 0 3px ${tone}22` : undefined,
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

  const { data: transfers } = useTransfers({ pageSize: 100 })
  const running = (transfers?.transfers ?? []).filter(
    (t) => t.state === 'RUNNING' || t.state === 'PLANNING' || t.state === 'READY',
  ).length
  const failing = (transfers?.transfers ?? []).filter((t) => t.state === 'FAILED').length

  const selected = NAV.map((n) => n.key)
    .filter((k) => (k === '/' ? location.pathname === '/' : location.pathname.startsWith(k)))
    .sort((a, b) => b.length - a.length)[0] ?? '/'

  const section = NAV.find((n) => n.key === selected)?.label ?? branding.name

  return (
    <Layout style={{ height: '100dvh', overflow: 'hidden' }}>
      <Sider
        width={216}
        style={{
          background: palette.sidebar,
          // A rule the navy itself provides, so the sidebar has an edge rather
          // than bleeding into the page background at exactly one lightness.
          boxShadow: 'inset -1px 0 0 rgba(255,255,255,0.06)',
        }}
      >
        {/*
          The name, and the mark a deployment sets beside it. Both come from
          theme.ts, which is the one file a company edits to make this look
          like theirs.
        */}
        <div style={{ padding: '18px 16px 14px' }}>
          <Space size={8} align="center">
            <BrandMark />
            <Typography.Text style={{ color: '#fff', fontWeight: 700, fontSize: 15 }}>
              {branding.name}
            </Typography.Text>
          </Space>
        </div>

        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[selected]}
          items={NAV}
          onClick={({ key }) => navigate(key)}
          style={{ background: palette.sidebar, borderInlineEnd: 0 }}
        />

        <div
          style={{
            position: 'absolute', bottom: 0, width: '100%', padding: 16,
            borderTop: '1px solid rgba(255,255,255,0.12)',
          }}
        >
          <Space size={10}>
            <Avatar size="small" style={{ background: palette.primary }}>
              {(who?.subject ?? 'A').slice(0, 2).toUpperCase()}
            </Avatar>
            <div style={{ lineHeight: 1.3 }}>
              <div style={{ color: '#fff', fontSize: 13 }}>{who?.subject ?? 'Not signed in'}</div>
              <div style={{ color: 'rgba(255,255,255,0.6)', fontSize: 11 }}>
                {who?.authenticated ? (who.roles?.join(', ') || 'Product Owner') : 'Not signed in'}
              </div>
            </div>
          </Space>
        </div>
      </Sider>

      <Layout style={{ minWidth: 0, overflow: 'hidden' }}>
        {/*
          The top bar carries WHERE YOU ARE and WHAT THE SYSTEM IS DOING.

          It was an empty white band with two icons pushed against the right
          edge - fourteen hundred pixels of nothing above every page. The two
          facts now in it were both already computed and both hidden: the
          section name existed only as a highlighted nav item on the other side
          of the window, and the number of downloads currently running lived
          inside the tooltip of a bell, which is to say nowhere. An operations
          console should say what it is doing without being asked.
        */}
        <Header
          style={{
            background: palette.topBar, borderBottom: `1px solid ${palette.topBarBorder}`,
            display: 'flex', alignItems: 'center', gap: 16, paddingInline: 24,
            // Ant sets `line-height: 64px` on the header. Anything inline
            // inside it inherits a 64px line box, which turned a 26px pill
            // into a 72px ellipse hanging below the bar.
            lineHeight: 'normal',
          }}
        >
          <Typography.Text
            style={{
              fontSize: 15, fontWeight: 600, color: palette.headingText,
              letterSpacing: '-0.01em',
            }}
          >
            {section}
          </Typography.Text>

          <div style={{ marginInlineStart: 'auto' }}>
            <ActivityPill running={running} failing={failing} />
          </div>

          <Space size={4}>
            <Tooltip title="Documentation">
              <Button type="text" icon={<QuestionCircleOutlined />} aria-label="Help" />
            </Tooltip>

            <Tooltip title={failing ? `${failing} download${failing === 1 ? '' : 's'} failed` : 'No notifications'}>
              <Badge count={failing} size="small">
                <Button type="text" icon={<BellOutlined />} aria-label="Notifications" />
              </Badge>
            </Tooltip>
          </Space>
        </Header>

        <Content style={{ flex: 1, minHeight: 0, padding: 24, maxWidth: '100%', overflow: 'auto' }}>
          {children}
        </Content>
      </Layout>
    </Layout>
  )
}
