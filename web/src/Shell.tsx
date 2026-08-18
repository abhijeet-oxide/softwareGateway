import type { ReactNode } from 'react'
import { Avatar, Badge, Button, Layout, Menu, Space, Tooltip, Typography } from 'antd'
import {
  AppstoreOutlined, BarChartOutlined, BellOutlined, CloudDownloadOutlined,
  DatabaseOutlined, FileTextOutlined, HistoryOutlined, HomeOutlined,
  PlayCircleOutlined, QuestionCircleOutlined, SettingOutlined,
} from '@ant-design/icons'
import { useLocation, useNavigate } from 'react-router-dom'
import { useIdentity, useCan } from './auth/permissions'
import { useRunDiscovery, useTransfers } from './api/queries'
import { attNavy } from './theme'
import { TimeAgo } from './components/chips'

const { Sider, Header, Content } = Layout

/**
 * The application shell.
 *
 * Eight nav items, in this order, and nothing else ever. Detail views are
 * drill-downs, not extra entries (UI brief §3).
 */
const NAV = [
  { key: '/', icon: <HomeOutlined />, label: 'Overview' },
  { key: '/products', icon: <AppstoreOutlined />, label: 'Products' },
  { key: '/software', icon: <FileTextOutlined />, label: 'Software' },
  { key: '/downloads', icon: <CloudDownloadOutlined />, label: 'Downloads' },
  { key: '/repositories', icon: <DatabaseOutlined />, label: 'Repositories' },
  { key: '/activity', icon: <HistoryOutlined />, label: 'Activity' },
  { key: '/reports', icon: <BarChartOutlined />, label: 'Reports' },
  { key: '/settings', icon: <SettingOutlined />, label: 'Settings' },
]

export function Shell({ children }: { children: ReactNode }) {
  const location = useLocation()
  const navigate = useNavigate()
  const { who } = useIdentity()

  // Run Discovery is the estate-wide primary action, so it asks the
  // estate-wide question — which a caller scoped to some products cannot
  // answer, and the control disables itself rather than failing on click.
  const mayDiscover = useCan('operate')
  const discover = useRunDiscovery()

  const { data: transfers } = useTransfers({ pageSize: 100 })
  const running = (transfers?.transfers ?? []).filter(
    (t) => t.state === 'RUNNING' || t.state === 'PLANNING' || t.state === 'READY',
  ).length
  const failing = (transfers?.transfers ?? []).filter((t) => t.state === 'FAILED').length

  // The most recent completion is the closest honest answer to "when did we
  // last look", without inventing a discovery timestamp the API does not serve
  // estate-wide.
  const lastActivity = (transfers?.transfers ?? [])
    .map((t) => t.completedAt)
    .filter((t): t is string => Boolean(t))
    .sort()
    .at(-1)

  const selected = NAV.map((n) => n.key)
    .filter((k) => (k === '/' ? location.pathname === '/' : location.pathname.startsWith(k)))
    .sort((a, b) => b.length - a.length)[0] ?? '/'

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider width={216} style={{ background: attNavy }}>
        <div style={{ padding: '18px 16px 14px', color: '#fff' }}>
          <Typography.Text style={{ color: '#fff', fontWeight: 700, fontSize: 15 }}>
            Software Lifecycle Manager
          </Typography.Text>
        </div>

        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[selected]}
          items={NAV}
          onClick={({ key }) => navigate(key)}
          style={{ background: attNavy, borderInlineEnd: 0 }}
        />

        <div
          style={{
            position: 'absolute', bottom: 0, width: '100%', padding: 16,
            borderTop: '1px solid rgba(255,255,255,0.12)',
          }}
        >
          <Space size={10}>
            <Avatar size="small" style={{ background: '#1E4E8C' }}>
              {(who?.subject ?? 'A').slice(0, 2).toUpperCase()}
            </Avatar>
            <div style={{ lineHeight: 1.3 }}>
              <div style={{ color: '#fff', fontSize: 13 }}>{who?.subject ?? '—'}</div>
              <div style={{ color: 'rgba(255,255,255,0.6)', fontSize: 11 }}>
                {who?.authenticated ? (who.roles?.join(', ') || 'Product Owner') : 'Not signed in'}
              </div>
            </div>
          </Space>
        </div>
      </Sider>

      <Layout>
        <Header
          style={{
            background: '#fff', borderBottom: '1px solid #E4E8EE',
            display: 'flex', alignItems: 'center', justifyContent: 'flex-end',
            gap: 16, paddingInline: 24,
          }}
        >
          <Space size={16}>
            <Typography.Text type="secondary" style={{ fontSize: 13 }}>
              Last activity: <TimeAgo at={lastActivity} />
            </Typography.Text>

            <Tooltip
              title={
                mayDiscover
                  ? 'Poll every vendor registry now, rather than waiting for the next scheduled scan.'
                  : 'You do not have permission to run discovery.'
              }
            >
              <Button
                type="primary"
                icon={<PlayCircleOutlined />}
                disabled={!mayDiscover}
                loading={discover.isPending}
                onClick={() => discover.mutate(undefined)}
              >
                Run Discovery
              </Button>
            </Tooltip>

            <Tooltip title="Documentation">
              <Button type="text" icon={<QuestionCircleOutlined />} aria-label="Help" />
            </Tooltip>

            <Tooltip title={`${running} downloading, ${failing} failed`}>
              <Badge count={failing} size="small">
                <Button type="text" icon={<BellOutlined />} aria-label="Notifications" />
              </Badge>
            </Tooltip>
          </Space>
        </Header>

        <Content style={{ padding: 24, maxWidth: '100%', overflowX: 'hidden' }}>
          {children}
        </Content>
      </Layout>
    </Layout>
  )
}
