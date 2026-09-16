import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { followTransfers } from './events'

/**
 * Keeps the change feed connected for as long as the application is mounted.
 *
 * Renders nothing. It lives here rather than on the Downloads page because the
 * shell's own status line is fed by the same events, and a stream that
 * connected and disconnected as somebody navigated between two pages that both
 * want it would be one reconnection per navigation.
 *
 * Mounted INSIDE the identity provider, so the first connection carries a
 * settled session rather than racing the code exchange - the same ordering
 * argument as every read in this tree. See api/events.
 */
export function TransferFeed() {
  const qc = useQueryClient()

  useEffect(() => {
    const feed = followTransfers(qc)
    return feed.stop
  }, [qc])

  return null
}
