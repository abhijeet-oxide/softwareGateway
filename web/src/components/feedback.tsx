import { useEffect } from 'react'
import { App, Typography } from 'antd'
import { describeFailure, type Failure } from '../api/errors'
import { c, mono } from '../uikit'

/**
 * WHERE A FAILURE IS SHOWN, decided once.
 *
 * # The bug this file is the fix for
 *
 * Press Run health check without the permission for it and nothing happened.
 * No spinner, no error, no toast - the button did not even look pressed. The
 * request went, the Coordinator answered 403 with a sentence explaining
 * exactly what was missing, TanStack stored the error on the query, and no
 * code anywhere read it. The reader concluded the button was broken, which is
 * the worst outcome available: the system worked perfectly and said nothing.
 *
 * That was not one missing `catch`. It was the absence of a place to put one.
 * Every page had to remember to render every failure it could produce, and the
 * ones nobody remembered were silent.
 *
 * # The rule
 *
 * A failure is reported centrally when the reader ASKED for something. The
 * query client's caches call `reportFailure` for:
 *
 *   - every mutation - a mutation is somebody pressing a button;
 *   - every query marked `meta.action` - a read somebody explicitly asked for,
 *     which is a button too even though TanStack calls it a refetch;
 *   - every query whose data was already on screen - a background refresh that
 *     failed, which is otherwise invisible and leaves stale numbers looking
 *     current.
 *
 * A first load that fails is NOT reported here: the page renders `ErrorState`
 * in its own body, which has room for the detail and a Try again, and a toast
 * on top of it would say the same thing twice.
 *
 * Sign-in is the one exclusion. A 401 has already sent the browser to the
 * identity provider (see api/client), and a toast about it would land on a page
 * that is being replaced.
 */

/**
 * The bound notifier.
 *
 * A module-level holder rather than a context, because the callers are the
 * query client's caches - which are constructed outside React and have no
 * component to read a context from. `FeedbackBridge` fills it in on mount,
 * so the notifications are Ant Design's themed ones rather than the static
 * fallback, which renders outside the theme and warns about it.
 */
let notify: ((failure: Failure) => void) | undefined

/** Recent reports, so one broken thing is said once. */
const recent = new Map<string, number>()
const DEDUPE_MS = 4000

/**
 * Reports a failure to the reader.
 *
 * Deduplicated, because a page that fans out a read per product produces one
 * failure per product for one cause, and twelve identical toasts stack past
 * the top of the window and hide the thing they are about.
 */
export function reportFailure(error: unknown, action?: string): void {
  const failure = describeFailure(error)
  // Already being handled by a redirect to the identity provider.
  if (failure.kind === 'unauthenticated') return

  const key = `${failure.code ?? failure.kind}|${failure.detail}|${action ?? ''}`
  const now = Date.now()
  const last = recent.get(key)
  if (last !== undefined && now - last < DEDUPE_MS) return
  recent.set(key, now)
  // Bounded: the map is keyed by message, and a long session against a flapping
  // Coordinator would otherwise keep every distinct one forever.
  for (const [k, at] of recent) {
    if (now - at > DEDUPE_MS) recent.delete(k)
  }

  notify?.(action ? { ...failure, title: `${action} failed` } : failure)
}

/**
 * Binds the notifier to Ant Design's themed notification API.
 *
 * Rendered once, inside `<App>`, above the router. It draws nothing.
 */
export function FeedbackBridge() {
  const { notification } = App.useApp()

  useEffect(() => {
    notify = (failure) => {
      notification.open({
        type: failure.kind === 'denied' ? 'warning' : 'error',
        message: failure.title,
        description: <FailureDetail failure={failure} />,
        // Long enough to read a sentence and a request id, and dismissible.
        // A refusal is worth longer than a blip: the reader's next move is to
        // ask somebody for a role, and they need the sentence to quote.
        duration: failure.kind === 'denied' ? 10 : 6,
        placement: 'bottomRight',
      })
    }
    return () => {
      notify = undefined
    }
  }, [notification])

  return null
}

/**
 * What the toast says under its heading: the Coordinator's own sentence, and
 * the request id when there is one.
 *
 * The id is the thing that makes a support conversation one round trip instead
 * of three, and it is set in the monospace face because it is a value to be
 * copied rather than prose to be read.
 */
function FailureDetail({ failure }: { failure: Failure }) {
  return (
    <div style={{ minWidth: 0 }}>
      <div style={{ fontSize: 13, lineHeight: 1.5 }}>{failure.detail}</div>
      {failure.requestId && (
        <Typography.Text
          copyable={{ text: failure.requestId, tooltips: ['Copy request id', 'Copied'] }}
          style={{
            display: 'inline-block',
            marginTop: 6,
            fontFamily: mono,
            fontSize: 11,
            color: c.text3,
            wordBreak: 'break-all',
          }}
        >
          {failure.requestId}
        </Typography.Text>
      )}
    </div>
  )
}
