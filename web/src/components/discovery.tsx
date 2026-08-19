import { useEffect, useState } from 'react'
import {
  App, Button, Card, Modal, Progress, Select, Space, Table, Tag, Tooltip, Typography,
} from 'antd'
import {
  CheckCircleFilled, ExclamationCircleFilled, PlayCircleOutlined, SyncOutlined,
} from '@ant-design/icons'
import { useDiscoveryStatuses, useRunDiscovery } from '../api/queries'
import { useCan } from '../auth/permissions'
import { formatCount, formatDuration } from '../domain/format'
import { matches } from '../domain/derive'
import { SearchBar } from './layout'
import { NA, Value } from './value'
import { TimeAgo } from './chips'
import { palette, semantic } from '../theme'
import type { DiscoverySourceState, Product } from '../api/types'

/**
 * What discovery is doing, and what it last found.
 *
 * # Why this is a panel and not a spinner
 *
 * Discovery is the operation a Product Owner triggers most and understands
 * least: it polls several vendor registries, each covering several
 * repositories, each carrying many tags, and against a slow registry it runs
 * for minutes. A button that starts it and reports nothing is indistinguishable
 * from a button that does nothing - which is exactly how it read.
 *
 * The API already reports all of it (phase, repositories, tags, artifacts, new
 * packages, errors, and a summary of the last completed run), so this shows
 * what is actually happening rather than an animation standing in for it.
 *
 * # What it will not do
 *
 * Show a percentage before there is one. Until enumeration finishes,
 * `repositoriesTotal` is zero - the scan is still waiting on the registry's
 * catalog and nobody knows how much work there is. That state is named, not
 * rendered as 0%.
 */

const PHASE: Record<string, string> = {
  ENUMERATING_REPOSITORIES: 'Finding repositories',
  LISTING_TAGS: 'Listing versions',
  RESOLVING_TAGS: 'Reading version details',
}

function phaseLabel(phase?: string): string {
  return phase ? (PHASE[phase] ?? phase) : 'Scanning'
}

/**
 * The first sentence of a scan failure, for a row that has one line for it.
 *
 * Registry errors arrive as a nested chain - what we were doing, what the
 * client said, what the proxy said, what DNS said - and the whole of it is
 * worth keeping. It is not worth putting all four clauses in a table cell,
 * where it buries the other three sources. So the row carries the part that
 * names the problem and the tooltip carries the chain verbatim.
 *
 * Truncated, never reworded: an error a Product Owner pastes to an engineer
 * has to be the error the engineer can search for.
 */
function summariseError(message: string): string {
  const head = message.split(': Get ')[0] ?? message
  return head.length > 120 ? `${head.slice(0, 117)}…` : head
}

/**
 * When this source will next be polled, counting down.
 *
 * # Why a countdown and not an interval
 *
 * "next scan in under 15 minutes" was true for the entire fifteen minutes and
 * never moved, so it answered "how often" when the question is "how long". The
 * scheduler polls each source `intervalSeconds` after its last run, so the
 * remaining time is `lastRunAt + interval - now` - a real instant, worth
 * counting toward.
 *
 * It ticks once a second, and only while it is on screen: the component
 * unmounts with the panel, so nothing keeps running behind a page nobody is
 * looking at.
 */
function NextScan({ source }: { source: DiscoverySourceState }) {
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])

  if (!source.intervalSeconds) {
    return (
      <Space size={6}>
        <CheckCircleFilled style={{ color: semantic.success }} />
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          Polled on demand only
        </Typography.Text>
      </Space>
    )
  }

  const last = source.lastRunAt ? Date.parse(source.lastRunAt) : undefined
  // Without a last run there is no anchor to count from, so the interval is
  // stated as a fact about the schedule rather than as a countdown that would
  // be starting from a moment we invented.
  const remaining =
    last === undefined || Number.isNaN(last)
      ? undefined
      : Math.max(0, (last + source.intervalSeconds * 1000 - now) / 1000)

  return (
    <Space size={6}>
      <CheckCircleFilled style={{ color: semantic.success }} />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {remaining === undefined ? (
          <>Next scan every <Value>{formatDuration(source.intervalSeconds)}</Value></>
        ) : remaining <= 0 ? (
          'Next scan due now'
        ) : (
          <>Next scan in <Value>{formatDuration(remaining)}</Value></>
        )}
      </Typography.Text>
    </Space>
  )
}

