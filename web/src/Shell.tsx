import type { ReactNode } from 'react'
import { Avatar, Badge, Button, Layout, Menu, Space, Tooltip, Typography } from 'antd'
import {
  AppstoreOutlined, BarChartOutlined, BellOutlined, CloudDownloadOutlined,
  DatabaseOutlined, FileTextOutlined, HistoryOutlined, HomeOutlined,
  QuestionCircleOutlined, SettingOutlined,
} from '@ant-design/icons'
import { useLocation, useNavigate } from 'react-router-dom'
import { useIdentity } from './auth/permissions'
import { useTransfers } from './api/queries'
import { attNavy } from './theme'

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

  const { data: transfers } = useTransfers({ pageSize: 100 })
  const running = (transfers?.transfers ?? []).filter(
    (t) => t.state === 'RUNNING' || t.state === 'PLANNING' || t.state === 'READY',
  ).length
  const failing = (transfers?.transfers ?? []).filter((t) => t.state === 'FAILED').length

  const selected = NAV.map((n) => n.key)
    .filter((k) => (k === '/' ? location.pathname === '/' : location.pathname.startsWith(k)))
    .sort((a, b) => b.length - a.length)[0] ?? '/'

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider width={216} style={{ background: attNavy }}>
        <div style={{ padding: '18px 16px 14px', color: '#fff' }}>
          <Typography.Text style={{ color: '#fff', fontWeight: 700, fontSize: 15 }}>
            Software Gateway
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
              <div style={{ color: '#fff', fontSize: 13 }}>{who?.subject ?? 'Not signed in'}</div>
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
