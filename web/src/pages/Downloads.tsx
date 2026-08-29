import { Alert, Button, Card, Col, Row, Space, Table, Tabs, Tag, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import {
  ArrowRightOutlined, BookOutlined, CloudDownloadOutlined, HistoryOutlined,
  RocketOutlined, ThunderboltOutlined,
} from '../icons'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import {
  useDownloadsForAll, useProducts, useReplicationForAll, useRulesForAll, useTransfers, useWorkers,
} from '../api/queries'
import { isLive, isPromotion, repositoryOf, transferVersion } from '../domain/derive'
import { describeFleet, holdOn, summariseFleet, type Fleet } from '../domain/fleet'
import { bytes, elapsedSeconds, formatBytes, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { ManagedInGit, TimeAgo, TransferStateTag } from '../components/chips'
import { EmptyStateCard, ErrorState, SearchBar } from '../components/layout'
import { DownloadProgress } from '../components/progress'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { c, mono, StatusPill } from '../uikit'
import type { AutoDownloadRuleView, DownloadView, ReplicationView, Transfer } from '../api/types'

/**
 * Page 6 - Downloads.
 *
 * Answers: what is downloading right now, what has finished, and what comes in
 * automatically?
 *
 * # This page starts nothing
 *
 * A download is started from the package being downloaded, where the reader
 * can see what they are asking for. Here they are watching, and a page that
 * both watches and acts had a product selector at the top that scoped only
 * half of it - the ongoing list is estate-wide, and a selector above it
 * implied otherwise.
 *
 * # The distinction the configuration half is built on
 *
 * A DOWNLOAD is what happens: where software goes, in what order, and what has
 * to verify on the way. It carries no pattern.
 *
 * An AUTO-DOWNLOAD RULE is when it happens by itself: a version pattern and
 * the name of the download it triggers. It performs nothing of its own.
 *
 * A rule fires the same download a person runs by hand. If this page makes
 * that look like two mechanisms, it has failed.
 *
 * # And what this page cannot do
 *
 * Change configuration. Git owns it and Flux reconciles it, so a write here
 * would be reverted minutes later with nothing in any log to explain it
 * (docs/design/19 §4). There is no enable/disable toggle on a rule, because
 * there is nothing behind one.
 */

function chainFlow(chain: string[] | undefined) {
  if (!chain?.length) return <NA reason="This download names no targets, so there is no chain to resolve." />
  return (
    <Space size={4} wrap>
      {chain.map((step, i) => (
        <span key={`${step}-${i}`}>
          {i > 0 && <ArrowRightOutlined style={{ fontSize: 10, color: c.text3, margin: '0 4px' }} />}
          <Tag style={{ marginInlineEnd: 0 }}>{step}</Tag>
        </span>
      ))}
    </Space>
  )
}

/**
 * WHICH RELEASE this is, ellipsised, with the full path on the tooltip.
 *
 * `packageName` and not the transfer's origin. They are the same row for a
 * download and completely different for a promotion, whose origin is a target
 * - so this column used to name a promoted release after the lab it was being
 * promoted out of. A release is called what the vendor published it as,
 * whatever it has been copied to since.
 *
 * The origin has not been lost: it is the left half of the Route column, which
 * is where it belongs.
 */
function PackageCell({ transfer, width = 200 }: { transfer: Transfer; width?: number }) {
  const name = transfer.packageName || repositoryOf(transfer.source)
  if (!name) return <NA />
  return (
    <Tooltip title={name}>
      <Typography.Text
        style={{
          display: 'block',
          minWidth: 0,
          maxWidth: width,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          fontFamily: mono,
          fontSize: 12,
        }}
        ellipsis={{ tooltip: false }}
      >
        {name}
      </Typography.Text>
    </Tooltip>
  )
}

/**
 * Where a transfer went, as one cell.
 *
 * On EVERY transfer table, not only promotions. A download's origin is the
 * vendor and its destination is whichever target the chain named, and "which
 * of my three targets did this actually land in" is a question the listing
 * could not answer at all - the reader had to open the download to find out.
 *
 * The configured NAMES rather than the resolved host and path: `lab →
 * production` is what somebody asked for and what they will say out loud, and
 * a column of `acme.jfrog.io/nokia-lab/cmm → acme.jfrog.io/nokia-prod/cmm` is
 * two hundred characters of which four differ. The full coordinates are on the
 * tooltip and on the transfer's own page.
 */
function Route({ transfer }: { transfer: Transfer }) {
  const from = (transfer.sourceName || repositoryOf(transfer.source)).split('/')[0]
  const to = transfer.targetName || repositoryOf(transfer.target)
  if (!from && !to) return <NA />
  return (
    <Tooltip title={`${transfer.source} → ${transfer.target}`}>
      <Space size={4}>
        <Typography.Text>{from}</Typography.Text>
        <ArrowRightOutlined style={{ fontSize: 10, color: c.text3 }} />
        <Typography.Text strong>{to}</Typography.Text>
      </Space>
    </Tooltip>
  )
}

/**
 * Whether the registry relocated it or our workers copied it.
 *
 * Colour on the fast one only. Both are correct, so a copy is not a warning -
 * painting it amber would teach readers to distrust a perfectly good
 * promotion.
 */
function MethodTag({ transfer }: { transfer: Transfer }) {
  if (transfer.strategy === 'relocate') {
    return (
      <Tooltip title="The registry moved it between two of its own repositories. No bytes crossed the wire.">
        <StatusPill tone="ok" dot={false} style={{ marginInlineEnd: 0 }}>Relocate</StatusPill>
      </Tooltip>
    )
  }
  return (
    <Tooltip title="Our workers read from one target and wrote to the other.">
      <Tag style={{ marginInlineEnd: 0 }}>Copy</Tag>
    </Tooltip>
  )
}

/**
 * A transfer's state, with what is holding it up when something is.
 *
 * # Why the state alone was not enough
 *
 * `RUNNING` is set the moment a transfer's first job completes and stays set
 * while nothing at all is in flight - which is exactly the state somebody scans
 * this table to find. `READY` is honest and its tooltip promises a worker is
 * about to take the first job, which is a promise nobody can keep when there is
 * no worker.
 *
 * The server's word is kept, because it is what `transferctl` and the logs say
 * and the two must line up. What is added is the second half of the sentence:
 * what would have to change for this row to move.
 */
function TransferState({ transfer, fleet }: { transfer: Transfer; fleet: Fleet }) {
  const hold = holdOn(transfer, fleet)
  return (
    <Space direction="vertical" size={2}>
      <TransferStateTag state={transfer.state} />
      {hold && (
        <Tooltip title={`${hold.detail} ${describeFleet(fleet)}`}>
          <Typography.Text
            style={{
              fontSize: 11,
              // Amber for the one that needs somebody, grey for the ones that
              // are the queue working as designed. A reader scanning the column
              // should not have to read every row to find the one that matters.
              color: hold.actionable ? c.danger : c.text3,
              whiteSpace: 'nowrap',
            }}
          >
            {hold.kind === 'no-workers' ? 'No worker running' : hold.label}
          </Typography.Text>
        </Tooltip>
      )}
    </Space>
  )
}

/** The one-line version of a hold, for a cell that has no room for a sentence. */
function holdLine(transfer: Transfer, fleet: Fleet): string | undefined {
  const hold = holdOn(transfer, fleet)
  if (!hold) return undefined
  return hold.kind === 'no-workers' ? 'no worker to run it' : 'waiting for a worker'
}

/** One product's row in a configuration table. */
type WithProduct<T> = T & { product: string }

function searchable<T>(rows: T[], query: string, text: (row: T) => string) {
  const needle = query.trim().toLowerCase()
  if (!needle) return rows
  return rows.filter((row) => text(row).toLowerCase().includes(needle))
}

function TableSearch({ value, onChange, placeholder = 'Search by product or package', style }: {
  value: string
  onChange: (value: string) => void
  placeholder?: string
  style?: React.CSSProperties
}) {
  return <SearchBar value={value} onChange={onChange} placeholder={placeholder} width={280} style={{ marginBottom: 0, ...style }} />
}

function TableToolbar({ children }: { children: React.ReactNode }) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: 12,
        padding: '16px 12px 12px',
        flexWrap: 'wrap',
      }}
    >
      {children}
    </div>
  )
}

