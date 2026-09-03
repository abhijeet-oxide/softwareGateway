import { Alert, Button, Space, Tooltip, Typography } from 'antd'

import type { SecurityRegistration } from '../api/types'
import { formatRelative } from '../domain/format'
import { ReloadOutlined } from '../icons'
import { ScannerMark } from './icons'
import { ReplicationLogButton } from './replicationprogress'
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

/**
 * The header control, beside Sync.
 *
 * # Why it is up here rather than in the banner
 *
 * Because the two acts are one workflow read left to right - replicate the
 * release to the scanner, then collect what it found - and this half of it
 * lived only in a banner among the summary cards. On a release nobody had
 * replicated that put the most consequential control on the tab below the fold,
 * while the one control the header did offer was a sync that could only ever
 * come back empty.
 *
 * ONE LABEL, in one colour, whatever the state. "Replicate to Anchore again"
 * and a red variant for a failed run made three buttons out of one action, and
 * the reader had to work out which of the three they were looking at before
 * pressing the only one there has ever been. The banner underneath says what
 * state the release is in; this says what the button does.
 *
 * Absent once every scanner holds the release, because at that point it is a
 * control whose honest label is "do nothing again".
 */
export function ReplicateButton({ registrations, onReplicate, pending, size = 'middle' }: {
  registrations?: SecurityRegistration[]
  onReplicate: (provider: string) => void
  pending?: string
  size?: 'small' | 'middle'
}) {
  const rows = registrations ?? []
  // A run in flight has its own panel with its own Stop. A second control up
  // here would be a third thing describing one operation.
  const outstanding = rows.filter(
    (r) => r.state !== 'registered' && !(r.state === 'registering' && !r.stalled),
  )
  const r = outstanding[0]
  if (!r) return null

  return (
    <Tooltip
      title={!r.canReplicate ? r.reason : `Submits this release's images to ${r.label} and `
        + `creates its application version. ${r.label} pulls and analyses on its own schedule; `
        + 'sync this release afterwards to collect the results.'}
    >
      <Button
        size={size}
        type="primary"
        icon={<ScannerMark provider={r.provider} />}
        loading={pending === r.provider}
        disabled={!r.canReplicate}
        onClick={() => onReplicate(r.provider)}
      >
        Replicate to {r.label}
      </Button>
    </Tooltip>
  )
}

/**
 * The last replication's transcript, for the header row.
 *
 * Beside the sync's log rather than inside the alert, because the two are the
 * same kind of thing - the record of a run that has finished - and a reader
 * looking for one should not find the other somewhere else entirely.
 */
export function ReplicationLogControl({ registrations, size = 'middle' }: {
  registrations?: SecurityRegistration[]
  size?: 'small' | 'middle'
}) {
  const r = (registrations ?? []).find((x) => (x.log?.length ?? 0) > 0)
  if (!r) return null
  return <ReplicationLogButton registration={r} size={size} />
}

export function ReplicationNotice({ registrations, onReplicate, pending }: {
  registrations?: SecurityRegistration[]
  onReplicate: (provider: string) => void
  /** The provider currently being replicated, so only its button spins. */
  pending?: string
}) {
  const rows = registrations ?? []
  if (rows.length === 0) return null

  // A RUNNING replication draws its own panel, and this steps aside for it.
  // Two things describing one operation - a live bar and a banner with a
  // disabled button under it - is one of them too many, and the disabled one
  // is the one that says nothing.
  const idle = rows.filter((r) => !(r.state === 'registering' && !r.stalled))
  if (idle.length === 0) return null

  // Anything not finished comes first and gets the loud treatment. A release
  // where everything is registered gets one quiet line, because at that point
  // the fact is only worth confirming.
  const outstanding = idle.filter((r) => r.state !== 'registered')
  if (outstanding.length === 0) {
    return <ReplicatedLine registrations={idle} onReplicate={onReplicate} pending={pending} />
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
      // The RETRY in the action slot, not stacked under the text. As a row of
      // its own it needed a full-width flex container and left a band of empty
      // alert beneath the sentence it belonged to.
      action={
        <Button
          size="small"
          icon={<ReloadOutlined />}
          loading={busy}
          disabled={running || !r.canReplicate}
          onClick={() => onReplicate(r.provider)}
        >
          Retry
        </Button>
      }
      description={
        <Space direction="vertical" size={6} style={{ width: '100%' }}>
          {/*
            The scanner's own words, and on a failure they are the WHOLE of it.
            A paragraph explaining what an absence of findings means is worth
            saying to somebody who has not replicated yet; above a scanner's
            error message it is padding between the reader and the sentence
            that names the fix.
          */}
          {description && (
            <Typography.Text type="secondary" style={{ fontSize: 12.5 }}>
              {description}
            </Typography.Text>
          )}
          {r.error && (
            <Typography.Text style={{ fontSize: 12.5 }}>
              {r.error}
            </Typography.Text>
          )}
          {/* The remedy under the evidence, and quieter than it. */}
          {r.remedy && (
            <Typography.Text type="secondary" style={{ fontSize: 12.5 }}>
              {r.remedy}
            </Typography.Text>
          )}
          {!r.canReplicate && r.reason && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>{r.reason}</Typography.Text>
          )}
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
      description: `Its images are being submitted to ${r.label}. Analysis runs on `
        + `${r.label}'s own schedule once they are accepted.`,
    }
  }
  if (r.stalled) {
    return {
      type: 'warning',
      message: `Replication to ${r.label} was interrupted`,
      description: 'The Coordinator running it stopped, so no replication is in progress. '
        + 'Run it again: images already accepted are not submitted twice.',
    }
  }
  if (r.state === 'failed') {
    // NO description. The error underneath is the whole message, and a
    // paragraph about what an absence of findings means sits between the
    // reader and the sentence that names the fix.
    return {
      type: 'error',
      message: `Replication to ${r.label} failed`,
      description: '',
    }
  }
  if (r.state === 'partial') {
    return {
      type: 'warning',
      message: `${r.outstanding.toLocaleString()} of this release's `
        + `${r.expected.toLocaleString()} images are not in ${r.label}`,
      description: `${r.label} holds ${r.associated.toLocaleString()} of them and reports no `
        + 'findings for the remainder. This is the expected state of a release that is still '
        + 'being transferred: replicate it again once the remaining images have landed.',
    }
  }
  // Never replicated, which is the state this notice exists for.
  return {
    type: 'warning',
    message: `This release has not been replicated to ${r.label}`,
    description: `${r.label} is configured for this release but has not been sent its images, `
      + 'so it has reported no findings. The absence of findings records that no analysis was '
      + `requested, not that the release is clean. Replicating submits the images to ${r.label} `
      + 'and creates the application version; analysis runs on its own schedule, and a sync '
      + 'collects the results.',
  }
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
            {r.analysed < r.associated && `, ${r.analysed.toLocaleString()} analysed`}
          </Typography.Text>
          {r.url && (
            <Typography.Link href={r.url} target="_blank" rel="noreferrer" style={{ fontSize: 12 }}>
              Open in {r.label}
            </Typography.Link>
          )}
          <Tooltip title={`Resubmits this release's images to ${r.label} and re-checks the `
            + 'application version. Submission by digest is idempotent, so images it already '
            + 'holds are not analysed again.'}
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
