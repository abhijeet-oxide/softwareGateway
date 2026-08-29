import type { Problem, ErrorCode } from './types'

/**
 * The HTTP client, mirroring pkg/apis/softwaregateway/v1/client.go.
 *
 * Same-origin by construction: the Coordinator installs no CORS middleware, so
 * the browser reaches `/api/v1/...` on its own origin and Vite proxies it in
 * development.
 */

/**
 * An error the Coordinator described, as opposed to one the network produced.
 *
 * `code` is the machine-readable RFC 9457 value. Branch on it - never on the
 * HTTP status and never on the prose, which is written for people and will be
 * reworded.
 */
export class ApiError extends Error {
  readonly code: ErrorCode
  readonly status: number
  readonly requestId: string | undefined

  constructor(problem: Problem, status: number) {
    super(problem.detail || problem.title || `request failed with ${status}`)
    this.name = 'ApiError'
    this.code = problem.code
    this.status = status
    this.requestId = problem.requestId
  }
}

/** The Coordinator could not be reached at all, which calls for a different fix. */
export class UnreachableError extends Error {
  constructor(cause: unknown) {
    super('The Coordinator could not be reached. Check that it is running and reachable from this host.')
    this.name = 'UnreachableError'
    this.cause = cause
  }
}

const BASE = '/api/v1'

/**
 * How many of our own calls may be in flight at once.
 *
 * # Why there is a ceiling at all
 *
 * A browser opens six connections per host over HTTP/1.1, and everything
 * shares them: our reads, and the JavaScript chunk for whatever page the
 * reader has just clicked on. Several pages here fan out PER PRODUCT - the
 * Downloads page asks three questions about each of them - so an estate with a
 * dozen products opens thirty-odd requests the moment that page mounts. Against
 * a Coordinator busy streaming a release those are not fast, and while they
 * hold every connection, a click on another page has nowhere to fetch its code
 * from. The URL changes and the screen does not, which is exactly the symptom
 * this ceiling and the keyed boundary in `../routing` were both written for.
 *
 * Four rather than six, so two connections are always free for the things that
 * are not us: a page's chunk, a font, an export the reader started.
 *
 * # Why this is not a rate limit
 *
 * Nothing is dropped, delayed on a timer, or deduplicated here. A call that
 * arrives while four are running waits for one of them to finish and then goes
 * immediately, in the order it asked. Over HTTP/2, where the ceiling would be
 * unnecessary, it costs one queue hop and nothing else.
 */
const MAX_IN_FLIGHT = 4

let inFlight = 0
const waiting: (() => void)[] = []

/** Waits for a slot. Resolves immediately whenever one is free. */
function acquire(): Promise<void> {
  if (inFlight < MAX_IN_FLIGHT) {
    inFlight++
    return Promise.resolve()
  }
  return new Promise((resolve) => waiting.push(resolve))
}

/**
 * Hands the slot on, to the longest waiter.
 *
 * FIFO deliberately. A stack would starve the first caller under sustained
 * load, and the first caller is usually the page the reader is looking at.
 */
function release(): void {
  const next = waiting.shift()
  if (next) {
    next()
    return
  }
  inFlight--
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response
  await acquire()
  try {
    response = await fetch(BASE + path, {
      ...init,
      headers: {
        Accept: 'application/json',
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...init?.headers,
      },
    })
  } catch (cause) {
    throw new UnreachableError(cause)
  } finally {
    // The slot is held for the HEADERS, not for the body. Everything this
    // client reads is JSON small enough that the difference does not matter,
    // and releasing here means a slow parse cannot hold a connection nobody is
    // using any more.
    release()
  }

  if (!response.ok) {
    // A problem document is the contract, but a proxy or a crash can return
    // something else entirely. Falling back to a synthetic problem keeps the
    // error surface one type, so no caller has to handle "not even an error".
    let problem: Problem
    try {
      problem = (await response.json()) as Problem
      if (!problem?.code) throw new Error('not a problem document')
    } catch {
      problem = {
        code: response.status >= 500 ? 'INTERNAL' : 'INVALID_ARGUMENT',
        detail: `The server answered ${response.status} without an error document.`,
      }
    }
    throw new ApiError(problem, response.status)
  }

  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

/** Drops empty values so an unset filter contributes no query parameter. */
export function query(params: Record<string, string | number | boolean | undefined | null>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '' || value === false) continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}

/**
 * A package reference can contain a slash, which cannot survive a URL path
 * segment - %2F is decoded before routing, so `orbs/core:v1` would arrive as
 * two segments and match nothing. The repository moves to the query string,
 * exactly as the Go client does it.
 */
export function packageRef(ref: string): { segment: string; query: string } {
  const lastColon = ref.lastIndexOf(':')
  const slash = ref.indexOf('/')
  if (slash === -1 || lastColon === -1 || lastColon < slash) {
    return { segment: ref, query: '' }
  }
  const repository = ref.slice(0, lastColon)
  const tag = ref.slice(lastColon + 1)
  return { segment: tag, query: `?repository=${encodeURIComponent(repository)}` }
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'POST', body: JSON.stringify(body ?? {}) }),
}

export const path = {
  encode: encodeURIComponent,
}