/** A source's progress, or the reason there is no percentage yet. */
function SourceProgress({ s }: { s: DiscoverySourceState }) {
  if (!s.scanning) {
    if (s.lastError) {
      return (
        <Space size={6} align="start" style={{ width: '100%' }}>
          <ExclamationCircleFilled style={{ color: semantic.error, marginTop: 3 }} />
          <Tooltip title={s.lastError}>
            <Typography.Text
              type="danger"
              style={{ fontSize: 12 }}
              ellipsis={{ tooltip: false }}
            >
              {summariseError(s.lastError)}
            </Typography.Text>
          </Tooltip>
        </Space>
      )
    }
    return <NextScan source={s} />
  }

  // Until the catalog comes back there is no denominator, and a bar without one
  // would be a number nobody measured.
  if (!s.repositoriesTotal) {
    return (
      <Space size={6}>
        <SyncOutlined spin style={{ color: palette.primary }} />
        <Typography.Text style={{ fontSize: 12 }}>
          {phaseLabel(s.phase)} - waiting for the registry to list what it holds
        </Typography.Text>
      </Space>
    )
  }

  /*
   * ONE BAR, START TO FINISH.
   *
   * It was the repositories, which reached 100% the moment the last
   * `tags/list` returned with the whole of the work still to come. Then it was
   * whichever phase was live, which was accurate at every instant and useless
   * across them: it filled for the repositories, reset, filled again for the
   * versions, reset again. A bar that reaches the end and starts over is
   * reporting phases, and the phase is already written underneath it.
   *
   * So the server puts every phase on one scale and keeps it monotonic, and
   * this draws that and nothing else.
   */
  // Capped below 100: the scan is still running, and the last percent belongs
  // to the moment it says so itself.
  const percent = Math.min(99, (s.progress ?? 0) * 100)

  return (
    <Space direction="vertical" size={2} style={{ width: '100%' }}>
      <Progress percent={Number(percent.toFixed(0))} size="small" status="active" />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {scanPosition(s)}
      </Typography.Text>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {scanFindings(s)}
      </Typography.Text>
    </Space>
  )
}

/**
 * Where the scan has got to, in the units of whatever it is doing.
 *
 * Named in the phase's own terms rather than in one set of units for all three:
 * "38 of 48 repositories" and "1,204 of 3,180 versions" are both true at
 * different moments, and calling either of them "items" to make one sentence
 * work would make both of them vague.
 */
function scanPosition(s: DiscoverySourceState): string {
  const inFlight = s.tagsInFlight ?? s.repositoriesInFlight ?? 0
  const parallel = inFlight > 0 ? ` · ${inFlight} in parallel` : ''
  const where = s.currentTag
    ? ` · ${s.currentRepository ? `${s.currentRepository}:` : ''}${s.currentTag}`
    : s.currentRepository ? ` · ${s.currentRepository}` : ''

  if (s.phase === 'LISTING_TAGS') {
    return `Finding versions · ${formatCount(s.repositoriesDone ?? 0)} of `
      + `${formatCount(s.repositoriesTotal)} repositories${parallel}${where}`
  }
  if (s.phase === 'RESOLVING_TAGS') {
    // Two passes, and they have different denominators: every version is
    // checked, and only the new ones are read.
    const reading = (s.tagsToFetch ?? 0) > 0 && (s.tagsChecked ?? 0) >= (s.tagsTotal ?? 0)
    return reading
      ? `Reading new releases · ${formatCount(s.tagsFetched ?? 0)} of `
        + `${formatCount(s.tagsToFetch ?? 0)}${parallel}${where}`
      : `Checking versions · ${formatCount(s.tagsChecked ?? 0)} of `
        + `${formatCount(s.tagsTotal ?? 0)}${parallel}${where}`
  }
  return `${phaseLabel(s.phase)}${parallel}${where}`
}

