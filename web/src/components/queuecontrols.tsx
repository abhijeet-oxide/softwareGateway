import { useState } from 'react'
import { App, Button, InputNumber, Popconfirm, Space, Tooltip, Typography } from 'antd'
import {
  DeleteOutlined, PauseOutlined, PlayCircleOutlined, ReloadOutlined, StopOutlined,
} from '@ant-design/icons'
import { useTransferControl, useTransferPriority } from '../api/queries'
import { useCan } from '../auth/permissions'
import type { Transfer } from '../api/types'

/**
 * The verbs an operator has over one download, in one place.
 *
 * # Why this is a component and not a column of buttons per page
 *
 * Which verbs a transfer admits is the SERVER'S state machine, and it is not
 * obvious: `delete` is refused while anything is unsettled, `resume` acts only
 * on a pause, and `retry` only on a failure. A page that laid its own buttons
 * out would eventually offer one the transfer cannot take, and the operator
 * would learn that from a red toast.
 *
 * So the mapping from state to verbs lives here, and every page that shows a
 * download shows the same set.
 */

/** Which states admit which verb. Mirrors internal/store/control.go. */
const LIVE = ['PENDING', 'PLANNING', 'READY', 'RUNNING', 'VERIFYING']
const SETTLED = ['SUCCEEDED', 'FAILED', 'CANCELLED']

export function QueueControls({
  transfer, size = 'small', onDeleted, hasFailures = false,
}: {
  transfer: Pick<Transfer, 'id' | 'state' | 'priority' | 'product'>
  size?: 'small' | 'middle'
  /** Called after a successful delete, since the transfer no longer exists. */
  onDeleted?: () => void
  /**
   * Whether any JOB of this transfer has failed, which is not the same as the
   * transfer having failed. A download can be running with three components
   * permanently failed under it - the retry that fixes those is the thing
   * somebody wants, and offering it only once the whole transfer gives up
   * means waiting for a failure that has, in effect, already happened.
   */
  hasFailures?: boolean
}) {
  const { message } = App.useApp()
  const mayOperate = useCan('operate', { product: transfer.product })
  const control = useTransferControl(transfer.id)

  const state = transfer.state
  const live = LIVE.includes(state)
  const settled = SETTLED.includes(state)

  const act = async (verb: 'retry' | 'pause' | 'resume' | 'stop' | 'delete') => {
    try {
      const res = await control.mutateAsync(verb)
      message.success(said(verb, res.jobs, res.inFlight ?? 0))
      if (verb === 'delete') onDeleted?.()
    } catch (e) {
      message.error(e instanceof Error ? e.message : `The download could not ${verb}.`)
    }
  }

  return (
    <Space size={4}>
      {(state === 'FAILED' || (hasFailures && !SETTLED.includes(state))) && (
        <Tooltip title="Resumes from where it stopped. Artifacts already transferred are not moved again.">
          <Button
            size={size}
            icon={<ReloadOutlined />}
            disabled={!mayOperate}
            loading={control.isPending}
            onClick={() => void act('retry')}
          >
            Retry
          </Button>
        </Tooltip>
      )}

      {live && (
        <Tooltip title="Nothing new starts. Work already in flight finishes - abandoning a large blob most of the way through would be a worse trade than waiting.">
          <Button size={size} icon={<PauseOutlined />} disabled={!mayOperate} onClick={() => void act('pause')}>
            Pause
          </Button>
        </Tooltip>
      )}

      {state === 'PAUSED' && (
        <Tooltip title="Picks up exactly where the pause left off.">
          <Button size={size} icon={<PlayCircleOutlined />} disabled={!mayOperate} onClick={() => void act('resume')}>
            Resume
          </Button>
        </Tooltip>
      )}

      {(live || state === 'PAUSED') && (
        <Popconfirm
          title="Stop this download?"
          description={
            <div style={{ maxWidth: 320 }}>
              Everything not yet started is cancelled. What already reached the
              destination stays there - it is untagged, so nobody can pull it by
              accident, and the next download of the same content will find it
              and skip it.
              <br />
              <Typography.Text type="secondary">
                Not reversible: Resume acts on a pause, not on a stop.
              </Typography.Text>
            </div>
          }
          okText="Stop it"
          okButtonProps={{ danger: true }}
          onConfirm={() => void act('stop')}
        >
          <Button size={size} danger icon={<StopOutlined />} disabled={!mayOperate}>
            Stop
          </Button>
        </Popconfirm>
      )}

      {settled && (
        <Popconfirm
          title="Remove this download's record?"
          description={
            <div style={{ maxWidth: 320 }}>
              Deletes the download and its jobs from this tool's database.
              Nothing at the destination is removed, and nothing could be: what a
              download put there is content-addressed and shared with every other
              release that uses the same layers.
            </div>
          }
          okText="Remove the record"
          okButtonProps={{ danger: true }}
          onConfirm={() => void act('delete')}
        >
          <Button size={size} danger icon={<DeleteOutlined />} disabled={!mayOperate}>
            Delete
          </Button>
        </Popconfirm>
      )}
    </Space>
  )
}

