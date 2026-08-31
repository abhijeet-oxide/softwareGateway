import { Progress, Tag, Tooltip, Typography } from 'antd'
import {
  CheckCircleFilled, ClockCircleOutlined, CloseCircleFilled, LoadingOutlined, SyncOutlined,
} from '../icons'
import type { ContentGroup, PromotionProgress as Promotion, Strategy } from '../api/types'
import { formatBytes, formatCount, formatSpeed, formatAbsolute, formatDuration } from '../domain/format'
import { useByteRate } from '../domain/rate'
import { NA } from './value'
import { c } from '../uikit'

/**
 * The honest-numbers rule, made structural (docs/design/18 §6.1, 19 §6).
 *
 * There are two components here and they are not interchangeable:
 *
 *   <MeasuredProgress> takes REAL BYTE COUNTS and is the only thing in this
 *   application that may render a bar, a percentage, a speed or an ETA.
 *
 *   <StateStrip> takes TIMESTAMPS and a state, and cannot be made to look like
 *   a bar however it is styled.
 *
 * For the JFrog step we move the bytes, so we count them. For the Quay step
 * Quay pulls the content itself once we have configured it: we can see THAT a
 * sync started, THAT it finished and WHAT it produced, but not how many bytes
 * moved. A progress bar there would be a number derived from a timer, and
 * somebody would make a decision from it.
 *
 * MeasuredProgress refuses to render for a non-`copy` strategy, so the rule
 * cannot be broken by a caller who has not read this comment.
 *
 * A third case sits between them: work that IS ours and IS running, but whose
 * extent nobody knows yet - measuring a release walks a manifest tree whose
 * size is the thing being discovered. <WorkingBar> is for that. It animates,
 * so it reads as activity, and it carries no number, so it cannot be read as
 * a position.
 */

interface MeasuredProps {
  /** Bytes actually transferred. */
  transferred: number | undefined
  /** Bytes planned. */
  total: number | undefined
  /**
   * Bytes that did not have to move because the destination already had them.
   *
   * DONE WORK, and the bar has to say so. A release the destination already
   * holds is finished the moment planning discovers that, and a bar reading
   * `0 B of 63.7 GB · 0%` beside a summary reading `Saved 56.5 GB` was
   * describing a download that had nothing left to do as one that had not
   * started. The saved portion is drawn in its own colour, so "already there"
   * and "we moved it" stay distinguishable.
   */
  saved?: number | undefined
  /**
   * How the transfer is being performed. Anything but `copy` means the bytes
   * were not ours to count, and this component renders nothing.
   */
  strategy?: Strategy
  showText?: boolean
  speedBytesPerSecond?: number | undefined
}

export function MeasuredProgress({
  transferred, total, saved, strategy = 'copy', showText = true, speedBytesPerSecond,
}: MeasuredProps) {
  if (strategy !== 'copy') {
    // Not a fallback - a refusal. A caller reaching here has asked for a bar
    // over work whose bytes nobody counted.
    return (
      <NA reason="This work is performed by the destination registry, so there are no bytes for us to count. Its state is shown instead." />
    )
  }
  if (transferred === undefined || total === undefined || total <= 0) {
    return (
      <NA reason="The size of this work has not been established yet, so there is no percentage to show." />
    )
  }

  const alreadyThere = Math.max(0, Math.min(saved ?? 0, total))
  const done = Math.min(total, transferred + alreadyThere)
  const percent = (done / total) * 100
  const savedPercent = (alreadyThere / total) * 100

  return (
    <div>
      <Progress
        percent={Number(percent.toFixed(1))}
        // The green portion is what was already there. Ant Design draws it
        // from the left inside the same track, which is the right shape: it is
        // part of the same total, not a second bar.
        success={alreadyThere > 0 ? { percent: Number(savedPercent.toFixed(1)) } : undefined}
        size="small"
        strokeColor={percent >= 100 ? c.ok : undefined}
        // Stated, not inferred. Ant derives this from the percentage only when
        // no `success` segment is present, so two finished rows in one column
        // were drawing their label in two different colours.
        status={percent >= 100 ? 'success' : 'normal'}
        /*
          ALWAYS THE PERCENTAGE, never the component library's tick.

          Ant infers a success status at 100% and swaps the number for a check
          - but only when no `success` segment was passed. So in a column of
          these, a row that finished with nothing already present said tick and
          the row beside it said 100%, purely because of a prop the caller had
          no idea was load-bearing. One column, one vocabulary; the bar is
          already fully green when it is done.
        */
        format={(p) => `${Math.round(p ?? 0)}%`}
      />
      {showText && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {formatBytes(done)} of {formatBytes(total)}
          {alreadyThere > 0 && ` · ${formatBytes(alreadyThere)} already there`}
          {speedBytesPerSecond !== undefined && ` · ${formatSpeed(speedBytesPerSecond)}`}
        </Typography.Text>
      )}
    </div>
  )
}

