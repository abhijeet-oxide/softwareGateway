import { Suspense, lazy } from 'react'
import { Navigate, Route, Routes, useLocation, useParams, useSearchParams } from 'react-router-dom'
import { Spin } from 'antd'
import { Shell } from './Shell'
import { PageTransition } from './uikit'

/**
 * Routing.
 *
 * Nine nav entries, and two drill-downs reached from them - never as extra
 * nav entries (UI brief §3). Security is the ninth: it is a place somebody
 * arrives at with a CVE identifier in hand and no release in mind, so it
 * cannot be reached only from a release. Every page is lazily loaded, so the first paint
 * carries the shell and one page rather than all ten.
 */

const Overview = lazy(() => import('./pages/Overview'))
const Products = lazy(() => import('./pages/Products'))
const Packages = lazy(() => import('./pages/Packages'))
const PackageDetail = lazy(() => import('./pages/PackageDetail'))
const DownloadDetail = lazy(() => import('./pages/DownloadDetail'))
const Downloads = lazy(() => import('./pages/Downloads'))
const Compare = lazy(() => import('./pages/Compare'))
const Security = lazy(() => import('./pages/Security'))
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
  const { pathname } = useLocation()
  /*
    The KEY is the page, not the URL.

    A path with a parameter in it - one release, one download - is the same page
    showing something else, and re-running the entrance every time somebody
    changes a query string or steps between two releases would animate a change
    that is not a change of screen. Two segments is where every route in this
    application stops being a different page and starts being a different
    subject.
  */
  const page = pathname.split('/').slice(0, 3).join('/')

  return (
    <Shell>
      <Suspense fallback={<Spin size="large" style={{ display: 'block', margin: '80px auto' }} />}>
        <PageTransition routeKey={page}>
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
          <Route path="/security" element={<Security />} />
          <Route path="/repositories" element={<Repositories />} />
          <Route path="/activity" element={<Activity />} />
          <Route path="/reports" element={<Reports />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
        </PageTransition>
      </Suspense>
    </Shell>
  )
}
