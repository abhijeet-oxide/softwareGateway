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

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response
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
