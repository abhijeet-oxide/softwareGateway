import type { QueryClient } from '@tanstack/react-query'
import { authorization } from '../auth/session'

/**
 * WHAT CHANGED, streamed, so a page is not asking every five seconds whether
 * anything did.
 *
 * # What this replaces, and what it does not
 *
 * The Downloads page polls its listing every five seconds while anything is
 * running, and that listing is the most expensive read in the application.
 * This connects once and is told which transfer moved; the listing is then
 * re-read because there is something to re-read, rather than on a timer.
 *
 * THE POLLS ARE DELIBERATELY LEFT IN PLACE. They are the floor this rests on:
 * a proxy that buffers, a network that drops the stream, a Coordinator too old
 * to serve the route - in every one of those the page keeps working exactly as
 * it did, a little less promptly. Nothing here is load-bearing for
 * correctness, which is also why a dropped event costs nothing.
 *
 * # `fetch`, not EventSource
 *
 * EventSource cannot set headers, and this deployment authenticates with a
 * bearer token (auth/session). The alternatives are a token in the query
 * string - which lands in the proxy's access log - or a second authentication
 * path for one route. `fetch` with a streaming body takes the same
 * Authorization header as every other request, so the stream goes through the
 * same middleware as everything else.
 *
 * A browser WebSocket cannot set headers either, which is one of the reasons
 * this is not one. See docs/design/32-performance.md §5.6.
 */

/** How long to wait before reconnecting, growing while the stream will not stay up. */
const RETRY_MIN_MS = 1_000
const RETRY_MAX_MS = 30_000

export interface TransferEvents {
  /** Stops the stream and prevents any further reconnection. */
  stop: () => void
}

/**
 * Follows the change feed, invalidating what each event makes stale.
 *
 * Returns immediately; the connection is managed in the background for as long
 * as `stop` is not called.
 */
export function followTransfers(qc: QueryClient): TransferEvents {
  const control = new AbortController()
  let stopped = false
  let backoff = RETRY_MIN_MS

  const invalidate = (transfer: string) => {
    /*
      THE EVENT IS A HINT, so this asks for the data again rather than
      applying anything from the payload.

      `refetchType: 'active'` is what keeps it cheap: a query key nothing is
      mounted on is marked stale and re-read when something mounts it, not
      now. Without it, an event would re-fetch every transfer listing this
      session has ever visited.
    */
    void qc.invalidateQueries({ queryKey: ['transfers'], refetchType: 'active' })
    void qc.invalidateQueries({ queryKey: ['transfer', transfer], refetchType: 'active' })
    void qc.invalidateQueries({ queryKey: ['transfer-activity'], refetchType: 'active' })
  }

  const read = async () => {
    const res = await fetch('/api/v1/events', {
      headers: { Accept: 'text/event-stream', ...(await authorization()) },
      signal: control.signal,
      // A stream is not a response to cache, and an intermediary that tried
      // would hold it until it ended - which it never does.
      cache: 'no-store',
    })
    if (!res.ok || !res.body) {
      throw new Error(`events: ${res.status}`)
    }
    // Connected, so the next failure starts its backoff from the bottom again
    // rather than from wherever the last outage reached.
    backoff = RETRY_MIN_MS

    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''

    for (;;) {
      const { done, value } = await reader.read()
      if (done) return
      buffer += decoder.decode(value, { stream: true })

      // Server-sent events are separated by a blank line. Anything after the
      // last one is a partial frame and stays in the buffer.
      const frames = buffer.split('\n\n')
      buffer = frames.pop() ?? ''

      for (const frame of frames) {
        // Comments - the opening `: connected` and the heartbeat - carry no
        // data and exist to keep the connection observable.
        const data = frame
          .split('\n')
          .filter((line) => line.startsWith('data:'))
          .map((line) => line.slice(5).trim())
          .join('')
        if (!data) continue
        try {
          const parsed = JSON.parse(data) as { transfer?: string }
          if (parsed.transfer) invalidate(parsed.transfer)
        } catch {
          // A frame we cannot read is a frame we ignore. The polls underneath
          // will notice whatever it was about.
        }
      }
    }
  }

  const loop = async () => {
    while (!stopped) {
      try {
        await read()
      } catch {
        // Every failure is the same failure here - refused, dropped, refused
        // again - and none of them is worth a message on screen, because the
        // page is still being served by its polls. See the header comment.
      }
      if (stopped) return
      await new Promise((resolve) => setTimeout(resolve, backoff))
      backoff = Math.min(backoff * 2, RETRY_MAX_MS)
    }
  }

  void loop()

  return {
    stop: () => {
      stopped = true
      control.abort()
    },
  }
}