/**
 * A NATIVE PROMOTION's progress, counted in NAMES.
 *
 * A third honest denominator, alongside bytes and artifacts, and it exists for
 * the same reason the other two are kept apart: it is the only number this
 * work actually has. A registry relocating a release inside itself already
 * holds every blob, so nothing crosses the wire and nothing is ours to count.
 * What it publishes is names - the release's tag, and the name each bundled
 * component answers to - so those are what a bar over it can mean.
 *
 * This is a real bar rather than a <StateStrip> precisely because that
 * denominator is real: the names were recorded when the promotion was opened
 * and are marked off as they land, so the percentage is a count of finished
 * work over known work, not a timer.
 */
export function PromotionProgress({ promotion }: { promotion: Promotion | undefined }) {
  if (!promotion || promotion.namesTotal <= 0) {
    return (
      <NA reason="The registry is promoting this release. Nothing published yet." />
    )
  }

  const done = Math.min(promotion.namesDone, promotion.namesTotal)
  const percent = (done / promotion.namesTotal) * 100
  const failed = promotion.state === 'FAILED'

  return (
    <div>
      <Progress
        percent={Number(percent.toFixed(1))}
        size="small"
        status={failed ? 'exception' : undefined}
        strokeColor={percent >= 100 && !failed ? c.ok : undefined}
      />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {/*
          "tags" rather than "names", which is what the API and the schema call
          them. A name is a repository path plus a tag; `repo:tag` is what an
          operator says, and this is the line they read.

          The second clause is on every one of these, because a reader who has
          only ever seen byte bars reads "45 GB in four seconds" as a bug.
        */}
        {formatCount(done)} of {formatCount(promotion.namesTotal)} tags published
        {' · no bytes transferred'}
      </Typography.Text>
      {promotion.lastError && (
        <Typography.Text
          type="danger"
          style={{ display: 'block', fontSize: 12, marginTop: 4 }}
        >
          {promotion.lastError}
        </Typography.Text>
      )}
    </div>
  )
}

/**
 * What a transfer's content adds up to, over both populations that matter.
 *
 * COMPONENTS are what the destination can be said to HOLD: an image is copied
 * when its last layer and its manifest have both landed, and not a moment
 * before, because half an image is not an image.
 *
 * UNITS are the individual pushes underneath - every layer, config and
 * manifest that is a job of its own. They are what MOVES. A release of 260
 * components is sixty thousand of them, and the difference between the two
 * populations is the difference between a bar that sits at nought for an hour
 * and one that tells somebody how the hour is going.
 */
