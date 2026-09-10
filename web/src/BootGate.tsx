import { useEffect, useState, useSyncExternalStore, type ReactNode } from 'react'
import { ReloadOutlined } from './icons'
import { useQuery } from '@tanstack/react-query'
import { api, ApiError } from './api/client'
import type { VersionResponse } from './api/types'
import { signInStatus, signOut, subscribeSignIn } from './auth/session'
import brand from './brand'
import {
  AccessDeniedArt,
  BootSplash,
  c,
  ErrorArt,
  MaintenancePage,
  ServiceDownArt,
  SessionExpiredArt,
  SignedOutArt,
  StatusScreen,
} from './uikit'

/**
 * BootGate answers the first question the application has to ask: is the
 * Coordinator there?
 *
 * Until we know, nothing else may render - every page would otherwise fire its
 * own reads into the dark and paint a screen made of failed requests, which is
 * exactly what this application used to do. So it boots behind one probe:
 *
 *   probing     -> a quiet branded screen (the mark, nothing that looks broken)
 *   401         -> the sign-in, which the API client has already started
 *   403         -> the no-access page: signed in, and not for this
 *   503         -> the maintenance page, retrying on its own
 *   5xx / silence -> the service-unavailable page below, retrying on its own
 *   anything else -> the unexpected-answer page, which names what arrived
 *   answered    -> the application, for the rest of the session
 *
 * EVERY ONE OF THOSE IS A DIFFERENT FACT, and the reason there are six screens
 * rather than two is that they used to be two. A 401 from an unauthenticated
 * browser - the normal first second of every visit to a Coordinator with
 * authentication switched on - rendered "The Coordinator did not respond",
 * which was false in every word: it responded, promptly, and said exactly what
 * was wrong. Somebody reading that screen has no reason to suspect a sign-in
 * is missing, and every reason to go and look at a service that is perfectly
 * healthy. An outage screen is for an outage.
 *
 * A Coordinator that answers 503 is likewise THERE and has been told to stand
 * down, which nobody needs to escalate; one that does not answer at all may be
 * a service that fell over or a network that cannot reach it, and that is
 * worth somebody looking at.
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
    const retry = { onRetry: () => void q.refetch(), retrying: q.isFetching }
    // Branch on the CODE, never on the status or the prose - see api/client.ts.
    // A 503 without a problem document maps to UNAVAILABLE there, which is what
    // a proxy in front of the Coordinator answers during a planned window.
    if (q.error instanceof ApiError) {
      const { code, status } = q.error
      if (code === 'UNAVAILABLE') return <UnderMaintenance {...retry} />
      // The API client has already started a sign-in by the time this renders.
      // This screen says what is happening and what to do when it cannot.
      if (code === 'UNAUTHENTICATED') return <SignInRequired />
      if (code === 'PERMISSION_DENIED') return <NoAccess detail={q.error.message} />
      // An answer that is neither a fault nor a refusal. Rare, and worth its
      // own screen precisely because it is rare: a 404 here means the address
      // this UI was served from is not in front of a Coordinator at all, which
      // no amount of waiting for a service to come back will fix.
      if (status > 0 && status < 500) {
        return <UnexpectedAnswer status={status} detail={q.error.message} {...retry} />
      }
      return <ServiceUnavailable answered={status} {...retry} />
    }
    return <ServiceUnavailable {...retry} />
  }
  return showSplash ? <BootSplash brand={brand} /> : null
}

/**
 * The Coordinator wants a token and this browser has none.
 *
 * By the time this renders, api/client.ts has already called requireSignIn and
 * the browser is on its way to the identity provider - so the ordinary case is
 * the splash, for the half second before the page is replaced. The other two
 * states are the ones that need words, because in both of them redirecting
 * again would achieve nothing.
 */
function SignInRequired() {
  const status = useSyncExternalStore(subscribeSignIn, signInStatus)

  if (status === 'unconfigured') {
    return (
      <StatusScreen
        brand={brand}
        art={<SignedOutArt size={140} />}
        title="Sign-in is not configured"
        actions={[{ label: 'Try again', primary: true, onClick: () => window.location.reload() }]}
      >
        The Coordinator requires an authenticated caller, and this deployment
        published no identity provider to obtain a token from. An administrator
        sets OIDC_ISSUER and OIDC_CLIENT_ID on the web service, or runs the
        seeder that publishes them.
      </StatusScreen>
    )
  }

  if (status === 'rejected') {
    return (
      <StatusScreen
        brand={brand}
        art={<SessionExpiredArt size={140} />}
        title="The sign-in was not accepted"
        actions={[{ label: 'Sign out and try again', primary: true, onClick: () => void signOut() }]}
      >
        The identity provider issued a token and the Coordinator refused it.
        The two are configured against different issuers, audiences or clocks;
        signing in again produces the same token and the same refusal. An
        administrator compares SWGW_AUTH_ISSUER on the Coordinator with the
        issuer this page was pointed at.
      </StatusScreen>
    )
  }

  return <BootSplash brand={brand} label="Signing in" />
}

