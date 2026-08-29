import { Typography } from 'antd'
import { c, InlineNotice, mono, StatusPill } from '../uikit'
import type { ConfigError, Product } from '../api/types'

/**
 * A product whose configuration was REFUSED.
 *
 * # Why this exists
 *
 * Because the previous answer was silence. A product whose document failed to
 * parse or validate was dropped from the API, so the screen simply did not
 * mention it: no row, no name, no reason. An operator who had just written
 * `products/acme.yaml` saw a product list without acme in it and no way to tell
 * a rejected document from one they had never saved. The only record was a line
 * in the Coordinator's log, which is the one place the person who edited the
 * file is least likely to be looking.
 *
 * Every other failure in this system is a STATE with a reason attached. This
 * one was an absence, and an absence cannot be diagnosed.
 *
 * # The distinction that must never be flattened
 *
 * A rejected document does NOT always mean a stopped product. Loading is
 * fail-closed per product: a bad edit to a product that was already working
 * leaves the previous good configuration running, so what failed is the EDIT,
 * not the product. `configError.loaded` carries that, and the two cases get
 * different words, a different tone and a different urgency:
 *
 *   loaded: false - "Not loaded". Nothing about this product runs.
 *   loaded: true  - "Edit not applied". It is running the previous version.
 *
 * Saying "Not loaded" over a product that is happily replicating would send
 * somebody to fix an outage that is not happening. Saying "Edit not applied"
 * over a product that does nothing would let a real one go unnoticed.
 */

/** Whether this product is configured but not running at all. */
export function isNotLoaded(p: Product): boolean {
  return Boolean(p.configError && !p.configError.loaded)
}

/** Whether this product is running, but not on what its document now says. */
export function isStale(p: Product): boolean {
  return Boolean(p.configError?.loaded)
}

/** The pill that goes beside the product's name. */
export function ConfigErrorPill({ error }: { error: ConfigError }) {
  return error.loaded ? (
    <StatusPill
      tone="pending"
      style={{ marginInlineEnd: 0 }}
      title="This product is running, but on its PREVIOUS configuration: the most recent edit to its document was rejected."
    >
      Edit not applied
    </StatusPill>
  ) : (
    <StatusPill
      tone="danger"
      style={{ marginInlineEnd: 0 }}
      title="This product's document was rejected, so nothing about it runs: nothing is discovered, nothing is downloaded."
    >
      Not loaded
    </StatusPill>
  )
}

/**
 * The reason, in full.
 *
 * Field first and in monospace, because it is a path into the reader's own
 * file and it is what they will search for once they open it. The hint is the
 * sentence that turns a rule into an action, so it is kept rather than
 * summarised away.
 *
 * `details` is present for a validation failure and absent for a parse error,
 * which has no structure to show - then the raw message is all there is, and
 * printing it verbatim is the honest thing to do.
 */
export function ConfigErrorDetail({ error }: { error: ConfigError }) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <InlineNotice tone={error.loaded ? 'warn' : 'danger'}>
        {error.loaded
          ? 'This product is running its previous configuration. The most recent edit was rejected, so the change has not taken effect.'
          : 'This product is configured but not running. Nothing is discovered and nothing is downloaded until its document is accepted.'}
        {error.file && (
          <>
            {' '}Edit <Typography.Text style={{ fontFamily: mono }}>{error.file}</Typography.Text> in
            the configuration repository; it is re-read as soon as it changes.
          </>
        )}
      </InlineNotice>

      {error.details && error.details.length > 0 ? (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          {error.details.map((d, i) => (
            <div
              key={`${d.field}-${i}`}
              style={{
                display: 'flex', flexDirection: 'column', gap: 2,
                paddingInlineStart: 10,
                borderInlineStart: `2px solid ${c.dangerBd}`,
              }}
            >
              {d.field && (
                <Typography.Text style={{ fontFamily: mono, fontSize: 12, color: c.text2 }}>
                  {d.field}
                </Typography.Text>
              )}
              <Typography.Text style={{ fontSize: 13 }}>{d.message}</Typography.Text>
              {d.hint && (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {d.hint}
                </Typography.Text>
              )}
            </div>
          ))}
        </div>
      ) : (
        <Typography.Text style={{ fontFamily: mono, fontSize: 12, whiteSpace: 'pre-wrap' }}>
          {error.message}
        </Typography.Text>
      )}
    </div>
  )
}

/**
 * The one-line version, for a table cell.
 *
 * The first detail rather than the joined message: a document with six
 * problems produces a line six clauses long, which in a row of a table is a
 * wall nobody reads. One problem named exactly, with the rest counted, is
 * enough to tell somebody what kind of thing is wrong - and the row expands to
 * show all of them.
 */
export function ConfigErrorLine({ error }: { error: ConfigError }) {
  const first = error.details?.[0]
  const rest = (error.details?.length ?? 0) - 1
  return (
    <Typography.Text
      style={{ fontSize: 12, color: error.loaded ? c.pending : c.danger }}
      title={error.message}
    >
      {first ? (
        <>
          {first.field && (
            <Typography.Text style={{ fontFamily: mono, fontSize: 11, color: 'inherit' }}>
              {first.field}{': '}
            </Typography.Text>
          )}
          {first.message}
          {rest > 0 && ` (+${rest} more)`}
        </>
      ) : (
        error.message
      )}
    </Typography.Text>
  )
}