export function copyCounts(groups?: ContentGroup[]) {
  return (groups ?? []).reduce(
    (acc, g) => ({
      components: acc.components + g.total,
      componentsDone: acc.componentsDone + g.copied + g.present,
      componentsPresent: acc.componentsPresent + g.present,
      units: acc.units + (g.units ?? 0),
      unitsDone: acc.unitsDone + (g.unitsCopied ?? 0) + (g.unitsPresent ?? 0),
      unitsPresent: acc.unitsPresent + (g.unitsPresent ?? 0),
      unitsOutstanding: acc.unitsOutstanding + (g.unitsOutstanding ?? 0),
      failed: acc.failed + (g.unitsFailed ?? g.failed ?? 0),
    }),
    {
      components: 0, componentsDone: 0, componentsPresent: 0,
      units: 0, unitsDone: 0, unitsPresent: 0, unitsOutstanding: 0, failed: 0,
    },
  )
}

/**
 * ONE BAR, over what the destination actually holds.
 *
 * # What it is drawn from, and why that changed
 *
 * It used to count COMPONENTS, and a component only counts once every layer
 * beneath it and the manifest naming it have landed. So a download of a
 * 30 GB release read `0 of 260 · 0%` for its entire first hour - with a
 * hundred workers visibly streaming layers on the same screen, and a saving
 * of twelve gigabytes already discovered and reported two lines below. A bar
 * that is nought while the work is a third done is not a conservative bar, it
 * is a wrong one, and it is the one everybody was looking at.
 *
 * It is now drawn from BYTES on the destination - what we have streamed so
 * far, including the part of a blob a worker is streaming right now, plus what
 * was already there. Both are the distinct-content account, so one population,
 * converging on the release's own size. That is the number that tracks the
 * wait: it starts moving with the first megabyte and it counts the saving as
 * the done work it is.
 *
 * Where a transfer has no measured size yet the bar falls back to layers, and
 * then to components - each less granular than the last, and each still a
 * count of finished work over known work rather than anything derived from a
 * clock.
 *
 * # Why it cannot reach the end early
 *
 * Bytes finish before a download does: the manifests that name the content are
 * pushed last and weigh almost nothing, so a byte bar would sit at 100% with
 * real work outstanding. While a single unit is still to push the bar is held
 * just short of the end, so "full" and "finished" stay the same statement.
 *
 * # The green portion
 *
 * What the destination already held. Drawn inside the same track rather than
 * as a second bar: it is part of the same total, and the distinction that
 * matters is between what we moved and what was there - not between two kinds
 * of progress.
 */
