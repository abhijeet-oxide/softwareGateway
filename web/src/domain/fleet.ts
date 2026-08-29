import type { Transfer, Worker } from '../api/types'
import { isLive } from './derive'
import { formatCount } from './format'

/**
 * Whether anything can actually move right now, and if not, why not.
 *
 * # The question this file answers
 *
 * A download is planned by the Coordinator and PERFORMED BY WORKERS. If no
 * worker is running, a download that has been asked for is a perfectly healthy
 * row in a queue that nothing is draining - and every screen said it was going.
 * The shell's bar counted it among "downloads running", the listing's tag read
 * `Ready`, whose own tooltip promises a worker is about to take it, and the
 * progress cell said `estimating…` about an arrival with nothing behind it.
 *
 * Nothing there was false in the narrow sense. Together they told a reader that
 * their download was in progress, when the truth was that nothing was going to
 * happen until somebody started a worker - which is a completely different
 * afternoon, and the one fact none of those screens could see, because none of
 * them looked at the fleet.
 *
 * So the fleet and the transfer are read TOGETHER, in one place, and every
 * screen that reports on a download reports the same answer.
 */

/** The fleet, as one line of arithmetic. */
export interface Fleet {
  /** Workers that have heartbeated recently enough to be given work. */
  online: number
  /**
   * Workers that stopped heartbeating. Their jobs are being returned to the
   * queue; they are not capacity.
   */
  offline: number
  /** Draining workers finish what they hold and take nothing new. */
  draining: number
  /** Jobs the online workers could hold at once, and how many they do hold. */
  capacity: number
  busy: number
  /**
   * Whether the fleet is KNOWN. False while the listing is still loading or
   * has failed - and the difference matters, because "no workers" and "we have
   * not been told" produce the same zero and only one of them is worth
   * alarming somebody about.
   */
  known: boolean
}

export function summariseFleet(
  workers: Worker[] | undefined, known: boolean,
): Fleet {
  const fleet: Fleet = {
    online: 0, offline: 0, draining: 0, capacity: 0, busy: 0, known,
  }
  for (const w of workers ?? []) {
    switch (w.state) {
      case 'OFFLINE':
      case 'STALE':
        fleet.offline++
        break
      case 'DRAINING':
        fleet.draining++
        // Deliberately not capacity. A draining worker finishes what it holds
        // and accepts nothing new, so counting its ceiling would report room
        // that no job can ever be given.
        fleet.busy += w.activeJobs
        break
      default:
        fleet.online++
        fleet.capacity += w.maxConcurrency
        fleet.busy += w.activeJobs
    }
  }
  return fleet
}

/** Why a download that has been asked for is not moving. */
export type HoldKind =
  /** No worker is running at all, so nothing will move until one is. */
  | 'no-workers'
  /** Workers exist and every slot is taken by other work. */
  | 'no-capacity'
  /** The Coordinator is still working out what this download is made of. */
  | 'planning'
  /** Runnable, and no worker has picked it up yet. */
  | 'queued'

export interface Hold {
  kind: HoldKind
  /** Two or three words, for a tag or a cell. */
  label: string
  /** One sentence saying what is true and what would change it. */
  detail: string
  /**
   * Whether this needs somebody. A queue with workers draining it is working
   * as designed; a queue with no workers is not, and the difference must be
   * visible without reading.
   */
  actionable: boolean
}

/**
 * What is holding this download up, or null if nothing is.
 *
 * Null for anything settled, anything paused - somebody stopped it on purpose -
 * and anything with jobs actually in flight. A transfer with a worker moving
 * its bytes is not held up, whatever else is true of the fleet.
 */
export function holdOn(transfer: Transfer, fleet: Fleet): Hold | null {
  if (!isLive(transfer.state) || transfer.state === 'PAUSED'
    || transfer.state === 'CANCELLING') {
    return null
  }

  const progress = transfer.progress
  // IN FLIGHT is the test, not the state. A transfer reads `RUNNING` from the
  // moment its first job completes and goes on reading it while nothing at all
  // is happening - which is exactly the case somebody wants to know about.
  if ((progress?.jobsInFlight ?? 0) > 0) return null

  if (transfer.state === 'PLANNING'
    || (transfer.state === 'PENDING' && !progress?.jobsPlanned)) {
    return {
      kind: 'planning',
      label: 'Planning',
      detail: 'The Coordinator is reading this release to work out what has to move. '
        + 'Downloading starts when that finishes; no worker is needed for this part.',
      actionable: false,
    }
  }

  const waiting = progress?.jobsWaiting ?? progress?.jobsOutstanding ?? 0

  // NOT KNOWING is its own answer. The fleet listing may not have arrived, and
  // reporting "no workers are running" on the strength of a request that has
  // not come back yet would be alarming somebody about our own loading state.
  if (fleet.known && fleet.online === 0) {
    return {
      kind: 'no-workers',
      label: 'No worker',
      detail: fleet.offline > 0 || fleet.draining > 0
        ? `${describeFleet(fleet)} A download is planned by the Coordinator and performed `
          + 'by workers, so nothing will move until one is back.'
        : 'No worker has reported in. A download is planned by the Coordinator and '
          + 'performed by workers, so nothing will move until one is running.',
      actionable: true,
    }
  }

  if (fleet.known && fleet.capacity > 0 && fleet.busy >= fleet.capacity) {
    return {
      kind: 'no-capacity',
      label: 'Queued',
      detail: `Every slot on the ${countWord(fleet.online, 'worker')} is taken by other `
        + 'work. This download starts as soon as one frees up - higher priority moves it '
        + 'up the queue.',
      actionable: false,
    }
  }

  return {
    kind: 'queued',
    label: 'Queued',
    detail: waiting > 0
      ? `${formatCount(waiting)} of this download's jobs are waiting for a worker to take them.`
      : 'Planned and waiting for a worker to take the first job.',
    actionable: false,
  }
}

/** The fleet in a sentence, for the case where it is the news. */
export function describeFleet(fleet: Fleet): string {
  if (!fleet.known) return 'The worker fleet has not been read yet.'
  if (fleet.online === 0 && fleet.offline === 0 && fleet.draining === 0) {
    return 'No worker has ever reported in.'
  }
  if (fleet.online === 0) {
    const parts: string[] = []
    if (fleet.offline > 0) parts.push(`${countWord(fleet.offline, 'worker')} stopped heartbeating`)
    if (fleet.draining > 0) parts.push(`${countWord(fleet.draining, 'worker')} draining`)
    return `No worker is available: ${parts.join(', ')}.`
  }
  return `${countWord(fleet.online, 'worker')} online, `
    + `${formatCount(fleet.busy) ?? 0} of ${formatCount(fleet.capacity) ?? 0} slots in use.`
}

function countWord(n: number, noun: string): string {
  return `${formatCount(n) ?? n} ${noun}${n === 1 ? '' : 's'}`
}

/**
 * A transfer listing split by whether anything is actually happening to it.
 *
 * The shell's bar used to say "N downloads running" over a count that included
 * every planned, queued and unstarted one. On a fleet that is down that is the
 * single most misleading sentence in the interface: it reports the thing the
 * reader is worried about as working.
 */
export function splitByMotion(transfers: Transfer[], fleet: Fleet) {
  const moving: Transfer[] = []
  const held: Transfer[] = []
  for (const t of transfers) {
    if (!isLive(t.state)) continue
    if (holdOn(t, fleet)) held.push(t)
    else moving.push(t)
  }
  return { moving, held }
}
