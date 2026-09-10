import { ApiError, UnreachableError } from './client'
import type { ErrorCode } from './types'

/**
 * ONE reading of a failure, for every surface that reports one.
 *
 * # Why this exists
 *
 * Because a failure was being described in forty places and described
 * differently in each. `catch (e) { message.error(e.message) }` was the common
 * shape, and it throws away everything the Coordinator went to the trouble of
 * sending: the machine-readable code, the request id that ties the screen to a
 * log line, and the difference between "you may not" and "it is broken". What
 * reached the reader was one sentence with no way to act on it - and in several
 * places, nothing at all.
 *
 * So every failure is read HERE, once, into the same four facts, and the
 * surfaces - a toast, an inline band, a whole page - differ only in how much
 * room they have to show them.
 */

/** What KIND of problem this is, which decides what the reader is offered. */
export type FailureKind =
  /** Signed in, and not permitted. Retrying cannot help; a role can. */
  | 'denied'
  /** Not signed in. The client is already doing something about it. */
  | 'unauthenticated'
  /** The Coordinator could not be reached at all. */
  | 'unreachable'
  /** The Coordinator is up and standing down - a planned window. */
  | 'unavailable'
  /** The request was wrong: a bad argument, a missing thing, a bad state. */
  | 'rejected'
  /** The Coordinator received it and failed on it. */
  | 'faulty'

export interface Failure {
  kind: FailureKind
  /** A heading: what happened, in four or five words. */
  title: string
  /** The Coordinator's own sentence, which is the part worth reading. */
  detail: string
  /** The RFC 9457 code, for the reader who is going to grep for it. */
  code?: ErrorCode
  /** Ties this screen to a line in the Coordinator's log. */
  requestId?: string
  /** Whether trying the same thing again could plausibly work. */
  retryable: boolean
}

/**
 * What the heading says, per code.
 *
 * Written as the CONCLUSION rather than as the mechanism: somebody reading a
 * toast wants to know whether this is theirs to fix, and "Access denied" and
 * "The Coordinator failed" send them to two different places. The detail
 * underneath carries the specifics, which come from the server and are not
 * reworded here.
 */
const TITLES: Record<ErrorCode, string> = {
  PERMISSION_DENIED: 'Access denied',
  UNAUTHENTICATED: 'Sign-in required',
  NOT_FOUND: 'Not found',
  ALREADY_EXISTS: 'Already exists',
  INVALID_ARGUMENT: 'That request was not valid',
  FAILED_PRECONDITION: 'Not possible right now',
  ABORTED: 'Interrupted',
  RESOURCE_EXHAUSTED: 'Too many requests',
  UNAVAILABLE: 'Temporarily unavailable',
  // "The Coordinator failed" was the process's own name in front of somebody
  // who has never been told it has one. The fault is the same fault; what
  // changes is that the heading now names the thing they were using.
  INTERNAL: 'The service failed',
}

const KINDS: Record<ErrorCode, FailureKind> = {
  PERMISSION_DENIED: 'denied',
  UNAUTHENTICATED: 'unauthenticated',
  NOT_FOUND: 'rejected',
  ALREADY_EXISTS: 'rejected',
  INVALID_ARGUMENT: 'rejected',
  FAILED_PRECONDITION: 'rejected',
  ABORTED: 'rejected',
  RESOURCE_EXHAUSTED: 'unavailable',
  UNAVAILABLE: 'unavailable',
  INTERNAL: 'faulty',
}

/**
 * Reads any thrown value into the one shape every surface renders.
 *
 * Takes `unknown` because that is what a `catch` binds and what TanStack hands
 * a callback, and a helper that demanded a narrower type would push the
 * narrowing back out to the forty call sites this exists to remove.
 */
export function describeFailure(error: unknown): Failure {
  if (error instanceof ApiError) {
    const kind = KINDS[error.code] ?? 'faulty'
    return {
      kind,
      title: TITLES[error.code] ?? 'That did not work',
      // The server's sentence, verbatim. It is written for a person - see
      // middleware.Refusal - and paraphrasing it here would be a second place
      // where the wording of a refusal lives.
      detail: error.message,
      code: error.code,
      requestId: error.requestId,
      // A refusal is not retryable and neither is a bad request: the same
      // request produces the same answer, and a Try again that cannot work is
      // worse than none.
      retryable: kind === 'unavailable' || kind === 'faulty',
    }
  }
  if (error instanceof UnreachableError) {
    return {
      kind: 'unreachable',
      // Named as the CONDITION rather than as the diagnosis. "The Coordinator
      // could not be reached" is three words of internal vocabulary and one
      // instruction, aimed at whoever runs the deployment, shown to whoever
      // was using it. What the connection is doing about it is said by the
      // connection surfaces; what this has to say is which of the four kinds
      // of bad news this is.
      title: 'Connection lost',
      detail: error.message,
      retryable: true,
    }
  }
  return {
    kind: 'faulty',
    title: 'Something went wrong',
    detail: error instanceof Error ? error.message : String(error ?? 'No further detail.'),
    retryable: true,
  }
}

/** Whether a failure is a refusal, which several surfaces answer differently. */
export function isDenied(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'PERMISSION_DENIED'
}
