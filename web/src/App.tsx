import { Suspense } from 'react'
import {
  Navigate, Route, Routes, useLocation, useNavigate, useParams, useSearchParams,
} from 'react-router-dom'
import { Shell } from './Shell'
import { useIdentity } from './auth/permissions'
import { identityClaims } from './auth/session'
import { NotFoundPage, PageTransition, StatePage } from './uikit'
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
const Policies = lazyRoute(() => import('./pages/Policies'))
const Repositories = lazyRoute(() => import('./pages/Repositories'))
const Activity = lazyRoute(() => import('./pages/Activity'))
const Reports = lazyRoute(() => import('./pages/Reports'))
const Settings = lazyRoute(() => import('./pages/Settings'))
const Profile = lazyRoute(() => import('./pages/Profile'))

/**
 * In the order a person is likely to want them.
 *
 * The navigation's own order, with the two drill-downs last: a reader reaches
 * a release or a download by clicking a row, which is always after the listing
 * that carries it.
 */
const ROUTES = [
  Overview, Packages, Downloads, Products, Repositories, Activity, Reports, Settings,
  PackageDetail, DownloadDetail, Compare, Security, Policies,
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

/**
 * An address that names no page.
 *
 * This used to redirect to the Overview, which is worse than it sounds: a
 * bookmark that has rotted, a link with a typo in it and a page that was
 * renamed all landed on the same dashboard with nothing said, so the reader
 * concluded the link had worked and reported the wrong problem later. The
 * shared kit's page names the address that was asked for and offers the two
 * ways on.
 *
 * It renders INSIDE the shell: the navigation is still correct and still
 * works, and taking it away for a mistyped path would be the application
 * treating a wrong address as an outage.
 *
 * Going back is offered only where there is somewhere to go back TO. A tab
 * opened directly on a dead link has one history entry, and a button that
 * silently does nothing is worse than no button.
 */
function NotFound() {
  const { pathname, search } = useLocation()
  const navigate = useNavigate()
  const canGoBack = window.history.length > 1
  return (
    <NotFoundPage
      path={pathname + search}
      actions={[
        { label: 'Go to Overview', primary: true, onClick: () => void navigate('/', { replace: true }) },
        ...(canGoBack ? [{ label: 'Go back', onClick: () => void navigate(-1) }] : []),
      ]}
    />
  )
}

/**
 * Signed in, and granted nothing.
 *
 * The Coordinator refuses every route to an account with no roles, which is
 * correct and, left to itself, unreadable: ten pages each reporting their own
 * failure, none of them saying the one thing that is actually true. The person
 * has done nothing wrong and cannot fix it themselves, so the screen says who
 * they are, what is missing, and who has to do something about it.
 *
 * It renders INSIDE the shell. The navigation stays correct and Profile keeps
 * working - that is where the account id lives, which is the thing an
 * administrator needs in order to grant anything, and where signing out is.
 * Taking the chrome away would leave them on a dead end.
 */
function NoAccess() {
  const { who } = useIdentity()
  const claims = identityClaims()
  const navigate = useNavigate()
  return (
    <StatePage
      code="No roles"
      title="This account has no access yet"
      subject={who?.email || claims.email || who?.subject}
      actions={[
        { label: 'Go to your profile', primary: true, onClick: () => void navigate('/profile') },
      ]}
      note="Roles arrive in the sign-in token, so a new one takes effect at the next sign-in rather than on this page."
    >
      Sign-in worked. What is missing is a role: they are granted in the identity
      provider, and this account holds none, so nothing here can be read or
      changed. Send the address above to whoever administers this gateway.
    </StatePage>
  )
}

export function App() {
  const { pathname } = useLocation()
  const { who } = useIdentity()
  usePreloadRoutes(ROUTES)

  /*
    Gated on AUTHENTICATED, not on the permission list alone. A deployment with
    authentication switched off reports `authenticated: false` and permissions
    of `["*"]`, and one that is still fetching reports nothing at all; neither
    is a person who has been granted nothing, and showing this page to either
    would be the application inventing a problem.
  */
  const noAccess = Boolean(who?.authenticated && (who.permissions ?? []).length === 0)

  /*
    ONE KEY, doing two jobs.

    The first is the entrance animation's: a path with a parameter in it - one
    release, one download - is the same page showing something else, and
    re-running the entrance when somebody steps between two releases would
    animate a change that is not a change of screen. Two segments is where
    every route here stops being a different page and starts being a different
    subject.

    The second is the Suspense boundary's, and it is a bug fix. React Router
    v7 navigates inside `startTransition`, and a transition that suspends
    deliberately keeps the PREVIOUS UI on screen rather than showing the
    fallback - which is right for a route whose code is already loaded, and is
    how a click on Repositories left Downloads on the screen with the URL
    already changed and nothing to look at. Keying the boundary gives each page
    its own, so there is no previous content for React to hold and a page whose
    code has not arrived says that it is loading.

    The same key serves both because they are the same question - "is this a
    different screen?" - and two keys that could disagree about it would
    eventually animate one thing while remounting another.
  */
  const page = pathname.split('/').slice(0, 3).join('/')

  /*
    A SEPARATE ROUTE TABLE, not an extra entry in the one below.

    The obvious spelling - a `path="*"` route added before the others - does
    nothing at all: React Router ranks routes by how specific they are rather
    than taking the first that matches, and `*` scores lowest, so every real
    page still won and the gate only ever showed on an address that already
    404ed. Two routes and no competition is the version that works.

    Profile stays reachable because it is the one page that still functions:
    it reads /whoami, which is never gated, and it carries the account id an
    administrator needs plus the way to sign out.
  */
  if (noAccess) {
    return (
      <Shell>
        <Suspense fallback={<PageLoading />}>
          <Routes>
            <Route path="/profile" element={<Profile.Component />} />
            <Route path="*" element={<NoAccess />} />
          </Routes>
        </Suspense>
      </Shell>
    )
  }

  return (
    <Shell>
      <RouteErrorBoundary resetKey={page}>
        <Suspense key={page} fallback={<PageLoading />}>
          <PageTransition routeKey={page}>
            <Routes>
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
              {/*
                The rulebook. Reachable from a release's Compliance tab and by
                link, deliberately NOT a tenth nav entry: the shell's nine are
                the nine, and this is a reference somebody opens from a finding
                rather than a place they go.
              */}
              <Route path="/policies" element={<Policies.Component />} />
              <Route path="/repositories" element={<Repositories.Component />} />
              <Route path="/activity" element={<Activity.Component />} />
              <Route path="/reports" element={<Reports.Component />} />
              <Route path="/settings" element={<Settings.Component />} />
              <Route path="/profile" element={<Profile.Component />} />
              {/*
                Everything else. A redirect here would hide the mistake; see
                `NotFound` above for why that cost more than it saved.
              */}
              <Route path="*" element={<NotFound />} />
            </Routes>
          </PageTransition>
        </Suspense>
      </RouteErrorBoundary>
    </Shell>
  )
}