/**
 * What the scan has FOUND so far.
 *
 * Live, because "is it finding anything?" is asked while it is still looking -
 * and until these counters moved during the scan rather than at the end of it,
 * the only answer available was "wait and see".
 */
function scanFindings(s: DiscoverySourceState): string {
  const parts: string[] = []
  if (s.packages) parts.push(`${formatCount(s.packages)} releases seen`)
  if (s.newPackages) parts.push(`${formatCount(s.newPackages)} new`)
  if (s.artifacts) parts.push(`${formatCount(s.artifacts)} manifests read`)
  if (s.errors) parts.push(`${formatCount(s.errors)} errors`)
  return parts.length > 0 ? parts.join(' · ') : 'nothing found yet'
}

/**
 * The Run Discovery control, and the only one in the application.
 *
 * A component rather than a bare button because the button is the easy half:
 * the half that matters is reporting what the request ACTUALLY did, and a
 * second copy of this elsewhere would inevitably be a `mutate()` with no
 * feedback - which is precisely how this read as broken.
 *
 * `product` fixes the scan to one product and hides the chooser; leaving it out
 * offers every product, with "all of them" as the default.
 */
export function RunDiscoveryButton({
  products, product, block,
}: {
  products: Product[]
  /** Fixes the scan to one product and removes the choice. */
  product?: string
  block?: boolean
}) {
  const { message } = App.useApp()
  const run = useRunDiscovery()
  const mayOperate = useCan('operate', product ? { product } : undefined)

  const [asking, setAsking] = useState(false)
  const [chosen, setChosen] = useState<string | undefined>(product)

  const start = async () => {
    try {
      const result = await run.mutateAsync(product ?? chosen)
      setAsking(false)

      // The response is the answer, and discarding it is what made this button
      // look broken. `alreadyRunning` is a real outcome, not a failure.
      //
      // Discriminated on `products`, not on `started`: both shapes carry a
      // `started` key and it means a different thing in each - a count in the
      // fleet-wide response, an object in the per-product one.
      const fleet = 'products' in result
      const started = fleet ? result.started : (result.started?.sources ?? 0)
      const already = fleet
        ? (result.alreadyRunning ?? 0)
        : (result.started?.alreadyRunning ?? 0)
      const scope = product ?? chosen

      if (started > 0) {
        message.success(
          `Discovery started on ${started} source${started === 1 ? '' : 's'}` +
          (scope ? ` for ${scope}.` : ' across every product.'),
        )
      } else if (already > 0) {
        message.info(
          `A scan is already running on ${already} source${already === 1 ? '' : 's'}. ` +
          'Its progress is shown on the Overview.',
        )
      } else {
        message.warning(
          'Nothing was started: no source of this product has discovery enabled in configuration.',
        )
      }
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'Discovery could not be started.')
    }
  }

  return (
    <>
      <Tooltip
        title={
          mayOperate
            ? 'Poll the vendor registries now rather than waiting for the next scheduled scan.'
            : 'You do not have permission to run discovery.'
        }
      >
        <Button
          type="primary"
          block={block}
          icon={<PlayCircleOutlined />}
          disabled={!mayOperate}
          loading={run.isPending}
          onClick={() => setAsking(true)}
        >
          Run Discovery
        </Button>
      </Tooltip>

      <Modal
        open={asking}
        title="Run discovery"
        okText="Run discovery"
        confirmLoading={run.isPending}
        onOk={() => void start()}
        onCancel={() => setAsking(false)}
      >
        <Typography.Paragraph type="secondary">
          Polls the vendor registries for releases published since the last scan.
          {product
            ? ` This scans ${product} only.`
            : ' Choose a product to scan only that one, or leave it blank to scan every product.'}
        </Typography.Paragraph>

        {!product && (
          <Select
            style={{ width: '100%' }}
            allowClear
            placeholder="Every product"
            value={chosen}
            onChange={setChosen}
            options={products.map((p) => ({
              value: p.productId,
              label: p.displayName || p.productId,
            }))}
          />
        )}

        <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0, fontSize: 12 }}>
          The scan runs in the background and its progress appears on the Overview. Nothing is
          downloaded - discovery only records what the vendor has published.
        </Typography.Paragraph>
      </Modal>
    </>
  )
}

