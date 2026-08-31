import { useEffect, useRef, useState } from 'react'

/**
 * How fast a download is going RIGHT NOW, as opposed to how fast it has gone.
 *
 * # The number this replaces, and why it read as kilobytes per second
 *
 * The only speed this interface had was `bytes moved / seconds since the
 * transfer started`. That is an average over the whole life of the download,
 * and it is dragged down by every second in which nothing moved:
 *
 *   - planning, which contacts no worker and moves no bytes at all;
 *   - the wait for a worker to take the first job;
 *   - a stall while a registry was returning 503s;
 *   - the dedupe pass, where the fastest possible outcome - the destination
 *     already holds the content - moves ZERO bytes and still costs time.
 *
 * A download that spent twenty minutes queued and has since been moving at
 * 90 MB/s for thirty seconds reports about 2 MB/s, and reports it as "Speed".
 * The number is arithmetically correct and it answers a question nobody asked.
 * Worse, it is the number somebody uses to decide whether the link is broken.
 *
 * # What this measures instead
 *
 * The difference between two observations of the same counter, divided by the
 * wall-clock time between them, over a short trailing window. That is the
 * genuine current rate: it starts at nothing, rises when bytes start moving,
 * and falls to zero when they stop - which is exactly the behaviour that makes
 * "is this thing moving?" answerable at a glance.
 *
 * # Why it is a window and not the last two samples
 *
 * The polling interval is a couple of seconds and a worker reports a blob's
 * progress in bursts, so two adjacent samples routinely differ by a factor of
 * three in either direction. A number that swings like that is unreadable and,
 * being unreadable, gets ignored. The window is long enough to be steady and
 * short enough that a stall shows up within a few seconds of starting.
 *
 * # Both numbers are kept, because they answer different questions
 *
 * The average still matters - it is what "how long will the next one take"
 * rests on, and it is the honest summary of a finished download. It is just
 * not the answer to "what is it doing now", and labelling one as the other is
 * what made the speed look broken.
 */

/** How far back the window reaches. */
const WINDOW_MS = 20_000
/** Samples older than this are dropped even if the window is not full. */
const MAX_SAMPLES = 40

export interface Rate {
  /**
   * The current rate in bytes per second, or undefined while there is not
   * enough of a window to divide by.
   *
   * Undefined rather than zero on purpose: "we have not measured it yet" and
   * "it is not moving" are different facts, and only the second one is news.
   */
  current: number | undefined
  /** Whether the window is long enough for `current` to mean anything. */
  settled: boolean
  /** Bytes moved across the window, for the tooltip that shows the working. */
  windowBytes: number
  windowSeconds: number
}

interface Sample {
  at: number
  bytes: number
}

/**
 * Samples a monotonically increasing byte counter and reports its slope.
 *
 * `bytes` is expected to come from a polled query, so this hook records a
 * sample whenever the value it is handed CHANGES - not on a timer of its own.
 * A second timer would sample the same value repeatedly between polls and
 * report a rate that decayed towards zero between every refresh.
 */
export function useByteRate(bytes: number | undefined, live: boolean): Rate {
  const samples = useRef<Sample[]>([])
  // Re-renders when a sample lands, so the number on screen follows the poll.
  const [, tick] = useState(0)

  useEffect(() => {
    if (!live || bytes === undefined) {
      // A settled download has no current rate, and holding the last one would
      // leave a finished transfer claiming to be moving at 90 MB/s.
      samples.current = []
      return
    }

    const now = Date.now()
    const last = samples.current.at(-1)

    /*
      A COUNTER THAT WENT BACKWARDS is a different transfer, or a retry that
      reset what it had moved. Keeping the old samples across that would
      produce a large negative delta and then a nonsense rate; starting again
      costs one window's worth of "measuring" and is always right.
    */
    if (last && bytes < last.bytes) {
      samples.current = [{ at: now, bytes }]
      tick((n) => n + 1)
      return
    }

    // Nothing new to record. Deliberately still a no-op rather than a sample
    // with the same value: an unchanged counter between two polls is genuinely
    // "no bytes moved", and the window below already reads that as zero
    // through the elapsed time.
    if (last && last.bytes === bytes && now - last.at < 1000) return

    samples.current = [...samples.current, { at: now, bytes }]
      .filter((s) => now - s.at <= WINDOW_MS)
      .slice(-MAX_SAMPLES)
    tick((n) => n + 1)
  }, [bytes, live])

  const window = samples.current
  const first = window[0]
  const last = window.at(-1)

  if (!live || !first || !last || first === last) {
    return { current: undefined, settled: false, windowBytes: 0, windowSeconds: 0 }
  }

  const seconds = (last.at - first.at) / 1000
  const moved = last.bytes - first.bytes

  // A window under a couple of seconds divides by almost nothing, and the
  // number that comes out is noise wearing a unit.
  if (seconds < 2) {
    return { current: undefined, settled: false, windowBytes: moved, windowSeconds: seconds }
  }

  return {
    current: Math.max(0, moved / seconds),
    settled: seconds >= 6,
    windowBytes: moved,
    windowSeconds: seconds,
  }
}
