import { Suspense } from 'react'
import { Navigate, Route, Routes, useLocation, useParams, useSearchParams } from 'react-router-dom'
import { Shell } from './Shell'
import {
  lazyRoute, PageLoading, RouteErrorBoundary, usePreloadRoutes, type RouteModule,
} from './routing'

/**
 * Routing.
 *
 * Nine nav entries, and two drill-downs reached from them - never as extra
 * nav entries (UI brief §3). Security is the ninth: it is a place somebody
 * arrives at with a CVE identifier in hand and no release in mind, so it
 * cannot be reached only from a release. Every page is lazily loaded, so the first paint
 * carries the shell and one page rather than all ten.
 *
 * # Lazily loaded, and then warmed
 *
 * Loading them lazily and leaving it there is what made a click on
 * Repositories change the URL and leave Downloads on the screen: the router's
 * navigation is a transition, a transition that suspends keeps the OLD page
 * up, and the chunk was queued behind a page's worth of polling. So the
 * chunks are fetched while nothing is happening, and the boundary below is
 * keyed so a slow one shows a loading state rather than the previous page.
 * See `./routing` for the whole account.
 */

const Overview = lazyRoute(() => import('./pages/Overview'))
const Products = lazyRoute(() => import('./pages/Products'))
const Packages = lazyRoute(() => import('./pages/Packages'))
const PackageDetail = lazyRoute(() => import('./pages/PackageDetail'))
const DownloadDetail = lazyRoute(() => import('./pages/DownloadDetail'))
const Downloads = lazyRoute(() => import('./pages/Downloads'))
const Compare = lazyRoute(() => import('./pages/Compare'))
const Security = lazyRoute(() => import('./pages/Security'))
const Repositories = lazyRoute(() => import('./pages/Repositories'))
const Activity = lazyRoute(() => import('./pages/Activity'))
const Reports = lazyRoute(() => import('./pages/Reports'))
const Settings = lazyRoute(() => import('./pages/Settings'))

/**
 * In the order a person is likely to want them.
 *
 * The navigation's own order, with the two drill-downs last: a reader reaches
 * a release or a download by clicking a row, which is always after the listing
 * that carries it.
 */
const ROUTES = [
  Overview, Packages, Downloads, Products, Repositories, Activity, Reports, Settings,
  PackageDetail, DownloadDetail, Compare, Security,
] as unknown as RouteModule<never>[]

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
  const location = useLocation()
  usePreloadRoutes(ROUTES)

  /*
    THE KEY IS THE FIX, and it is one line with a long explanation behind it.

    React Router v7 navigates inside `startTransition`. A transition that
    suspends deliberately keeps the previous UI on screen rather than showing
    the Suspense fallback - which is right for a route whose code is already
    loaded, and is how a click on Repositories left Downloads on the screen
    with the URL already changed and nothing to look at.

    Keying the boundary on the path makes each page its own boundary. There is
    then no previous content for React to hold, so a page whose code has not
    arrived shows that it is loading. Pages whose chunks are already warm - by
    the time anybody clicks, all of them - are unaffected: they resolve
    synchronously and no fallback is ever painted.

    Keyed on pathname rather than on the whole location, so changing a query
    parameter (the repository scope, a filter) does not remount the page and
    lose its state.
  */
  return (
    <Shell>
      <RouteErrorBoundary resetKey={location.pathname}>
        <Suspense key={location.pathname} fallback={<PageLoading />}>
          <Routes location={location}>
            <Route path="/" element={<Overview.Component />} />
            <Route path="/products" element={<Products.Component />} />
            <Route path="/products/:product" element={<Products.Component />} />
            <Route path="/packages" element={<Packages.Component />} />
            <Route path="/packages/:product/:reference" element={<PackageDetail.Component />} />
            {/*
              The old spelling. Links to it exist in chat threads and tickets, so
              it redirects rather than 404ing - and it redirects to the same path
              under the new name so a deep link to one release still lands on it.
            */}
            <Route path="/software" element={<Navigate to="/packages" replace />} />
            <Route path="/software/:product/:reference" element={<LegacyPackageRedirect />} />
            <Route path="/downloads" element={<Downloads.Component />} />
            <Route path="/downloads/:transferId" element={<DownloadDetail.Component />} />
            <Route path="/packages/compare" element={<Compare.Component />} />
            {/* The old path - links to it exist already, so it redirects rather than 404s. */}
            <Route path="/compare" element={<LegacyCompareRedirect />} />
            <Route path="/security" element={<Security.Component />} />
            <Route path="/repositories" element={<Repositories.Component />} />
            <Route path="/activity" element={<Activity.Component />} />
            <Route path="/reports" element={<Reports.Component />} />
            <Route path="/settings" element={<Settings.Component />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </Suspense>
      </RouteErrorBoundary>
    </Shell>
  )
}