/**
 * Rescan ONE product, from the row that shows what it last found.
 *
 * Deliberately not a confirmation: a scan is read-only - it records what the
 * vendor has published and downloads nothing - so the cost of an accidental
 * click is one registry poll. The fleet-wide button keeps its dialog, because
 * that one starts a scan against every registry the deployment can reach.
 */
function DiscoverSource({ product, scanning }: { product: string; scanning?: boolean }) {
  const { message } = App.useApp()
  const run = useRunDiscovery()
  const mayOperate = useCan('operate', { product })

  const start = async () => {
    try {
      const result = await run.mutateAsync(product)
      const started = 'products' in result ? result.started : (result.started?.sources ?? 0)
      const already = 'products' in result
        ? (result.alreadyRunning ?? 0)
        : (result.started?.alreadyRunning ?? 0)
      message.success(
        started > 0
          ? `Scanning ${started} source${started === 1 ? '' : 's'} of ${product}. Progress is in this panel.`
          : already > 0
            ? `${product} is already being scanned - ${already} source${already === 1 ? '' : 's'} in progress.`
            : `Nothing to scan: ${product} has no source with discovery enabled.`,
      )
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'Discovery could not be started.')
    }
  }

  return (
    <Tooltip
      title={
        mayOperate
          ? `Look at ${product}'s registries now. Nothing is downloaded.`
          : 'You do not have permission to run discovery.'
      }
    >
      <Button
        size="small"
        icon={scanning ? <SyncOutlined spin /> : <PlayCircleOutlined />}
        disabled={!mayOperate || scanning}
        loading={run.isPending}
        onClick={() => void start()}
      >
        {scanning ? 'Scanning' : 'Discover'}
      </Button>
    </Tooltip>
  )
}

