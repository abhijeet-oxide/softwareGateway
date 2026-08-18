import { Suspense, lazy } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { Spin } from 'antd'
import { Shell } from './Shell'

/**
 * Routing.
 *
 * Eight nav entries, and two drill-downs reached from them — never as extra
 * nav entries (UI brief §3). Every page is lazily loaded, so the first paint
 * carries the shell and one page rather than all ten.
 */

const Overview = lazy(() => import('./pages/Overview'))
const Products = lazy(() => import('./pages/Products'))
const Software = lazy(() => import('./pages/Software'))
const SoftwareDetail = lazy(() => import('./pages/SoftwareDetail'))
const DownloadDetail = lazy(() => import('./pages/DownloadDetail'))
const Downloads = lazy(() => import('./pages/Downloads'))
const Compare = lazy(() => import('./pages/Compare'))
const Repositories = lazy(() => import('./pages/Repositories'))
const Activity = lazy(() => import('./pages/Activity'))
const Reports = lazy(() => import('./pages/Reports'))
const Settings = lazy(() => import('./pages/Settings'))

export function App() {
  return (
    <Shell>
      <Suspense fallback={<Spin size="large" style={{ display: 'block', margin: '80px auto' }} />}>
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/products" element={<Products />} />
          <Route path="/products/:product" element={<Products />} />
          <Route path="/software" element={<Software />} />
          <Route path="/software/:product/:reference" element={<SoftwareDetail />} />
          <Route path="/downloads" element={<Downloads />} />
          <Route path="/downloads/:transferId" element={<DownloadDetail />} />
          <Route path="/compare" element={<Compare />} />
          <Route path="/repositories" element={<Repositories />} />
          <Route path="/activity" element={<Activity />} />
          <Route path="/reports" element={<Reports />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Suspense>
    </Shell>
  )
}
