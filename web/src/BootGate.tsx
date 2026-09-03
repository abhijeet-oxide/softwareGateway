import { useEffect, useState, type ReactNode } from 'react'
import { ReloadOutlined } from './icons'
import { useQuery } from '@tanstack/react-query'
import { api, ApiError } from './api/client'
import type { VersionResponse } from './api/types'
import brand from './brand'
import { BootSplash, MaintenancePage, ServiceDownArt, StatusScreen } from './uikit'

/**
 * BootGate answers the first question the application has to ask: is the
 * Coordinator there?
 *
 * Until we know, nothing else may render - every page would otherwise fire its
 * own reads into the dark and paint a screen made of failed requests, which is
 * exactly what this application used to do. So it boots behind one probe:
 *
 *   probing     -> a quiet branded screen (the mark, nothing that looks broken)
 *   503         -> the maintenance page, retrying on its own
 *   no answer   -> the service-unavailable page below, retrying on its own
 *   answered    -> the application, for the rest of the session
 *
 * The first two failures are the same wait and a different fact, and they are
 * worth separating: a Coordinator that answers 503 is a Coordinator that is
 * THERE and has been told to stand down, which nobody needs to escalate. One
 * that does not answer at all may be a service that fell over, or a network
 * that cannot reach it, and that is worth somebody looking at. A single screen
 * for both would make the planned case read like the unplanned one.
 *
 * Once the service has answered even once the gate steps aside for good: a
 * later blip is a temporary outage, and a page's own error state handles that
 * without taking the workspace away from the person using it.
 *
 * Both screens are the shared design system's, so this moment - the first
 * thing anybody sees of the product, and what they see on its worst day -
 * looks and behaves the same in every tool on the platform.
 */

const RETRY_SECONDS = 15

export function BootGate({ children }: { children: ReactNode }) {
  // The probe retries on a timer owned by the query, not by the screen below
  // it. A screen that fired a retry when it MOUNTED span: the refetch cleared
  // the error, the gate swapped back to the splash, the screen unmounted, the
  // fetch failed, it mounted again and retried again - several requests a
  // second against a service that is already having a bad day.
  const q = useQuery({
    queryKey: ['boot', 'version'],
    queryFn: () => api.get<VersionResponse>('/system/version'),
    staleTime: Infinity,
    retry: false,
    refetchInterval: (query) => (query.state.data ? false : RETRY_SECONDS * 1000),
  })

  // A probe that answers in a few hundred milliseconds should not flash a
  // splash screen; hold the boot screen back briefly so a fast start is silent.
  const [showSplash, setShowSplash] = useState(false)
  useEffect(() => {
    const t = setTimeout(() => setShowSplash(true), 350)
    return () => clearTimeout(t)
  }, [])

  // q.data survives later failures, so its presence means "the service has
  // answered at least once" - the gate's one-way door.
  if (q.data) return <>{children}</>
  // Latched, deliberately: a retry in flight briefly looks like "no answer
  // yet", and swapping the failure screen back to a splash every fifteen
  // seconds reads as a page that cannot make up its mind.
  if (q.isError || q.failureCount > 0) {
    // Branch on the CODE, never on the status or the prose - see api/client.ts.
    // A 503 without a problem document maps to UNAVAILABLE there, which is what
    // a proxy in front of the Coordinator answers during a planned window.
    if (q.error instanceof ApiError && q.error.code === 'UNAVAILABLE') {
      return <UnderMaintenance onRetry={() => void q.refetch()} retrying={q.isFetching} />
    }
    return <ServiceUnavailable onRetry={() => void q.refetch()} retrying={q.isFetching} />
  }
  return showSplash ? <BootSplash brand={brand} /> : null
}

/**
 * The Coordinator answered, and what it said was "not right now".
 *
 * Planned work is not an outage and must not be dressed as one: nothing here
 * is red, nothing suggests a fault, and the only thing being asked for is
 * time. The page is the shared kit's, so a maintenance window looks the same
 * in every tool on the platform.
 */
function UnderMaintenance({ onRetry, retrying }: { onRetry: () => void; retrying: boolean }) {
  const left = useRetryCountdown(retrying)
  return (
    <MaintenancePage
      full
      brand={brand}
      actions={[
        {
          label: 'Check again now',
          primary: true,
          icon: <ReloadOutlined />,
          loading: retrying,
          onClick: onRetry,
        },
      ]}
      note={
        <span role="status" aria-live="polite">
          {retrying ? 'Checking' : `Checking again in ${left}s`}
        </span>
      }
    >
      The Coordinator is up and has been placed in maintenance. Downloads
      already in progress are unaffected; this screen clears itself as soon as
      the window ends.
    </MaintenancePage>
  )
}

/**
 * The seconds until the next automatic attempt.
 *
 * DISPLAY only: the retry itself belongs to the query above, and a screen that
 * fired its own on mount is what span the gate - the refetch cleared the error,
 * the gate swapped back to the splash, the screen unmounted, the fetch failed,
 * it mounted and retried again, several requests a second against a service
 * already having a bad day. Both failure screens count down, so the count is
 * here rather than in either of them.
 */
function useRetryCountdown(retrying: boolean): number {
  const [left, setLeft] = useState(RETRY_SECONDS)
  useEffect(() => {
    if (retrying) {
      setLeft(RETRY_SECONDS)
      return
    }
    const t = setInterval(() => setLeft((n) => (n <= 1 ? RETRY_SECONDS : n - 1)), 1000)
    return () => clearInterval(t)
  }, [retrying])
  return left
}

function ServiceUnavailable({
  onRetry,
  retrying,
}: {
  onRetry: () => void
  retrying: boolean
}) {
  const left = useRetryCountdown(retrying)

  return (
    <StatusScreen
      brand={brand}
      art={<ServiceDownArt size={140} />}
      title="Service unavailable"
      actions={[
        {
          label: 'Try again now',
          primary: true,
          icon: <ReloadOutlined />,
          loading: retrying,
          onClick: onRetry,
        },
      ]}
      note={
        <span role="status" aria-live="polite">
          {retrying ? 'Checking' : `Checking again in ${left}s`}
        </span>
      }
    >
      The Coordinator did not respond. It may be starting up, restarting, or
      unreachable from this host. Nothing is affected by this - no download was
      cancelled and no configuration was changed.
    </StatusScreen>
  )
}