export function CopyProgress({
  groups, transferred, total, saved, strategy = 'copy', speedBytesPerSecond, live,
}: {
  /** The per-kind rollup - the same rows the Contents table renders. */
  groups?: ContentGroup[]
  /** Bytes moved, the release's size, and bytes already there. */
  transferred: number | undefined
  total: number | undefined
  saved?: number | undefined
  strategy?: Strategy
  speedBytesPerSecond?: number | undefined
  /** Whether the transfer is still going, which is all the animation means. */
  live?: boolean
}) {
  if (strategy !== 'copy') {
    return (
      <NA reason="This work is performed by the destination registry, so there is nothing here for us to count. Its state is shown instead." />
    )
  }

  const n = copyCounts(groups)
  const moved = Math.max(0, transferred ?? 0)
  const present = Math.max(0, saved ?? 0)
  const weighed = total !== undefined && total > 0

  // In order of how closely each tracks the wait. Bytes first because they are
  // the only one that moves while a large layer is in flight.
  const fraction = weighed
    ? Math.min(1, (moved + present) / total)
    : n.units > 0
      ? n.unitsDone / n.units
      : n.components > 0
        ? n.componentsDone / n.components
        : undefined

  // Before planning there is no denominator and no bar to draw. Said rather
  // than rendered as 0%, which is a position nobody measured.
  if (fraction === undefined) {
    return (
      <NA reason="This download has not been planned yet, so what it is made of is not known." />
    )
  }

  // Something still to push, by the finest population we have. This is what
  // keeps a byte bar off 100% while the manifests are still going up.
  const outstanding = n.units > 0
    ? n.unitsOutstanding > 0
    : n.components > 0
      ? n.componentsDone < n.components
      : false

  const percent = outstanding ? Math.min(fraction * 100, 99.9) : fraction * 100
  const presentFraction = weighed
    ? Math.min(1, present / total)
    : n.units > 0
      ? n.unitsPresent / n.units
      : n.components > 0 ? n.componentsPresent / n.components : 0
  const complete = !outstanding && percent >= 100

  return (
    <div>
      <Progress
        percent={Number(percent.toFixed(1))}
        success={presentFraction > 0
          ? { percent: Number((presentFraction * 100).toFixed(1)) }
          : undefined}
        size="small"
        status={n.failed > 0
          ? 'exception'
          : complete ? 'success' : live ? 'active' : 'normal'}
        strokeColor={complete ? c.ok : undefined}
      />
      {weighed && (
        <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block' }}>
          {/*
            ON THE TARGET, not "moved". The two are different numbers and the
            reader wants the first one: what is there now, however it got
            there. What we moved and what was already there follow it, because
            that is the COST, and the cost is a second question.
          */}
          {formatBytes(Math.min(total, moved + present))} of {formatBytes(total)} on the target
          {present > 0 && ` · ${formatBytes(present)} already there`}
          {moved > 0 && present > 0 && ` · ${formatBytes(moved)} downloaded`}
          {speedBytesPerSecond !== undefined && ` · ${formatSpeed(speedBytesPerSecond)}`}
        </Typography.Text>
      )}
      {(n.units > 0 || n.components > 0) && (
        <Typography.Text type="secondary" style={{ fontSize: 11, display: 'block' }}>
          {n.units > 0 && `${formatCount(n.unitsDone)} of ${formatCount(n.units)} layers`}
          {n.units > 0 && n.components > 0 && ' · '}
          {n.components > 0
            && `${formatCount(n.componentsDone)} of ${formatCount(n.components)} components complete`}
          {n.failed > 0 && ` · ${formatCount(n.failed)} failed`}
        </Typography.Text>
      )}
    </div>
  )
}

/**
 * A download's progress in ONE CELL: how far, how long, how much longer.
 *
 * The three belong together. A bar alone says how far without saying whether
 * that took a minute or an afternoon; an elapsed column alone says how long
 * without saying how far. And the number an operator actually wants - when it
 * will be done - is the one that is never on screen, because it is the only
 * one of the three that has to be derived.
 *
 * The ETA is derived HONESTLY or not at all: bytes we moved, over the time we
 * spent moving them, applied to what is left. It is absent for a settled
 * download (there is nothing left), for a delegated one (the bytes were not
 * ours to count), and while nothing has moved yet (a rate from a zero
 * numerator is a guess wearing a number's clothes).
 */
/**
 * WHEN IT WILL BE DONE, derived one way for every surface that shows it.
 *
 * This exists because it was derived twice and the two disagreed on screen: the
 * downloads list said ~3h 22m left while the download's own page said 8h 17m
 * about the same transfer at the same moment. Neither number was a rounding
 * difference and a reader had no way to tell which to believe.
 *
 * The cause was two different accounts of what is LEFT. The detail page used
 * the planner's `outstandingBytes`, which counts per (repository, digest): a
 * component published under its own name as well as inside the bundle has its
 * layers planned twice, and the second copy costs no bytes because the registry
 * mounts it. That account says a 27 GB release has 58 GB left to move, so the
 * page multiplied its own honest speed by an inflated distance. The page had
 * already moved its BAR and its saving onto the distinct-content account for
 * exactly this reason - the ETA was simply left behind.
 *
 * So: what is left is the release's own content, less what we moved and less
 * what the destination already had. Bytes we moved over the time we spent
 * moving them is the rate. Undefined rather than wrong when either half is
 * missing - a rate from a zero numerator is a guess wearing a number's clothes.
 */
