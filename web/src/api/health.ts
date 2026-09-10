import { connection, type Probe, type ProbeResult } from '../uikit'

/**
 * HOW THIS APPLICATION ASKS WHETHER ITS SERVICE IS THERE.
 *
 * The monitor in the shared kit owns the scheduling, the backoff and every
 * screen that shows the answer; it deliberately knows nothing about HTTP or
 * about this deployment. This file is the other half: one request, and the
 * rules for reading what comes back.
 *
 * # Why the version endpoint rather than /healthz
 *
 * Because /healthz is not on the path this application actually uses. It lives
 * at the root rather than under /api/v1, so whether a browser can reach it
 * depends on how the proxy in front of the deployment is routed - and an
 * address that answers 404 for the probe while the API works perfectly would
 * report an outage that does not exist. The version endpoint is on the same
 * prefix as every other read on screen, which makes it the only honest answer
 * to "can this page talk to its service": if the probe can reach it, so can
 * the page.
 *
 * It is also what the boot gate already asks, so a deployment that gets past
 * the boot screen is a deployment where this probe is known to work.
 *
 * # No credentials, on purpose
 *
 * REACHABILITY IS NOT AUTHORISATION. The probe sends no token and reads a 401
 * as a healthy answer, because a service that refuses an anonymous caller has
 * demonstrably received the request, parsed it and replied - which is the
 * entire question. Sending the session's token instead would put a probe that
 * runs every forty-five seconds into the token renewal path, where an expiry
 * during an outage could start a sign-in redirect nobody asked for.
 */
const PROBE_PATH = '/api/v1/system/version'

/**
 * What each answer means.
 *
 * The three-way split matters because the surfaces say different things about
 * each, and the difference is what stops an interface sending somebody to
 * check a network cable during a planned restart:
 *
 *   503  the service answered and is standing down - a restart, a maintenance
 *        window, a replica pulled out of rotation. Nothing is broken.
 *   502/504  the web tier answered because the service did not. The service is
 *        down rather than merely busy, and the tier in front of it is fine.
 *   anything else  it answered. Even 401 and 500 are answers, and neither is a
 *        reachability problem.
 */
export const probeService: Probe = async (signal) => {
  const response = await fetch(PROBE_PATH, {
    method: 'GET',
    // A cached 200 would let a browser report a service that has been gone for
    // ten minutes as healthy, which is the one failure a health check may not
    // have.
    cache: 'no-store',
    headers: { Accept: 'application/json' },
    signal,
  })

  if (response.status === 503) {
    return {
      verdict: 'unavailable',
      status: 503,
      detail: `${PROBE_PATH} answered 503: the service is not accepting requests.`,
    }
  }
  if (response.status === 502 || response.status === 504) {
    return {
      verdict: 'unreachable',
      status: response.status,
      detail: `${PROBE_PATH} answered ${response.status}: the web tier could not reach the service.`,
    }
  }

  // An address that answers 200 with a page rather than a document is not this
  // API. It is a proxy that routed the request somewhere else, or a static
  // bundle being served for every path - and reporting that as healthy is how
  // an interface ends up insisting everything is fine at a screen full of
  // failures.
  const type = response.headers.get('Content-Type') ?? ''
  if (response.ok && !type.includes('json')) {
    return {
      verdict: 'unreachable',
      status: response.status,
      detail: `${PROBE_PATH} answered ${response.status} with ${type || 'no content type'} rather than a version document. This address may not be in front of the service.`,
    }
  }

  return { verdict: 'ok', status: response.status } satisfies ProbeResult
}

/**
 * Points the shared monitor at this deployment, and says what happens when the
 * service comes back.
 *
 * `refetch` is the half that makes an outage recoverable rather than merely
 * survivable. Everything on screen during an outage is as old as the last
 * successful read, and a page that simply stopped failing would go on showing
 * those numbers indefinitely - so the moment the service answers again, every
 * query the reader is actually looking at is refetched. Nothing is unmounted
 * and nothing is navigated: the screen fills back in underneath them.
 */
export function watchService(refetch: () => void): void {
  connection.configure({ probe: probeService })
  connection.onRestore(refetch)
}
