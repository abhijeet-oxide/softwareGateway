import { useEffect, useState, type ReactNode } from 'react'
import { ReloadOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { api } from './api/client'
import type { VersionResponse } from './api/types'
import brand from './brand'
import { BootSplash, ServiceDownArt, StatusScreen } from './uikit'

/**
 * BootGate answers the first question the application has to ask: is the
 * Coordinator there?
 *
 * Until we know, nothing else may render - every page would otherwise fire its
 * own reads into the dark and paint a screen made of failed requests, which is
 * exactly what this application used to do. So it boots behind one probe:
 *
 *   probing   -> a quiet branded screen (the mark, nothing that looks broken)
 *   no answer -> the service-unavailable page below, retrying on its own
 *   answered  -> the application, for the rest of the session
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
    return <ServiceUnavailable onRetry={() => void q.refetch()} retrying={q.isFetching} />
  }
  return showSplash ? <BootSplash brand={brand} /> : null
}

function ServiceUnavailable({
  onRetry,
  retrying,
}: {
  onRetry: () => void
  retrying: boolean
}) {
  // The countdown is DISPLAY: it says the page is working on it, so nobody has
  // to wonder whether to keep clicking. The retry itself is the query's, above.
  const [left, setLeft] = useState(RETRY_SECONDS)
  useEffect(() => {
    if (retrying) {
      setLeft(RETRY_SECONDS)
      return
    }
    const t = setInterval(() => setLeft((n) => (n <= 1 ? RETRY_SECONDS : n - 1)), 1000)
    return () => clearInterval(t)
  }, [retrying])

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
          {retrying ? 'Checking…' : `Checking again in ${left}s`}
        </span>
      }
    >
      {brand.appName} can&rsquo;t reach the Coordinator right now. It may be starting up,
      restarting, or briefly under maintenance. Nothing is affected by this - no download was
      cancelled and no configuration was changed.
    </StatusScreen>
  )
}
