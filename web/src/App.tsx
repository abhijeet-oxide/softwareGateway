import { Suspense, lazy } from 'react'
import { Navigate, Route, Routes, useParams, useSearchParams } from 'react-router-dom'
import { Spin } from 'antd'
import { Shell } from './Shell'

/**
 * Routing.
 *
 * Eight nav entries, and two drill-downs reached from them - never as extra
 * nav entries (UI brief §3). Every page is lazily loaded, so the first paint
 * carries the shell and one page rather than all ten.
 */

const Overview = lazy(() => import('./pages/Overview'))
const Products = lazy(() => import('./pages/Products'))
const Packages = lazy(() => import('./pages/Packages'))
const PackageDetail = lazy(() => import('./pages/PackageDetail'))
const DownloadDetail = lazy(() => import('./pages/DownloadDetail'))
const Downloads = lazy(() => import('./pages/Downloads'))
const Compare = lazy(() => import('./pages/Compare'))
const Repositories = lazy(() => import('./pages/Repositories'))
const Activity = lazy(() => import('./pages/Activity'))
const Reports = lazy(() => import('./pages/Reports'))
const Settings = lazy(() => import('./pages/Settings'))

/** Carries a bookmarked /software/{product}/{reference} onto /packages. */
function LegacyPackageRedirect() {
  const { product, reference } = useParams()
  const [params] = useSearchParams()
  const query = params.toString()
  return (
    <Navigate
      replace
      to={`/packages/${encodeURIComponent(product ?? '')}/${encodeURIComponent(reference ?? '')}` +
          (query ? `?${query}` : '')}
    />
  )
}

/** Carries a bookmarked /compare, product and version intact, onto /packages/compare. */
function LegacyCompareRedirect() {
  const [params] = useSearchParams()
  const query = params.toString()
  return <Navigate replace to={`/packages/compare${query ? `?${query}` : ''}`} />
}

export function App() {
  return (
    <Shell>
      <Suspense fallback={<Spin size="large" style={{ display: 'block', margin: '80px auto' }} />}>
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/products" element={<Products />} />
          <Route path="/products/:product" element={<Products />} />
          <Route path="/packages" element={<Packages />} />
          <Route path="/packages/:product/:reference" element={<PackageDetail />} />
          {/*
            The old spelling. Links to it exist in chat threads and tickets, so
            it redirects rather than 404ing - and it redirects to the same path
            under the new name so a deep link to one release still lands on it.
          */}
          <Route path="/software" element={<Navigate to="/packages" replace />} />
          <Route path="/software/:product/:reference" element={<LegacyPackageRedirect />} />
          <Route path="/downloads" element={<Downloads />} />
          <Route path="/downloads/:transferId" element={<DownloadDetail />} />
          <Route path="/packages/compare" element={<Compare />} />
          {/* The old path - links to it exist already, so it redirects rather than 404s. */}
          <Route path="/compare" element={<LegacyCompareRedirect />} />
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