/**
 * What a verb did, in the operator's terms.
 *
 * The in-flight count is the load-bearing half: a stop that leaves jobs running
 * has NOT finished stopping, and somebody who is not told that watches a
 * `CANCELLING` download and wonders whether it is stuck.
 */
function said(verb: string, jobs: number, inFlight: number): string {
  const flight = inFlight > 0 ? ` ${inFlight} already in flight will finish.` : ''
  switch (verb) {
    case 'retry': return 'Retrying from where it stopped - work already done is not repeated.'
    case 'pause': return `Paused. ${jobs} jobs will not be handed out.${flight}`
    case 'resume': return `Resumed. ${jobs} jobs are leasable again.`
    case 'stop': return `Stopping. ${jobs} jobs cancelled.${
      inFlight > 0 ? ` ${inFlight} in flight; it reads CANCELLING until they report.` : ''}`
    case 'delete': return `Removed the record and ${jobs} jobs. Nothing at the destination was touched.`
    default: return 'Done.'
  }
}

/**
 * Where a download sits in the queue, and a way to move it.
 *
 * # Why this is worth an editable control rather than a number
 *
 * Priority is the ONLY answer to "my download is behind one I care less about".
 * Workers take the highest-priority job that is ready, so a download can be
 * moved to the front without stopping anything - and until this existed, the
 * only way to influence the order was to pause the thing in front, which stops
 * work rather than reordering it.
 *
 * The reorder applies to what has not started. Jobs a worker already holds run
 * to completion, which the tooltip says, because otherwise a raised priority
 * that changes nothing for the next few minutes reads as broken.
 */
export function PriorityControl({
  transfer,
}: {
  transfer: Pick<Transfer, 'id' | 'state' | 'priority' | 'product'>
}) {
  const { message } = App.useApp()
  const mayOperate = useCan('operate', { product: transfer.product })
  const setPriority = useTransferPriority(transfer.id)
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState<number | null>(transfer.priority)

  const settled = SETTLED.includes(transfer.state)

  const save = async () => {
    if (value === null) return
    try {
      const res = await setPriority.mutateAsync(value)
      message.success(
        res.jobs > 0
          ? `Priority ${value}. ${res.jobs} jobs reordered${
            (res.inFlight ?? 0) > 0 ? `; ${res.inFlight} already in flight finish where they are` : ''}.`
          : `Priority ${value}. Nothing was waiting to reorder.`,
      )
      setEditing(false)
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The priority could not be changed.')
    }
  }

  if (!editing) {
    // The NUMBER is the control. A "Change" button beside every value made a
    // one-field edit look like a form, and doubled the width of a column whose
    // content is at most four characters.
    return (
      <Tooltip
        title={
          settled || !mayOperate
            ? '0-1000, higher runs first. The default is 50, and downloads of equal priority run oldest first.'
            : 'Click to change. 0-1000, higher runs first; applies to work not yet started.'
        }
      >
        <Typography.Link
          disabled={settled || !mayOperate}
          onClick={() => {
            setValue(transfer.priority)
            setEditing(true)
          }}
          style={{ fontVariantNumeric: 'tabular-nums' }}
        >
          {transfer.priority}
        </Typography.Link>
      </Tooltip>
    )
  }

  return (
    <Space size={4}>
      <InputNumber
        size="small"
        min={0}
        max={1000}
        value={value}
        style={{ width: 88 }}
        autoFocus
        onChange={setValue}
        onPressEnter={() => void save()}
      />
      <Tooltip title="Applies to work not yet started. Jobs a worker is already running finish where they are.">
        <Button size="small" type="primary" loading={setPriority.isPending} onClick={() => void save()}>
          Set
        </Button>
      </Tooltip>
      <Button size="small" type="text" onClick={() => setEditing(false)}>Cancel</Button>
    </Space>
  )
}
