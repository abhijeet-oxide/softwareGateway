import { Alert, Button, Space, Tag, Tooltip, Typography } from 'antd'

import type { SecurityRegistration } from '../api/types'
import { formatRelative } from '../domain/format'
import { c } from '../uikit'

/**
 * Replicating a release to a scanner that has to be TOLD about it.
 *
 * # Why this notice exists at all
 *
 * JFrog Xray indexes a repository: an image that lands there is scanned because
 * it landed there. Anchore does not - it is told an image exists, pulls it, and
 * analyses it on its own schedule. So a release nobody has replicated to Anchore
 * has no Anchore findings, and the honest reason is not "the scanner found
 * nothing" but "the scanner has never been asked".
 *
 * Those two render identically as an empty table, which is the single failure
 * this whole feature exists to prevent. This notice is that distinction, on
 * screen, with the button that resolves it.
 *
 * # Why it is a banner and not a row in a settings panel
 *
 * Because it is a thing to DO, once, and then never think about again. A reader
 * who opens the Security tab of a freshly transferred release should not have to
 * discover that a second scanner exists and was never told; they should be
 * shown one sentence and one button. Once replicated it collapses to a line.
 */

/** Whether anything here is worth drawing. */
export function hasReplication(registrations?: SecurityRegistration[]): boolean {
  return (registrations?.length ?? 0) > 0
}

export function ReplicationNotice({ registrations, onReplicate, pending }: {
  registrations?: SecurityRegistration[]
  onReplicate: (provider: string) => void
  /** The provider currently being replicated, so only its button spins. */
  pending?: string
}) {
  const rows = registrations ?? []
  if (rows.length === 0) return null

  // Anything not finished comes first and gets the loud treatment. A release
  // where everything is registered gets one quiet line, because at that point
  // the fact is only worth confirming.
  const outstanding = rows.filter((r) => r.state !== 'registered')
  if (outstanding.length === 0) {
    return <ReplicatedLine registrations={rows} onReplicate={onReplicate} pending={pending} />
  }

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {outstanding.map((r) => (
        <OutstandingNotice key={r.provider} registration={r} onReplicate={onReplicate} pending={pending} />
      ))}
    </Space>
  )
}

/**
 * The banner for a scanner that has not been told about this release, or has
 * been told about only part of it.
 *
 * Four states, four sentences, and the difference between them is what the
 * reader does next - which is the only reason to distinguish them at all.
 */
function OutstandingNotice({ registration: r, onReplicate, pending }: {
  registration: SecurityRegistration
  onReplicate: (provider: string) => void
  pending?: string
}) {
  const busy = pending === r.provider
  const running = r.state === 'registering' && !r.stalled

  const { type, message, description } = describe(r)

  return (
    <Alert
      type={type}
      showIcon
      message={message}
      description={
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Typography.Text type="secondary" style={{ fontSize: 12.5 }}>
            {description}
          </Typography.Text>
          {r.error && (
            /*
              The scanner's own words, kept. A notice that said "it failed" and
              swallowed the reason sends the reader to a log they have to know
              exists; the sentence that came back from Anchore names the fix
              nine times out of ten.
            */
            <Typography.Text type="secondary" style={{ fontSize: 12, fontStyle: 'italic' }}>
              {r.error}
            </Typography.Text>
          )}
          <Space size={8} wrap>
            <Button
              type="primary"
              size="small"
              loading={busy}
              disabled={running || !r.canReplicate}
              onClick={() => onReplicate(r.provider)}
            >
              {r.state === '' ? `Replicate to ${r.label}` : `Replicate to ${r.label} again`}
            </Button>
            {!r.canReplicate && r.reason && (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>{r.reason}</Typography.Text>
            )}
            <Counts registration={r} />
          </Space>
        </Space>
      }
    />
  )
}