export function DiscoveryPanel({ products }: { products: Product[] }) {
  const names = products.map((p) => p.productId)
  const results = useDiscoveryStatuses(names)

  const loading = results.some((r) => r.isLoading)
  // A follower replica runs no discovery loop and says so, which is a different
  // answer from "nothing is scheduled".
  const leaderElsewhere = results.length > 0 && results.every((r) => r.data && !r.data.running)

  const allRows: DiscoverySourceState[] = results.flatMap((r) => r.data?.sources ?? [])
  const scanning = allRows.filter((s) => s.scanning)

  /*
   * A product has several sources and a deployment has several products, so
   * this table is tens of rows before anything unusual happens. Finding the
   * one somebody wants to rescan by reading down the list is the difference
   * between a table and a filing cabinet.
   */
  const [search, setSearch] = useState('')
  const rows = search.trim()
    ? allRows.filter((s) => matches(search, s.source, s.product, s.currentRepository))
    : allRows

  // The most recent completed scan across every source - the honest answer to
  // "when did we last look", rather than a transfer's timestamp standing in for
  // one.
  const lastRunAt = allRows
    .map((s) => s.lastRunAt)
    .filter((t): t is string => Boolean(t))
    .sort()
    .at(-1)

  const newSinceLastRun = allRows.reduce((n, s) => n + (s.lastNewPackages ?? 0), 0)
  const errors = allRows.filter((s) => s.lastError)


  return (
    <>
      <Card
        title={
          <Space size={8}>
            Discovery
            {scanning.length > 0 && (
              <Tag icon={<SyncOutlined spin />} color="processing">
                {scanning.length} source{scanning.length === 1 ? '' : 's'} scanning
              </Tag>
            )}
          </Space>
        }
        loading={loading}
        extra={
          <Space size={12}>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Last run:{' '}
              {lastRunAt ? <TimeAgo at={lastRunAt} /> : (
                <NA reason="No scan has completed since this Coordinator started." />
              )}
            </Typography.Text>
            <RunDiscoveryButton products={products} />
          </Space>
        }
      >
        {leaderElsewhere ? (
          <Typography.Text type="secondary">
            Discovery runs on the leader, and this replica is a follower. Scans are still happening -
            they are simply not being run from here, so there is no progress to show.
          </Typography.Text>
        ) : allRows.length === 0 && !loading ? (
          <Typography.Text type="secondary">
            No source has discovery enabled in configuration, so nothing is polled automatically.
          </Typography.Text>
        ) : (
          <>
            <Space size={24} style={{ marginBottom: 12 }} wrap>
              {/*
                One sentence, read left to right. It used to be a label, a
                colon and a number - "Found on the last run: 0 new releases" -
                which reads as a form field rather than as the answer to the
                question the panel exists for.
              */}
              <Typography.Text type="secondary" style={{ fontSize: 13 }}>
                Found{' '}
                <Typography.Text strong>
                  <Value>{formatCount(newSinceLastRun)}</Value>
                </Typography.Text>{' '}
                new {newSinceLastRun === 1 ? 'release' : 'releases'} in the last sync
              </Typography.Text>
              {errors.length > 0 && (
                <Typography.Text type="danger" style={{ fontSize: 13 }}>
                  {errors.length} source{errors.length === 1 ? '' : 's'} failed on the last run
                </Typography.Text>
              )}
            </Space>

            {allRows.length > 5 && (
              <SearchBar
                value={search}
                onChange={setSearch}
                placeholder="Search sources by name, product or repository"
                matched={rows.length}
                total={allRows.length}
                width={300}
              />
            )}

            <Table<DiscoverySourceState>
              size="small"
              pagination={false}
              dataSource={rows}
              rowKey={(s) => `${s.product}-${s.source}`}
              scroll={{ x: 900 }}
              columns={[
                {
                  title: 'Source',
                  width: 150,
                  render: (_, s) => (
                    <Space direction="vertical" size={0}>
                      <Typography.Text strong>{s.source}</Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                        {s.product}
                      </Typography.Text>
                    </Space>
                  ),
                },
                { title: 'Status', width: 340, render: (_, s) => <SourceProgress s={s} /> },
                {
                  // While scanning, what has been CHECKED - which moves - not
                  // what has been "resolved", which was reported in one step
                  // at the end of the phase and read as nothing happening.
                  title: 'Packages seen',
                  width: 110,
                  align: 'right',
                  render: (_, s) => (
                    <Value>{formatCount(s.scanning ? s.tagsChecked : s.lastTagsListed)}</Value>
                  ),
                },
                {
                  title: 'New releases',
                  width: 110,
                  align: 'right',
                  render: (_, s) => {
                    const n = s.scanning ? s.newPackages : s.lastNewPackages
                    return n ? <Tag color="blue">{n}</Tag> : <Value>{formatCount(n)}</Value>
                  },
                },
                {
                  title: 'Time',
                  width: 90,
                  align: 'right',
                  render: (_, s) => (
                    <Value>
                      {formatDuration(
                        s.scanning
                          ? (s.elapsedMs ?? 0) / 1000
                          : s.lastDurationMs
                            ? s.lastDurationMs / 1000
                            : null,
                      )}
                    </Value>
                  ),
                },
                {
                  title: 'Last run',
                  width: 110,
                  render: (_, s) =>
                    s.lastRunAt ? <TimeAgo at={s.lastRunAt} /> : (
                      <NA reason="This source has not completed a scan since the Coordinator started." />
                    ),
                },
                {
                  // Per SOURCE, where the reader is already looking at that
                  // source's last result. Scanning the whole estate to refresh
                  // one registry is minutes of work nobody asked for.
                  title: 'Actions',
                  width: 120,
                  render: (_, s) => <DiscoverSource product={s.product} scanning={s.scanning} />,
                },
              ]}
            />
          </>
        )}
      </Card>

    </>
  )
}
