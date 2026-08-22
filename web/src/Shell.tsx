import type { ReactNode } from 'react'
import { Avatar, Badge, Button, Layout, Menu, Space, Tooltip, Typography } from 'antd'
import {
  BarChartOutlined, BellOutlined,
  DatabaseOutlined, HistoryOutlined,
  QuestionCircleOutlined, SettingOutlined,
} from '@ant-design/icons'
import DashboardOutlineIcon from '@iconify-react/material-symbols/dashboard-outline';
import ProductCatalogIcon from '@iconify-react/fluent-mdl2/product-catalog';
import { DownloadIcon, Icon, PackageIcon } from './components/icons'
import { useLocation, useNavigate } from 'react-router-dom'
import { useIdentity } from './auth/permissions'
import { useTransfers } from './api/queries'
import { branding, palette } from './theme'
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
      <Sider width={216} style={{ background: palette.sidebar }}>
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

      <Layout>
        <Header
          style={{
            background: palette.topBar, borderBottom: `1px solid ${palette.topBarBorder}`,
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
