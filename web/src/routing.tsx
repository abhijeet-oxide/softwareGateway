import { Component, lazy, useEffect, type ComponentType, type ReactNode } from 'react'
import brand from './brand'
import { ErrorPage, LoadingPage } from './uikit'

/**
 * How a page gets onto the screen.
 *
 * # The bug this file exists for
 *
 * Clicking Repositories while on Downloads changed the URL and left the
 * Downloads page on screen. Clicking two or three more links eventually got
 * there. Nothing was broken and nothing said anything.
 *
 * Three things compose into that:
 *
 *  1. Every page is `React.lazy`, so a first visit to one is a NETWORK FETCH
 *     for its chunk.
 *
 *  2. React Router v7 wraps navigation in `startTransition`. That is a good
 *     default - it stops a fast route flashing a spinner - and it means a
 *     transition that SUSPENDS keeps the old UI on screen rather than showing
 *     the Suspense fallback. The router has moved; React is still painting the
 *     previous page, deliberately, and will keep doing so for as long as the
 *     chunk takes.
 *
 *  3. The chunk takes a long time on exactly the pages where this was noticed.
 *     A browser allows six connections per host, and Downloads holds several
 *     per product plus two polled transfer listings; against a Coordinator busy
 *     streaming a release, those requests are slow, and the chunk request
 *     queues behind them. Clicking more links "fixes" it because time passes
 *     and a connection frees.
 *
 * So: the page is preloaded before it is needed, the boundary is keyed so a
 * transition cannot silently hold the old page, and a chunk that fails to load
 * is retried rather than being remembered as broken forever.
 */

/**
 * A route module, loadable more than once.
 *
 * # Why the retry, and why the cache
 *
 * `React.lazy` calls its factory ONCE and remembers the promise - including a
 * REJECTED one. So a single failed chunk fetch (a dropped connection, a deploy
 * that replaced the file mid-session) breaks that route permanently: every
 * later navigation to it re-throws the same rejection, and only a full page
 * reload clears it. Retrying inside the factory means a transient failure costs
 * a second or two rather than the session.
 *
 * The resolved module is cached here as well, which is what makes `preload`
 * free: the eager load and the render share one promise, so preloading a page
 * the reader never visits costs one fetch and nothing else.
 */
export interface RouteModule<P = Record<string, never>> {
  Component: ComponentType<P>
  /** Fetch the chunk now, without rendering anything. Safe to call repeatedly. */
  preload: () => Promise<unknown>
}

const RETRY_DELAYS_MS = [250, 1000]

async function loadWithRetry<T>(load: () => Promise<T>): Promise<T> {
  let lastError: unknown
  for (let attempt = 0; attempt <= RETRY_DELAYS_MS.length; attempt++) {
    try {
      return await load()
    } catch (error) {
      lastError = error
      const delay = RETRY_DELAYS_MS[attempt]
      if (delay === undefined) break
      await new Promise((resolve) => setTimeout(resolve, delay))
    }
  }
  throw lastError
}

export function lazyRoute<P = Record<string, never>>(
  load: () => Promise<{ default: ComponentType<P> }>,
): RouteModule<P> {
  let pending: Promise<{ default: ComponentType<P> }> | undefined
  const once = () => (pending ??= loadWithRetry(load).catch((error) => {
    // Forget a rejection, so the next attempt is a real attempt. Keeping it
    // would reproduce the failure `React.lazy` has on its own.
    pending = undefined
    throw error
  }))

  return {
    Component: lazy(once) as unknown as ComponentType<P>,
    preload: () => once().catch(() => {
      // A preload is an optimisation. Its failure must never reach anybody:
      // the render-time load will try again and report properly if it still
      // cannot get there.
    }),
  }
}

/**
 * Warms every route's chunk once the first page has settled.
 *
 * # Why eagerly rather than on hover
 *
 * Hover-preloading is the fashionable answer and it is the wrong one here.
 * This is an operations console with nine pages, opened on a desk and left
 * open; the chunks are small, they are fetched once per deploy, and the
 * connection they compete for is the one carrying the polling. Fetching them
 * all while nothing is happening removes the competition entirely, where
 * hover-preloading only shortens it - and does nothing at all for a person
 * driving the navigation from the keyboard.
 *
 * `requestIdleCallback` where there is one, so this never delays the page the
 * reader actually asked for. One at a time, so the warm-up cannot itself
 * become the six requests that starve something.
 */