export default function Downloads() {
  const [transferPage, setTransferPage] = useState(1)
  const [transferToken, setTransferToken] = useState<string>()
  const [promotionPage, setPromotionPage] = useState(1)
  const [promotionToken, setPromotionToken] = useState<string>()
  const [ongoingSearch, setOngoingSearch] = useState('')
  const [downloadSearch, setDownloadSearch] = useState('')
  const [promotionSearch, setPromotionSearch] = useState('')
  const [rulesSearch, setRulesSearch] = useState('')
  const [autoDownloadSearch, setAutoDownloadSearch] = useState('')
  const products = useProducts()
  const productList = (products.data?.products ?? []).filter((p) => p.enabled)
  const names = productList.map((p) => p.productId)

  // TWO QUERIES, not one listing split in the browser.
  //
  // A busy estate's hundred most recent transfers are all downloads, so a
  // client-side split would leave the promotions table empty on exactly the
  // deployments that promote the most.
  const transfers = useTransfers({ pageSize: 25, operation: 'replicate', pageToken: transferToken })
  const promotionsQuery = useTransfers({ pageSize: 25, operation: 'promote', pageToken: promotionToken })
  const downloadsPerProduct = useDownloadsForAll(names)
  const rulesPerProduct = useRulesForAll(names)
  const replicationPerProduct = useReplicationForAll(names)

  /*
    THE FLEET. A download is planned by the Coordinator and performed by
    workers, and this page could not see them - so a queue that nothing was
    draining rendered identically to one being drained at full tilt, which is
    the difference between "wait" and "go and start a worker".
  */
  const workers = useWorkers()
  const fleet = summariseFleet(workers.data?.workers, workers.isSuccess)

  const all = transfers.data?.transfers ?? []
  const ongoing = all.filter((t) => isLive(t.state) || t.state === 'PAUSED')
  const finished = all.filter((t) => !isLive(t.state) && t.state !== 'PAUSED')

  // Downloads that have been asked for and cannot move because there is
  // nothing to move them. Distinct from ones merely queued behind other work,
  // which is the system working.
  const stranded = ongoing.filter((t) => holdOn(t, fleet)?.kind === 'no-workers')

  // Belt and braces on the filter: a Coordinator too old to know the
  // `operation` parameter answers with everything, and a promotion listed
  // among downloads reads as a download that mysteriously moved no bytes.
  const promotions = (promotionsQuery.data?.transfers ?? []).filter(isPromotion)
  const activePromotions = promotions.filter((t) => isLive(t.state) || t.state === 'PAUSED')
  const visibleOngoing = searchable(ongoing, ongoingSearch, (t) => `${t.product} ${t.packageName} ${t.tag} ${t.source} ${t.target}`)
  const visibleFinished = searchable(finished, downloadSearch, (t) => `${t.product} ${t.packageName} ${t.tag} ${t.source} ${t.target}`)
  const visiblePromotions = searchable(promotions, promotionSearch, (t) => `${t.product} ${t.packageName} ${t.tag} ${t.source} ${t.target}`)
  // Flattened with the product each row belongs to, since the tables now cover
  // the estate rather than one product at a time.
  const downloads: WithProduct<DownloadView>[] = downloadsPerProduct.flatMap((q, i) =>
    (q.data?.downloads ?? []).map((d) => ({ ...d, product: names[i]! })))
  const rules: AutoDownloadRuleView[] = rulesPerProduct.flatMap((q) => q.data?.rules ?? [])
  const visibleRules = searchable(downloads, rulesSearch, (d) => `${d.product} ${d.name} ${d.chain?.join(' ')}`)
  const visibleAutoDownloads = searchable(rules, autoDownloadSearch, (r) => `${r.product} ${r.name} ${r.tagPattern} ${r.download}`)
  const drifted: ReplicationView[] = replicationPerProduct.flatMap(
    (q) => (q.data?.replication ?? []).filter((r) => r.drift?.drifted))

  if (products.isError) {
    return (
      <>
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>

      {stranded.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message={
            `${stranded.length} download${stranded.length === 1 ? '' : 's'} `
            + `${stranded.length === 1 ? 'is' : 'are'} waiting, and no worker is running`
          }
          description={
            <Space direction="vertical" size={4}>
              <Typography.Text>{describeFleet(fleet)}</Typography.Text>
              <Typography.Text type="secondary">
                Downloads are planned here and performed by workers, so these will not move -
                and will not fail either - until at least one is back. Nothing has been lost:
                work already planned stays queued and starts on its own the moment a worker
                reports in.
              </Typography.Text>
            </Space>
          }
        />
      )}

      {drifted.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="A registry's configuration has drifted from what Git says"
          description={
            <Space direction="vertical" size={4}>
              {drifted.map((d) => (
                <Typography.Text key={`${d.product}-${d.target}`}>
                  <strong>{d.product} · {d.target}</strong>:{' '}
                  {d.drift?.fields?.map((f) => `${f.field} is ${f.observed}, Git says ${f.desired}`).join('; ')
                    || d.drift?.reason}
                </Typography.Text>
              ))}
              <Typography.Text type="secondary">
                Applying pushes what Git already says into the registry. It changes nothing in Git.
              </Typography.Text>
            </Space>
          }
        />
      )}

      <Row gutter={[16, 16]}>
        <Col span={24}>
          <Tabs
          items={[
            {
              key: 'ongoing',
              label: `Ongoing download${ongoing.length ? ` (${ongoing.length})` : ''}`,
              icon: <CloudDownloadOutlined />,
              children: (
          <Card
            loading={transfers.isLoading}
            styles={{ body: { padding: 0 } }}
          >
            <TableToolbar>
              <TableSearch value={ongoingSearch} onChange={setOngoingSearch} />
            </TableToolbar>
            {!transfers.isLoading && ongoing.length === 0 ? (
              <EmptyStateCard
                title="Nothing is downloading"
                explanation="Downloads started manually or by an auto-download rule appear here while they run. Finished ones are listed below."
                action={<Link to="/packages"><Button type="primary">Find a package to download</Button></Link>}
              />
            ) : (
              <DataTable<Transfer>
                tableEnhancedKey="downloads-ongoing"
                size="small"
                pagination={{
                  current: transferPage,
                  pageSize: 25,
                  total: transfers.data?.nextPageToken ? transferPage * 25 + 1 : transferPage * 25,
                  showSizeChanger: false,
                  onChange: (page) => {
                    if (page > transferPage && transfers.data?.nextPageToken) {
                      setTransferToken(transfers.data.nextPageToken)
                      setTransferPage(page)
                    }
                  },
                }}
                dataSource={visibleOngoing}
                rowKey={(t) => t.id}
                scroll={{ x: 1380 }}
                columns={[
                  { title: 'Product', width: 140, render: (_, t) => t.product },
                  { title: 'Package', width: 190, render: (_, t) => <PackageCell transfer={t} /> },
                  {
                    title: 'Version',
                    width: 150,
                    render: (_, t) => <Typography.Text style={{ fontFamily: mono }}>{transferVersion(t)}</Typography.Text>,
                  },
                  { title: 'Route', width: 200, render: (_, t) => <Route transfer={t} /> },
                  {
                    title: 'State',
                    width: 140,
                    render: (_, t) => <TransferState transfer={t} fleet={fleet} />,
                  },
                  {
                    // How far, how long, and how much longer - one cell,
                    // because each of the three is misleading without the
                    // other two.
                    title: 'Progress',
                    width: 220,
                    render: (_, t) => (
                      <DownloadProgress
                        // Distinct content, the same axis the download page
                        // draws: bytes weighed once however many repositories
                        // they reach, because the second copy is a mount.
                        transferred={bytes(t.progress?.contentMovedBytes)
                          ?? bytes(t.progress?.bytesTransferred)}
                        total={bytes(t.progress?.contentBytes)
                          ?? bytes(t.progress?.plannedBytes)}
                        saved={bytes(t.progress?.contentPresentBytes)
                          ?? bytes(t.progress?.savedBytes)}
                        groups={t.content}
                        strategy={t.strategy ?? 'copy'}
                        elapsedSeconds={elapsedSeconds(t.startedAt)}
                        live={isLive(t.state)}
                        // No arrival to promise while nothing is moving it.
                        heldBy={holdLine(t, fleet)}
                      />
                    ),
                  },
                  { title: 'Priority', width: 90, align: 'right', render: (_, t) => <PriorityControl transfer={t} /> },
                  {
                    title: 'Actions',
                    width: 290,
                    fixed: 'right',
                    render: (_, t) => (
                      <Space size={4}>
                        <Link to={`/downloads/${t.id}`}>
                          <Button size="small" type="primary">View download</Button>
                        </Link>
                        <QueueControls transfer={t} />
                      </Space>
                    ),
                  },
                ]}
              />
            )}
          </Card>
              ),
            },
            {
                key: 'downloads',
                label: 'Downloads',
              icon: <HistoryOutlined />,
              children: (
              <Card loading={transfers.isLoading} styles={{ body: { padding: 0 } }}>
            <TableToolbar>
              <TableSearch value={downloadSearch} onChange={setDownloadSearch} />
            </TableToolbar>
            {!transfers.isLoading && finished.length === 0 ? (
              <EmptyStateCard
                title="No download has finished yet"
                explanation="Once a download completes it is listed here with what it cost and how long it took."
                action={<Link to="/packages"><Button>Find a package to download</Button></Link>}
              />
            ) : (
              <DataTable<Transfer>
                tableEnhancedKey="downloads-finished"
                allow_export
                size="small"
                pagination={{
                  current: transferPage,
                  pageSize: 25,
                  total: transfers.data?.nextPageToken ? transferPage * 25 + 1 : transferPage * 25,
                  showSizeChanger: false,
                  onChange: (page) => {
                    if (page > transferPage && transfers.data?.nextPageToken) {
                      setTransferToken(transfers.data.nextPageToken)
                      setTransferPage(page)
                    }
                  },
                }}
                dataSource={visibleFinished}
                rowKey={(t) => t.id}
                scroll={{ x: 1280 }}
                columns={[
                  { title: 'Product', width: 140, render: (_, t) => t.product },
                  { title: 'Package', width: 190, render: (_, t) => <PackageCell transfer={t} /> },
                  {
                    title: 'Version',
                    width: 150,
                    render: (_, t) => <Typography.Text style={{ fontFamily: mono }}>{transferVersion(t)}</Typography.Text>,
                  },
                  { title: 'Route', width: 200, render: (_, t) => <Route transfer={t} /> },
                  { title: 'State', width: 130, render: (_, t) => <TransferStateTag state={t.state} /> },
                  {
                    title: 'Time',
                    width: 90,
                    align: 'right',
                    render: (_, t) => (
                      <Value>{formatDuration(elapsedSeconds(t.startedAt, t.completedAt))}</Value>
                    ),
                  },
                  {
                    title: 'Downloaded',
                    width: 110,
                    align: 'right',
                    render: (_, t) => <Value>{formatBytes(bytes(t.progress?.bytesTransferred))}</Value>,
                  },
                  { title: 'When', width: 100, render: (_, t) => <TimeAgo at={t.completedAt || t.createdAt} /> },
                  {
                    title: 'Actions',
                    width: 250,
                    fixed: 'right',
                    render: (_, t) => (
                      <Space size={4}>
                        <Link to={`/downloads/${t.id}`}>
                          <Button size="small" type="primary">View download</Button>
                        </Link>
                        <QueueControls transfer={t} />
                      </Space>
                    ),
                  },
                ]}
              />
            )}
          </Card>
              ),
            },
            {
                key: 'promotion',
                label: `Promotion${activePromotions.length ? ` (${activePromotions.length})` : ''}`,
              icon: <RocketOutlined />,
              children: (
          <Card
            loading={promotionsQuery.isLoading}
            styles={{ body: { padding: 0 } }}
          >
            <TableToolbar>
              <TableSearch value={promotionSearch} onChange={setPromotionSearch} />
            </TableToolbar>
            {!promotionsQuery.isLoading && promotions.length === 0 ? (
              <EmptyStateCard
                title="Nothing has been promoted"
                explanation="A downloaded release can be promoted from its own page, or from the row on the packages listing. Promotions appear here with where they went and how."
                action={<Link to="/packages"><Button>Find a release to promote</Button></Link>}
              />
            ) : (
              <DataTable<Transfer>
                tableEnhancedKey="downloads-promotions"
                allow_export
                size="small"
                pagination={{
                  current: promotionPage,
                  pageSize: 25,
                  total: promotionsQuery.data?.nextPageToken ? promotionPage * 25 + 1 : promotionPage * 25,
                  showSizeChanger: false,
                  onChange: (page) => {
                    if (page > promotionPage && promotionsQuery.data?.nextPageToken) {
                      setPromotionToken(promotionsQuery.data.nextPageToken)
                      setPromotionPage(page)
                    }
                  },
                }}
                dataSource={visiblePromotions}
                rowKey={(t) => t.id}
                scroll={{ x: 1100 }}
                columns={[
                  { title: 'Product', width: 140, render: (_, t) => t.product },
                  { title: 'Package', width: 180, render: (_, t) => <PackageCell transfer={t} /> },
                  {
                    title: 'Version',
                    width: 150,
                    render: (_, t) => (
                      <Typography.Text style={{ fontFamily: mono }}>{transferVersion(t)}</Typography.Text>
                    ),
                  },
                  // THE ROUTE, which is what a promotion IS. The listing above
                  // has no such column because a download's origin is always
                  // the vendor.
                  { title: 'Route', width: 220, render: (_, t) => <Route transfer={t} /> },
                  { title: 'How', width: 110, render: (_, t) => <MethodTag transfer={t} /> },
                  { title: 'State', width: 130, render: (_, t) => <TransferStateTag state={t.state} /> },
                  {
                    title: 'Time',
                    width: 90,
                    align: 'right',
                    render: (_, t) => (
                      <Value>{formatDuration(elapsedSeconds(t.startedAt, t.completedAt))}</Value>
                    ),
                  },
                  { title: 'When', width: 100, render: (_, t) => <TimeAgo at={t.completedAt || t.createdAt} /> },
                  {
                    title: 'Actions',
                    width: 250,
                    fixed: 'right',
                    render: (_, t) => (
                      <Space size={4}>
                        <Link to={`/downloads/${t.id}`}>
                          <Button size="small" type="primary">View promotion</Button>
                        </Link>
                        <QueueControls transfer={t} />
                      </Space>
                    ),
                  },
                ]}
              />
            )}
          </Card>
              ),
            },
            {
              key: 'rules',
              label: 'Rules',
              icon: <BookOutlined />,
              children: (
          <Card loading={downloadsPerProduct.some((q) => q.isLoading)} styles={{ body: { padding: 0 } }}>
            <TableToolbar>
              <TableSearch value={rulesSearch} onChange={setRulesSearch} />
              <ManagedInGit />
            </TableToolbar>
            <DataTable<WithProduct<DownloadView>>
              tableEnhancedKey="downloads-by-product"
              size="small"
              pagination={false}
              dataSource={visibleRules}
              rowKey={(d) => `${d.product}-${d.name || 'default'}`}
              scroll={{ x: 900 }}
              columns={[
                { title: 'Product', width: 150, render: (_, d) => d.product },
                {
                  title: 'Download',
                  width: 140,
                  render: (_, d) => d.name || <Typography.Text type="secondary">(default)</Typography.Text>,
                },
                {
                  title: 'Chain',
                  render: (_, d) => (d.chainError
                    ? <Typography.Text type="danger">{d.chainError}</Typography.Text>
                    : chainFlow(d.chain)),
                },
                {
                  title: 'Verification gates',
                  width: 250,
                  render: (_, d) => (
                    <Space size={4} wrap>
                      <Tooltip title="Whether the vendor signature must verify before the software is brought in.">
                        <Tag color={d.verifyBefore === 'true' ? 'green' : undefined}>
                          Before: {d.verifyBefore ?? 'inherit'}
                        </Tag>
                      </Tooltip>
                      <Tooltip title="Whether the signature must verify at the destination before the next step runs.">
                        <Tag color={d.verifyAfter === 'true' ? 'green' : undefined}>
                          After: {d.verifyAfter ?? 'inherit'}
                        </Tag>
                      </Tooltip>
                    </Space>
                  ),
                },
                { title: 'Priority', width: 90, align: 'right', render: (_, d) => d.priority },
              ]}
            />
          </Card>
              ),
            },
            {
              key: 'auto-download',
              label: 'Auto download',
              icon: <ThunderboltOutlined />,
              children: (
          <Card loading={rulesPerProduct.some((q) => q.isLoading)} styles={{ body: { padding: 0 } }}>
            <TableToolbar>
              <TableSearch value={autoDownloadSearch} onChange={setAutoDownloadSearch} />
              <ManagedInGit />
            </TableToolbar>

            {!rulesPerProduct.some((q) => q.isLoading) && rules.length === 0 ? (
              <EmptyStateCard
                title="No auto-download rules"
                explanation="Nothing is downloaded automatically. Rules are defined in Git; adding one there will show it here."
                action={<Link to="/packages"><Button>Download a package by hand</Button></Link>}
              />
            ) : (
              <Table<AutoDownloadRuleView>
                size="small"
                pagination={false}
                dataSource={visibleAutoDownloads}
                rowKey={(r) => `${r.product}-${r.name}`}
                scroll={{ x: 800 }}
                columns={[
                  { title: 'Product', width: 150, render: (_, r) => r.product },
                  { title: 'Rule', width: 150, render: (_, r) => r.name },
                  {
                    title: 'Matches versions',
                    width: 190,
                    render: (_, r) => <span style={{ fontFamily: mono, fontSize: 12 }}>{r.tagPattern}</span>,
                  },
                  {
                    title: 'Triggers',
                    width: 150,
                    render: (_, r) => r.download || <Typography.Text type="secondary">(default)</Typography.Text>,
                  },
                  {
                    title: 'State',
                    width: 130,
                    render: (_, r) => r.enabled ? <StatusPill tone="ok">Enabled</StatusPill> : (
                      <Tooltip title="A rule is turned off in Git and nowhere else. There is no runtime override, so there is no toggle here.">
                        <StatusPill tone="neutral">Disabled</StatusPill>
                      </Tooltip>
                    ),
                  },
                ]}
              />
            )}
          </Card>
              ),
            },
          ].sort((a, b) => {
            const order = { ongoing: 0, promotion: 1, downloads: 2, rules: 3, 'auto-download': 4 }
            return order[a.key as keyof typeof order] - order[b.key as keyof typeof order]
          })}
          />
        </Col>

      </Row>
    </>
  )
}
