import type { ReactNode } from 'react'
import { Tooltip, Typography } from 'antd'
import { useAvailability, useVersion, useWorkers } from '../api/queries'
import { formatCount } from '../domain/format'
import { TimeAgo } from './chips'
import { PanelFoot } from './panel'
import { c, CardHead, SectionCard, StatusPill } from '../uikit'
import { HddOutlined } from '../icons'
import type { PillTone } from '../uikit'
import { useIdentity } from '../auth/permissions'

/**
 * THE PROCESSES THIS DEPLOYMENT IS MADE OF, in one list.
 *
 * # Why the Coordinator is in it, and first
 *
 * Because it is a node like any other and the most important one. The fleet
 * panel used to list workers alone, which drew a system with no middle: a
 * reader looking at "1 worker running" had no way to tell whether the thing
 * handing that worker its jobs was up, and the availability of the Coordinator
 * was a separate card several hundred pixels away saying so in different words.
 *
 * They answer one question - IS THIS DEPLOYMENT WORKING - so they are one
 * list, in dependency order: the Coordinator plans the work, the workers move
 * the bytes. A fleet of zero beneath a healthy Coordinator is then a picture
 * rather than a sentence somebody has to assemble.
 *
 * # What each number means
 *
 * `activeJobs` is what the WORKER says it is running; `maxConcurrency` is what
 * it will take. The two disagreeing with the Coordinator's own count is worth
 * seeing rather than averaging away: a worker holding jobs the Coordinator has
 * already reaped is about to report completions nobody will accept.
 *
 * STALE is derived from the heartbeat rather than announced - a worker that was
 * killed never got to say so. The same is true of the Coordinator's row, which
 * comes from the availability record: a gap in it is a process that was not
 * there to write one.
 */
export function SystemPanel() {
  const { can } = useIdentity()
  const workers = useWorkers()
  const version = useVersion()
  // The Coordinator's own row. A caller who may not read it gets the fleet
  // without it rather than a card that refuses as a whole.
  const availability = useAvailability('24h')

  const fleet = workers.data?.workers ?? []
  const active = fleet.filter((w) => w.state === 'ACTIVE')
  const offline = fleet.filter((w) => w.state === 'OFFLINE' || w.state === 'STALE')

  const capacity = active.reduce((n, w) => n + (w.maxConcurrency ?? 0), 0)
  const running = active.reduce((n, w) => n + (w.activeJobs ?? 0), 0)

  const coordinator = availability.data
  const mayReadFleet = can('worker.view')

  return (
    <SectionCard
      className="ui-card-lead"
      style={{ height: '100%' }}
      title={
        <CardHead
          icon={<HddOutlined />}
          tone="review"
          title="System"
          status={offline.length > 0 && <StatusPill tone="danger" size="sm">{offline.length} offline</StatusPill>}
        />
      }
      extra={
        mayReadFleet && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {formatCount(running)} of {formatCount(capacity)} slots in use
          </Typography.Text>
        )
      }
    >
      {/*
        WHAT THE FLEET IS DOING, above the rows rather than under them. It is
        the sentence the rows are evidence for, and a reader who only wants the
        headline should not have to scan a list to assemble it.
      */}
      {mayReadFleet && fleet.length > 0 && (
        <div style={{ display: 'flex', gap: 10, marginBottom: 10, fontSize: 12.5 }}>
          <span>
            <b>{formatCount(active.length)}</b> {active.length === 1 ? 'worker' : 'workers'} running
          </span>
          <span style={{ color: c.border }}>|</span>
          <span style={{ color: c.text2 }}>
            {running === 0 ? 'No jobs running' : `${formatCount(running)} jobs running`}
          </span>
        </div>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        {/*
          THE COORDINATOR, FIRST. It plans every download; a fleet running
          beneath a Coordinator that is not serving is a fleet with nothing to
          do, and that is the order the two have to be read in.
        */}
        {coordinator?.recordedFrom ? (
          <Node
            name="Coordinator"
            tone={
              coordinator.status === 'HEALTHY' ? 'ok'
                : coordinator.status === 'DEGRADED' ? 'pending'
                : coordinator.status === 'DOWN' ? 'danger' : 'neutral'
            }
            state={
              coordinator.status === 'HEALTHY' ? 'SERVING'
                : coordinator.status === 'DEGRADED' ? 'DEGRADED'
                : coordinator.status === 'DOWN' ? 'NOT SERVING' : 'NOT RECORDED'
            }
            hint={
              coordinator.status === 'HEALTHY'
                ? 'Serving requests, with every dependency it needs.'
                : coordinator.status === 'DEGRADED'
                  ? 'Serving requests with something wrong behind it. The Settings page names which dependency.'
                  : 'Nothing has been recorded recently enough to count, so this Coordinator is not serving.'
            }
            detail={
              <>
                {version.data?.version ? `v${version.data.version}` : 'version not reported'} ·{' '}
                {coordinator.status === 'DOWN' ? 'stopped ' : 'serving since '}
                <TimeAgo at={coordinator.statusSince} />
              </>
            }
          />
        ) : null}

        {!mayReadFleet ? null : fleet.length === 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: 12.5, lineHeight: 1.5 }}>
            No worker has reported in. The Coordinator plans downloads and workers move the bytes, so
            nothing will transfer until at least one is running.
          </Typography.Text>
        ) : (
          fleet.map((w) => (
            <Node
              key={w.workerId}
              name={w.workerId}
              mono
              tone={w.state === 'ACTIVE' ? 'ok' : w.state === 'STALE' ? 'danger' : 'pending'}
              state={w.state}
              hint={
                w.state === 'STALE'
                  ? 'This worker stopped sending heartbeats. Its jobs return to the queue automatically.'
                  : w.state === 'DRAINING'
                    ? 'Finishing what it holds and taking no new work.'
                    : 'Leasing and running jobs normally.'
              }
              count={`${formatCount(w.activeJobs)}/${formatCount(w.maxConcurrency)}`}
              detail={
                <>
                  {w.version ? `v${w.version}` : 'version not reported'} · last heard{' '}
                  <TimeAgo at={w.lastHeartbeat} />
                </>
              }
            />
          ))
        )}
      </div>

      <PanelFoot to="/settings" action="View all workers" />
    </SectionCard>
  )
}

/**
 * One process in the list.
 *
 * The state is a pill AND a word, never a coloured dot alone: the dot is what
 * makes the row scannable and the word is what makes it readable to somebody
 * who cannot tell the dot's colour from the one above it.
 */
function Node({
  name,
  detail,
  state,
  tone,
  hint,
  count,
  mono,
}: {
  name: string
  detail: ReactNode
  state: string
  tone: PillTone
  hint: string
  count?: string
  mono?: boolean
}) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        border: `1px solid ${c.border}`,
        borderRadius: 8,
        padding: '8px 10px',
      }}
    >
      <div style={{ minWidth: 0, flex: 1 }}>
        <div
          style={{
            fontSize: 12.5,
            fontWeight: 600,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            fontFamily: mono ? 'var(--font-mono)' : undefined,
          }}
        >
          {name}
        </div>
        <div style={{ fontSize: 11, color: c.text3, marginTop: 1 }}>{detail}</div>
      </div>
      {count && (
        <Typography.Text style={{ fontSize: 12, flexShrink: 0 }} className="ui-num">
          {count}
        </Typography.Text>
      )}
      <Tooltip title={hint}>
        <span style={{ flexShrink: 0 }}>
          <StatusPill tone={tone} size="sm">{state}</StatusPill>
        </span>
      </Tooltip>
    </div>
  )
}