export function usePreloadRoutes(routes: RouteModule<never>[]) {
  useEffect(() => {
    let cancelled = false

    const warm = async () => {
      for (const route of routes) {
        if (cancelled) return
        await route.preload()
      }
    }

    const idle = window.requestIdleCallback
    if (idle) {
      const handle = idle(() => void warm(), { timeout: 4000 })
      return () => {
        cancelled = true
        window.cancelIdleCallback?.(handle)
      }
    }

    const timer = setTimeout(() => void warm(), 1200)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
    // The route list is a module constant. Depending on its identity would
    // re-run this on every render of the shell.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
}

/**
 * What is shown while a page's code is on its way.
 *
 * The shared kit's loading page: named work rather than a bare spinner,
 * because the two possible waits are very different lengths and the reader
 * should be able to tell that something is happening at all. It is
 * indeterminate on purpose - a chunk in flight has no denominator, and a bar
 * that filled would be stating a position nobody has.
 *
 * It renders inside the shell, so the navigation stays on screen and a reader
 * who has changed their mind can go somewhere else rather than waiting for a
 * page they no longer want. See the file comment: without a keyed boundary
 * around this it is never shown at all - the router's transition keeps the
 * PREVIOUS page up instead, which is the bug this file exists for.
 */
export function PageLoading() {
  return <LoadingPage label="Loading this page" />
}

/**
 * The error a broken chunk produces, said out loud.
 *
 * A page whose code cannot be fetched is not a page that can retry itself into
 * existence - the module never evaluated - so the offer is a reload, which is
 * the thing that actually works. Without this the failure surfaces as a blank
 * area and an exception in a console nobody has open.
 *
 * It is the shared kit's error page rather than a paragraph written here, so
 * the worst screen in the application is the one screen nobody improvised. The
 * message is carried through into its details rather than dropped: the reader
 * is usually the person who would have fixed it from the message alone, and a
 * failure quoted into a ticket without one costs a round trip to recover.
 */
export class RouteErrorBoundary extends Component<
  { children: ReactNode; resetKey: string },
  { error: Error | undefined }
> {
  state: { error: Error | undefined } = { error: undefined }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidUpdate(previous: { resetKey: string }) {
    // Navigating away from a page that failed clears the failure. Otherwise
    // one broken chunk would hold the whole shell on an error screen.
    if (previous.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: undefined })
    }
  }

  render() {
    if (!this.state.error) return this.props.children
    return (
      <ErrorPage
        title="This page could not be loaded"
        detail={this.state.error.message}
        actions={[{ label: 'Reload', primary: true, onClick: () => window.location.reload() }]}
      >
        Its code did not arrive. That is almost always the connection to this
        Coordinator, or a new version having been deployed while this tab was
        open - reloading picks up the current one.
      </ErrorPage>
    )
  }
}

/**
 * The last boundary, outside the shell.
 *
 * `RouteErrorBoundary` covers the page; this covers everything the page sits
 * in - the navigation, the bar, the providers. A throw up there is rare and
 * unrecoverable, and without a boundary it renders as a white document with an
 * exception in a console nobody has open, which is indistinguishable from a
 * server that stopped answering.
 *
 * It takes the viewport and names the product itself, because at this point
 * there is no chrome left to say which application the reader is looking at.
 * Nothing here can navigate: the router may be the thing that failed, so the
 * two offers are a reload and the root address, both of which work without it.
 */
export class AppErrorBoundary extends Component<{ children: ReactNode }, { error: Error | undefined }> {
  state: { error: Error | undefined } = { error: undefined }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  render() {
    if (!this.state.error) return this.props.children
    return (
      <ErrorPage
        full
        brand={brand}
        title="This application stopped"
        detail={this.state.error.message}
        actions={[
          { label: 'Reload', primary: true, onClick: () => window.location.reload() },
          { label: 'Go to Overview', href: '/' },
        ]}
      >
        An error reached the top of the interface, so nothing further can be
        drawn. No download was cancelled and no configuration was changed by
        this - reloading restores the session.
      </ErrorPage>
    )
  }
}