export function etaSeconds({
  transferred, total, saved, elapsedSeconds,
}: {
  transferred: number | undefined
  total: number | undefined
  saved: number | undefined
  elapsedSeconds: number | undefined
}): number | undefined {
  if (!elapsedSeconds || elapsedSeconds <= 0) return undefined
  if (!transferred || transferred <= 0) return undefined
  if (total === undefined) return undefined

  const speed = transferred / elapsedSeconds
  // Counting the saved bytes as remaining is how a download with nothing left
  // to do came to report an ETA of forever.
  const remaining = Math.max(0, total - transferred - (saved ?? 0))
  if (remaining <= 0) return undefined
  return remaining / speed
}

export function DownloadProgress({
  transferred, total, saved, groups, strategy = 'copy', elapsedSeconds, live, heldBy,
}: {
  transferred: number | undefined
  total: number | undefined
  /** Bytes the destination already had. Done work - see MeasuredProgress. */
  saved?: number | undefined
  /** Artifact groups, when available, so the percentage matches the detail view. */
  groups?: ContentGroup[]
  strategy?: Strategy
  /** Seconds spent moving bytes so far. */
  elapsedSeconds: number | undefined
  /** Whether this download is still going. */
  live: boolean
  /**
   * Why nothing is moving it, when nothing is - two or three words from
   * domain/fleet.
   *
   * It suppresses the ETA, which is the point. The arithmetic still produces
   * a number for a download with no worker behind it, and that number is a
   * fiction with two decimal places sitting in the same cell as the reason
   * there will be no arrival.
   */
  heldBy?: string
}) {
  /*
    TWO SPEEDS, because they answer two questions and this cell was showing
    neither.

    The average - bytes moved over the download's whole life - is what an
    estimate rests on: what is left will be moved at roughly the rate
    everything so far was, and an ETA that swung by ten minutes every poll
    would be worse than none. It is computed inside `etaSeconds` rather than
    here, so that this cell and the download's own page cannot arrive at two
    different answers - which they did, by ~5 hours, on one download.

    The LIVE rate is what belongs on screen. The average is dragged down by
    every second the download existed and did not move - planning, the wait for
    a worker, a stall - so a download queued for twenty minutes and now moving
    at 90 MB/s averages a couple of megabytes a second. This cell used to
    compute that number and then not render it at all, which at least spared
    the reader; the number that is worth rendering is the one below.
  */
  const rate = useByteRate(transferred, live)

  // What is LEFT, which is neither moved nor already there. Counting the saved
  // bytes as remaining is how a download with nothing left to do came to
  // report an ETA of forever.
  const remaining = total !== undefined && transferred !== undefined
    ? Math.max(0, total - transferred - (saved ?? 0))
    : undefined
  // A held download is going nowhere, so there is no arrival to estimate.
  const eta = live && !heldBy
    ? etaSeconds({ transferred, total, saved, elapsedSeconds })
    : undefined
  const notStarted = live
    && (elapsedSeconds ?? 0) <= 0
    && (transferred ?? 0) <= 0
    && (total ?? 0) <= 0

  return (
    <div style={{ minWidth: 180 }}>
      {!notStarted && (
        groups?.some((group) => group.total > 0)
          ? <CopyProgress
              groups={groups}
              transferred={transferred}
              total={total}
              saved={saved}
              strategy={strategy}
              speedBytesPerSecond={live ? rate.current : undefined}
              live={live}
            />
          : <MeasuredProgress
              transferred={transferred}
              total={total}
              saved={saved}
              strategy={strategy}
              showText={false}
            />
      )}
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {notStarted ? 'Not started' : `Elapsed: ${formatDuration(elapsedSeconds) ?? 'Unavailable'}`}
        {eta !== undefined ? ` · ~${formatDuration(eta)} left` : ''}
        {heldBy ? ` · ${heldBy}` : ''}
        {/*
          "Estimating" only while there is something left to estimate. A
          download whose content the destination already holds has nothing
          remaining, and saying it is estimating an arrival is describing a
          wait that is over.
        */}
        {live && !notStarted && eta === undefined && remaining === 0 ? ' · No remaining work' : ''}
        {live && !heldBy && eta === undefined && remaining !== 0 && strategy === 'copy'
          ? ' · estimating…'
          : ''}
      </Typography.Text>
    </div>
  )
}