/** The four situations, and the sentence each of them needs. */
function describe(r: SecurityRegistration): {
  type: 'info' | 'warning' | 'error'
  message: string
  description: string
} {
  if (r.state === 'registering' && !r.stalled) {
    return {
      type: 'info',
      message: `Replicating this release to ${r.label}`,
      description: 'Its images are being submitted for analysis. This takes seconds; the analysis '
        + 'itself then runs on the scanner\'s own schedule.',
    }
  }
  if (r.stalled) {
    return {
      type: 'warning',
      message: `Replication to ${r.label} was interrupted`,
      description: 'The Coordinator running it stopped. Nothing is running now - run it again. '
        + 'Images already submitted are not submitted twice.',
    }
  }
  if (r.state === 'failed') {
    return {
      type: 'error',
      message: `This release could not be replicated to ${r.label}`,
      description: `${r.label} has no record of these images, so it has nothing to analyse and `
        + 'this release has no findings from it. That is not a clean result - it is an unasked '
        + 'question.',
    }
  }
  if (r.state === 'partial') {
    return {
      type: 'warning',
      message: `${r.outstanding.toLocaleString()} of this release's `
        + `${r.expected.toLocaleString()} images are not in ${r.label}`,
      description: `${r.label} holds ${r.associated.toLocaleString()} of them and has nothing to `
        + 'say about the rest. This is the ordinary state of a release that is still being '
        + 'transferred: replicate it again once the remaining images have landed.',
    }
  }
  // Never replicated, which is the state this notice exists for.
  return {
    type: 'warning',
    message: `This release has not been replicated to ${r.label}`,
    description: `${r.label} is configured for this release and has never been told these images `
      + 'exist, so it has nothing to analyse and no findings to report. An empty result here is '
      + 'an unasked question rather than a clean release. Replicating submits the images and '
      + 'creates the application version in ' + r.label + '; analysis then runs on its own '
      + 'schedule and a sync collects the results.',
  }
}

/**
 * The numbers, where there are any worth showing.
 *
 * Absent on a release that has never been replicated - zeroes beside a button
 * that has never been pressed are three facts that all say "nothing yet", which
 * the sentence above already said better.
 */
function Counts({ registration: r }: { registration: SecurityRegistration }) {
  if (r.expected === 0 && r.associated === 0) return null
  return (
    <Space size={6} wrap>
      <Tooltip title={`${r.label} holds ${r.associated.toLocaleString()} of this release's `
        + `${r.expected.toLocaleString()} images.`}
      >
        <Tag style={{ marginInlineEnd: 0, fontSize: 11 }}>
          {r.associated.toLocaleString()}/{r.expected.toLocaleString()} images
        </Tag>
      </Tooltip>
      {r.analysed > 0 && (
        <Tooltip title="Analysis runs on the scanner's own schedule. Sync this release to collect
          the results of the ones that have finished.">
          <Tag style={{ marginInlineEnd: 0, fontSize: 11 }}>
            {r.analysed.toLocaleString()} analysed
          </Tag>
        </Tooltip>
      )}
    </Space>
  )
}

/**
 * The quiet line for a release that IS replicated.
 *
 * # Why it is still on screen at all
 *
 * Because the reader needs to be able to tell "Anchore has these images and has
 * not finished analysing them" from "Anchore was never told about them", and
 * both of those show an empty or partial findings table. One line says which.
 *
 * It also carries the link into the scanner, which is the other thing somebody
 * wants from this area once the button has done its job - and the button
 * itself, because a release whose images changed needs replicating again.
 */
function ReplicatedLine({ registrations, onReplicate, pending }: {
  registrations: SecurityRegistration[]
  onReplicate: (provider: string) => void
  pending?: string
}) {
  return (
    <Space size={12} wrap style={{ fontSize: 12.5 }}>
      {registrations.map((r) => (
        <Space key={r.provider} size={6} wrap>
          <Typography.Text type="secondary" style={{ fontSize: 12.5, color: c.text2 }}>
            Replicated to {r.label}
            {r.registeredAt && ` ${formatRelative(r.registeredAt)}`}
            {': '}
            {r.associated.toLocaleString()} images
            {/*
              The analysed count, which is the answer to the question this line
              is most often read to answer: the sync found nothing and the
              reader wants to know whether that is Anchore's fault or simply
              not finished yet.
            */}
            {r.analysed < r.associated && `, ${r.analysed.toLocaleString()} analysed so far`}
          </Typography.Text>
          {r.url && (
            <Typography.Link href={r.url} target="_blank" rel="noreferrer" style={{ fontSize: 12 }}>
              Open in {r.label}
            </Typography.Link>
          )}
          <Tooltip title={`Submits any images ${r.label} does not already hold and re-checks the `
            + 'application version. Images it already has are not submitted again.'}
          >
            <Button
              size="small"
              type="text"
              loading={pending === r.provider}
              disabled={!r.canReplicate}
              onClick={() => onReplicate(r.provider)}
              style={{ fontSize: 12, height: 22, paddingInline: 6 }}
            >
              Replicate again
            </Button>
          </Tooltip>
        </Space>
      ))}
    </Space>
  )
}