/**
 * Signed in, and not for this. A different fact from a missing sign-in, and
 * emphatically not an outage: the only way out is somebody granting a role,
 * which is why this screen offers no retry.
 */
function NoAccess({ detail }: { detail: string }) {
  return (
    <StatusScreen
      brand={brand}
      art={<AccessDeniedArt size={140} />}
      title="No access to this Coordinator"
      actions={[{ label: 'Sign in as someone else', onClick: () => void signOut() }]}
    >
      {detail} An administrator grants a role on the tenant or on a product.
    </StatusScreen>
  )
}

/**
 * The address answered, and not as a Coordinator.
 *
 * Almost always a proxy: this UI served from an origin whose /api/v1 goes
 * somewhere else, or nowhere. Retrying is offered because a misrouted request
 * during a deployment does resolve itself, and the status is named because it
 * is the one fact that identifies the fault.
 */
function UnexpectedAnswer({
  status,
  detail,
  onRetry,
  retrying,
}: {
  status: number
  detail: string
  onRetry: () => void
  retrying: boolean
}) {
  const left = useRetryCountdown(retrying)
  return (
    <StatusScreen
      brand={brand}
      art={<ErrorArt size={140} />}
      title="Unexpected answer from the Coordinator"
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
      <>
        <div>
          This address answered, and did not answer as {brand.appName}. Waiting
          will not resolve it, and the deployment needs somebody to look at it.
        </div>
        <div style={{ marginTop: 12, fontSize: 12, color: c.text3, lineHeight: 1.5 }}>
          The version endpoint answered {status} rather than a version. This
          address may not be in front of a Coordinator: check what /api/v1 is
          proxied to. {detail}
        </div>
      </>
    </StatusScreen>
  )
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
      {brand.appName} is up and has been placed in maintenance. Downloads
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

/**
 * The unplanned case, and ONLY the unplanned case: nothing answered, or a 5xx
 * came back. `answered` separates THREE facts, because the sentence a reader
 * quotes into a ticket is what decides who picks it up. Silence is a network
 * or a stopped container; a 502 or 504 is the web tier saying it could not
 * reach the Coordinator, which is a Coordinator that is down rather than a
 * Coordinator that failed; a 500 is the Coordinator itself failing a request
 * it did receive, which is a bug and belongs to a different team.
 */
function ServiceUnavailable({
  answered,
  onRetry,
  retrying,
}: {
  answered?: number
  onRetry: () => void
  retrying: boolean
}) {
  const left = useRetryCountdown(retrying)

  return (
    <StatusScreen
      brand={brand}
      art={<ServiceDownArt size={140} />}
      title={`${brand.appName} is not responding`}
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
      {/*
        TWO SENTENCES FOR TWO READERS, in that order and not the other way
        round.

        This screen used to open with "The Coordinator did not respond. It may
        be starting up, restarting, or unreachable from this host" - which is
        an accurate report written for the person who runs the deployment, and
        which arrives in front of the release manager who simply opened a
        bookmark. They do not know what a Coordinator is, have no host to be
        reachable from, and cannot act on any of it; what it tells them is that
        the product is broken and speaking a language they do not have.

        So the first line says the thing anybody can act on: it is not
        answering, nothing was lost, and the page is already checking. The
        diagnosis stays - this screen is also the one an operator gets a
        screenshot of - and moves below it, quieter, where somebody looking for
        it will find it and nobody else has to read it.
      */}
      <>
        <div>
          The service did not answer, so nothing can be loaded yet. This page
          keeps checking on its own and continues as soon as it does. Nothing is
          affected in the meantime - no download was cancelled and no
          configuration was changed.
        </div>
        <div style={{ marginTop: 12, fontSize: 12, color: c.text3, lineHeight: 1.5 }}>
          {!answered
            ? 'The Coordinator did not respond: it may be starting up, restarting, or unreachable from this host.'
            : answered === 500
              ? 'The Coordinator answered 500: it received the request and failed on it.'
              : `The web tier answered ${answered}: it could not reach the Coordinator.`}
        </div>
      </>
    </StatusScreen>
  )
}