export type StripState = 'pending' | 'running' | 'done' | 'failed'

interface StripProps {
  state: StripState
  /** What is true, in words. Never a number we did not measure. */
  label: string
  /** The timestamps that ARE the report for this kind of work. */
  events?: { label: string; at?: string }[]
  /** The destination's own message, verbatim, when it failed. */
  message?: string
}

/**
 * State and timestamps, for work performed by somebody else.
 *
 * Deliberately a row of labelled moments rather than anything with a track or
 * a fill, so it can never be misread as measured progress at a glance.
 */
export function StateStrip({ state, label, events = [], message }: StripProps) {
  const icon = {
    pending: <ClockCircleOutlined style={{ color: c.text2 }} />,
    running: <SyncOutlined spin style={{ color: c.brand }} />,
    done: <CheckCircleFilled style={{ color: c.ok }} />,
    failed: <CloseCircleFilled style={{ color: c.danger }} />,
  }[state]

  return (
    <div
      style={{
        border: '1px dashed ${c.borderStrong}',
        borderRadius: 4,
        padding: '10px 12px',
        background: c.surface2,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: events.length ? 8 : 0 }}>
        {icon}
        <Typography.Text strong>{label}</Typography.Text>
      </div>

      {events.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          {events.map((e) => (
            <Tooltip key={e.label} title={e.at ? formatAbsolute(e.at) : 'Not reached yet'}>
              <Tag color={e.at ? 'default' : undefined} style={{ marginInlineEnd: 0 }}>
                {e.label}: {e.at ? new Date(e.at).toLocaleTimeString('en-GB', {
                  hour: '2-digit', minute: '2-digit',
                }) : <NA />}
              </Tag>
            </Tooltip>
          ))}
        </div>
      )}

      {message && (
        <Typography.Paragraph
          type={state === 'failed' ? 'danger' : 'secondary'}
          style={{ marginTop: 8, marginBottom: 0, fontSize: 12 }}
        >
          {message}
        </Typography.Paragraph>
      )}
    </div>
  )
}


/**
 * Work in progress whose extent is not known.
 *
 * Deliberately NOT a percentage. Measuring a release walks a manifest tree to
 * find out how big it is - the total is the answer, not the input - so any
 * percentage would be invented. What is true and worth showing is that it is
 * running, what it is doing, and how long it has been doing it.
 *
 * The stripe slides rather than fills, so it cannot be mistaken for a position
 * at a glance. Elapsed time is the honest counterpart to a percentage: it does
 * not say how far along, but it does tell somebody whether to keep waiting.
 */
export function WorkingBar({
  label, detail, elapsedSeconds,
}: {
  label: string
  detail?: string
  elapsedSeconds?: number
}) {
  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
        <LoadingOutlined spin style={{ color: c.brand }} />
        <Typography.Text strong style={{ fontSize: 13 }}>{label}</Typography.Text>
        {elapsedSeconds !== undefined && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {formatDuration(elapsedSeconds)} elapsed
          </Typography.Text>
        )}
      </div>

      <div
        role="progressbar"
        aria-label={label}
        aria-busy="true"
        style={{
          height: 6,
          borderRadius: 3,
          background: c.surface2,
          overflow: 'hidden',
        }}
      >
        <div
          style={{
            height: '100%',
            width: '35%',
            borderRadius: 3,
            background: c.brand,
            animation: 'slm-working 1.4s ease-in-out infinite',
          }}
        />
      </div>

      {detail && (
        <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 6 }}>
          {detail}
        </Typography.Text>
      )}
    </div>
  )
}
