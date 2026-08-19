import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import type { UseMutationResult } from '@tanstack/react-query'
import {
  Alert, App, Button, Card, Col, Descriptions, Empty, Modal, Row, Space, Table, Tabs, Tooltip,
  Tree, Typography,
} from 'antd'
import { CloudDownloadOutlined, FolderOutlined } from '@ant-design/icons'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  useArtifacts, useInspectPackage, usePackage, usePackageFiles, useProduct, useRunDownload,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import {
  deriveLocations, deriveStatus, downloadedAt, isLive, matches, verification, version,
} from '../domain/derive'
import { bytes, formatBytes, formatCount, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { AnalyzeIcon, ARTIFACT_ICONS, Icon } from '../components/icons'
import { WorkingBar } from '../components/progress'
import {
  LocationChip, RepoLink, StatusBadge, TimeAgo, VerificationBadge,
} from '../components/chips'
import {
  EmptyStateCard, ErrorState, PageHeader, ReleaseTimeline, SavedPanel, SearchBar,
} from '../components/layout'
import { mono } from '../theme'
import type { Artifact, InspectPackageResponse, PackageFile } from '../api/types'
import { TargetTag } from '../components/chips'

/**
 * Page 3 — Package (release detail).
 *
 * Answers: everything about this one release, and what can I do with it?
 *
 * Two rules bite hardest here:
 *   - the release downloads WHOLE. The contents are shown and nothing in them
 *     is selectable, because cherry-picking a release is not a thing the
 *     system does.
 *   - "Saved (already present)" lives HERE, not on Home, and is labelled an
 *     estimate before a download and a measurement after.
 */

/**
 * Analysing a release, with something to look at while it happens.
 *
 * # Why this is a panel and not a button
 *
 * Analysing walks the release's whole manifest tree against the vendor
 * registry — for the 260-artifact releases this system is built for, that is
 * hundreds of round trips and takes a while. The button reported none of it:
 * it spun in place and the page changed silently, which is indistinguishable
 * from nothing happening.
 *
 * # Why there is no percentage
 *
 * The API has no progress feed for this, and it cannot sensibly have one: the
 * size of the tree is the thing being discovered, so there is no denominator
 * until the work is finished. <WorkingBar> is the honest shape — it shows that
 * work is running and how long it has been running, and states no position.
 *
 * What it CAN report is the outcome, and that is worth reporting in full: a
 * measurement that fetched nothing because the tree was already recorded is a
 * different event from one that read three hundred manifests, and both look
 * identical if all you do is refresh the page.
 */
function MeasurePanel({
  analysed, artifactCount, inspect, disabled,
}: {
  analysed: boolean
  artifactCount?: number
  inspect: UseMutationResult<InspectPackageResponse, Error, void, unknown>
  disabled: boolean
}) {
  const [startedAt, setStartedAt] = useState<number>()
  const [elapsed, setElapsed] = useState(0)

  useEffect(() => {
    if (!inspect.isPending || !startedAt) return
    const t = setInterval(() => setElapsed((Date.now() - startedAt) / 1000), 200)
    return () => clearInterval(t)
  }, [inspect.isPending, startedAt])

  const run = () => {
    setStartedAt(Date.now())
    setElapsed(0)
    inspect.mutate()
  }

  if (inspect.isPending) {
    // Measured against a registry that could not be reached: the call runs for
    // at least ninety seconds before anything comes back. A bar that says
    // nothing after the first minute is indistinguishable from a hang, so the
    // wording escalates rather than repeating itself.
    const scope = artifactCount
      ? `Reading the manifest tree from the vendor registry — ${artifactCount} artifacts to walk.`
      : 'Reading the manifest tree from the vendor registry.'
    const detail =
      elapsed > 45
        ? `${scope} Still going. A release this size can take several minutes, and an unreachable registry looks the same from here until it times out. Leaving this page cancels the measurement.`
        : `${scope} Large releases take a minute or two.`

    return (
      <div style={{ marginTop: 12 }}>
        <WorkingBar label="Measuring this release" elapsedSeconds={elapsed} detail={detail} />
      </div>
    )
  }

  if (inspect.isError) {
    return (
      <Alert
        style={{ marginTop: 12 }}
        type="error"
        showIcon
        message="This package could not be analyzed"
        description={
          <Space direction="vertical" size={4}>
            <Typography.Text>
              {inspect.error instanceof Error ? inspect.error.message : 'The registry did not answer.'}
            </Typography.Text>
            <Typography.Text type="secondary">
              Nothing was changed. The release can still be downloaded; its size is established
              as the download runs.
            </Typography.Text>
          </Space>
        }
        action={<Button size="small" onClick={run}>Try again</Button>}
      />
    )
  }

  if (inspect.isSuccess) {
    const r = inspect.data
    return (
      <Alert
        style={{ marginTop: 12 }}
        type="success"
        showIcon
        message={
          r.alreadyExpanded
            ? 'Already analyzed — the vendor registry was not contacted'
            : `Analyzed in ${formatDuration(elapsed) ?? 'a moment'}`
        }
        description={
          <Space size={16} wrap>
            <Typography.Text type="secondary">
              <Value>{formatCount(r.artifacts)}</Value> artifacts
            </Typography.Text>
            <Typography.Text type="secondary">
              <Value>{formatCount(r.blobs)}</Value> blobs
            </Typography.Text>
            <Typography.Text type="secondary">
              <Value>{formatBytes(r.totalBytes)}</Value> total
            </Typography.Text>
            {!r.alreadyExpanded && (
              <Typography.Text type="secondary">
                <Value>{formatCount(r.fetched)}</Value> manifests read
              </Typography.Text>
            )}
            {Boolean(r.signatureResolved) && (
              <Typography.Text type="secondary">
                <Value>{formatCount(r.signatureResolved)}</Value> signatures recorded
              </Typography.Text>
            )}
          </Space>
        }
      />
    )
  }

  if (analysed) return null

  return (
    <Space direction="vertical" size={4} style={{ marginTop: 12 }}>
      <Button
        size="small"
        icon={<Icon as={AnalyzeIcon} title="Analyze" />}
        disabled={disabled}
        onClick={run}
      >
        Analyze package
      </Button>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        Walks the manifest tree in the vendor registry to establish what this release contains —
        its files, and what each part weighs. Nothing is downloaded.
      </Typography.Text>
    </Space>
  )
}

/**
 * The three kinds a user thinks in, from the kind the API derived.
 *
 * # Why this no longer guesses
 *
 * It used to read the media type and call anything image-ish an image. Every
 * part of a vendor orb is served as `image.manifest.v1+json` with no
 * artifactType, so every chart was counted as an image and Helm Charts read
 * zero on every release. The API now classifies — using the vendor's own
 * annotations where the OCI fields cannot answer — and this only maps its
 * bounded vocabulary onto the three tiles.
 *
 * Signatures and the index itself are not release contents and are excluded
 * rather than swept into Files.
 */
function tileFor(a: Artifact): 'Images' | 'Helm Charts' | 'Files' | null {
  switch (a.kind) {
    case 'image': return 'Images'
    case 'chart': return 'Helm Charts'
    case 'file': return 'Files'
    case 'index':
    case 'signature':
      return null
    default:
      // `artifact` — content that declared nothing about itself — and any
      // kind this client has not been taught. Counted as Files rather than
      // silently dropped, since it IS part of the release.
      return a.kind ? 'Files' : null
  }
}

/**
 * The name a person recognises.
 *
 * The vendor's ref name is the only thing here that reads like a name —
 * `cfx-5000-product/bgcf:2511.174.0`. Without it the column showed the
 * artifact TYPE, so every row of the images tab said "Images".
 */
function artifactName(a: Artifact): string | null {
  const ref = a.annotations?.['org.opencontainers.image.ref.name']
  if (ref) return ref
  const title = a.annotations?.['org.opencontainers.image.title']
  if (title) return title
  return null
}

/**
 * A component's name and its tag, apart.
 *
 * The vendor writes them as one string — `cfx-5000-product/bgcf:2511.174.0` —
 * and they are two facts: what the component IS and which version of it this
 * release carries. In one column the version is the part that falls off the
 * end of the cell, and it is the part somebody is looking for.
 *
 * The colon must come after the last slash to be a tag separator: a registry
 * host may carry a port, and `near.example.com:5000/orbs/x` is a path with no
 * tag in it at all.
 */
function splitRef(ref: string | null): { name: string | null; tag: string | null } {
  if (!ref) return { name: null, tag: null }
  const colon = ref.lastIndexOf(':')
  if (colon < 0 || colon < ref.lastIndexOf('/')) return { name: ref, tag: null }
  return { name: ref.slice(0, colon), tag: ref.slice(colon + 1) }
}


/**
 * One kind of component, with a search over it.
 *
 * The Files tab had a search and the other two did not, which is the wrong way
 * round if anything: a release carries a hundred and sixty images and two file
 * bundles, and the hundred and sixty are the ones nobody can scan by eye.
 */
function ComponentTable({ artifacts, kind }: { artifacts: Artifact[]; kind: string }) {
  const [search, setSearch] = useState('')

  const rows = useMemo(() => {
    if (!search.trim()) return artifacts
    return artifacts.filter((a) => {
      const { name, tag } = splitRef(artifactName(a))
      return matches(search, name ?? undefined, tag ?? undefined, a.digest)
    })
  }, [artifacts, search])

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <SearchBar
        value={search}
        onChange={setSearch}
        placeholder={`Search ${kind.toLowerCase()} by name, tag or digest`}
        matched={rows.length}
        total={artifacts.length}
        width={320}
      />

      <Table<Artifact>
        size="small"
        dataSource={rows}
        rowKey={(a) => a.artifactId}
        pagination={{ pageSize: 10, hideOnSinglePage: true }}
        scroll={{ x: 640 }}
        columns={[
          {
            title: 'Name',
            render: (_, a) => (
              <Value reason="This artifact carries no name annotation; it is identified by its digest.">
                {splitRef(artifactName(a)).name}
              </Value>
            ),
          },
          {
            // Its own column. The vendor writes the name and the tag as one
            // string and they are two facts — what the component is, and which
            // version of it this release carries — and in one cell the version
            // is the half that falls off the end.
            title: 'Tag',
            width: 170,
            render: (_, a) => {
              const { tag } = splitRef(artifactName(a))
              return tag
                ? <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{tag}</Typography.Text>
                : <NA reason="This component answers to no tag of its own; it is addressed by digest." />
            },
          },
          {
            title: 'Size',
            width: 110,
            align: 'right',
            render: (_, a) => (
              <Value reason="This manifest has not been walked, so what it holds is not known. Analyse the package to establish it.">
                {formatBytes(bytes(a.contentBytes))}
              </Value>
            ),
          },
          {
            title: 'Digest',
            width: 190,
            render: (_, a) => (
              <Typography.Text copyable={{ text: a.digest }} style={{ fontFamily: mono, fontSize: 11 }}>
                {a.digest.slice(0, 19)}…
              </Typography.Text>
            ),
          },
        ]}
      />
    </Space>
  )
}

/**
 * What is inside a release, as a directory tree.
 *
 * # Why a tree and not a list
 *
 * A vendor's configuration bundle is two hundred paths under a handful of
 * directories, and a flat list of two hundred rows answers "is the network
 * configuration in here" by making somebody read all of them. The paths are
 * already hierarchical — the publisher wrote them that way — so the shape is
 * the vendor's, not one this page invented.
 *
 * # What it can show, and what it cannot
 *
 * Every layer that carries `org.opencontainers.image.title` IS one named file,
 * and analysis recorded that name. A layer with no title is a tar of an unknown
 * number of paths — an image layer — and is not listed at all: a row called
 * `layer sha256:…` would be a summary pretending to be an answer, and counting
 * them at the foot only told a reader about something they had not asked
 * about and could not act on.
 */
function FileTree({ files }: { files: PackageFile[] }) {
  const [search, setSearch] = useState('')

  const shown = useMemo(
    () => (search.trim() ? files.filter((f) => matches(search, f.path, f.component)) : files),
    [files, search])

  const tree = useMemo(() => buildTree(shown), [shown])

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <SearchBar
        value={search}
        onChange={setSearch}
        placeholder="Search files by path or component"
        matched={shown.length}
        total={files.length}
        width={320}
      />

      {shown.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="No file matches that" />
      ) : (
        <Tree
          treeData={tree}
          showLine={{ showLeafIcon: false }}
          // Directories open, files where they are: a tree that starts
          // collapsed makes somebody click three times to see what they came
          // for, and these are shallow.
          defaultExpandAll={shown.length <= 60}
          selectable={false}
          style={{ maxHeight: 420, overflow: 'auto' }}
        />
      )}


    </Space>
  )
}

/** One node of the tree, before Ant Design's shape. */
interface TreeNode {
  key: string
  title: ReactNode
  children?: TreeNode[]
}

/**
 * Paths into a tree, directories before files and both alphabetical.
 *
 * Sizes roll UP: a directory shows what everything under it weighs, which is
 * the number somebody is looking for when they ask what a bundle costs.
 */
function buildTree(files: PackageFile[]): TreeNode[] {
  interface Dir {
    dirs: Map<string, Dir>
    files: PackageFile[]
    bytes: number
  }
  const root: Dir = { dirs: new Map(), files: [], bytes: 0 }

  for (const file of files) {
    const parts = file.path.split('/').filter(Boolean)
    const name = parts.pop() ?? file.path
    let node = root
    node.bytes += bytes(file.sizeBytes) ?? 0
    for (const part of parts) {
      let next = node.dirs.get(part)
      if (!next) {
        next = { dirs: new Map(), files: [], bytes: 0 }
        node.dirs.set(part, next)
      }
      node = next
      node.bytes += bytes(file.sizeBytes) ?? 0
    }
    node.files.push({ ...file, path: name })
  }

  const render = (dir: Dir, prefix: string): TreeNode[] => {
    const out: TreeNode[] = []
    for (const [name, child] of [...dir.dirs.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
      out.push({
        key: `${prefix}/${name}`,
        title: (
          <Space size={6}>
            <FolderOutlined style={{ color: '#98A2B3' }} />
            <Typography.Text style={{ fontSize: 13 }}>{name}</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {formatBytes(child.bytes)}
            </Typography.Text>
          </Space>
        ),
        children: render(child, `${prefix}/${name}`),
      })
    }
    for (const file of [...dir.files].sort((a, b) => a.path.localeCompare(b.path))) {
      out.push({
        key: `${prefix}/${file.path}-${file.digest}`,
        title: (
          <Space size={6}>
            <Icon as={ARTIFACT_ICONS.Files} size={13} title="File" />
            <Typography.Text style={{ fontSize: 13 }}>{file.path}</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {formatBytes(bytes(file.sizeBytes))}
            </Typography.Text>

          </Space>
        ),
      })
    }
    return out
  }

  return render(root, '')
}

export default function PackageDetail() {
  const { product: productName, reference } = useParams()
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const { message } = App.useApp()

  // A version tag is not unique within a product — vendors publish the same
  // tag into every repository — so the repository travels with the link and is
  // what makes this lookup answerable. See releaseHref in domain/derive.
  const repository = params.get('repository') ?? undefined

  const product = useProduct(productName)
  const pkg = usePackage(productName, reference, repository)
  const artifacts = useArtifacts(productName, reference, repository)
  const files = usePackageFiles(productName, reference, repository)
  const inspect = useInspectPackage(productName!, reference!, repository)
  const runDownload = useRunDownload(productName!)

  const mayOperate = useCan('operate', { product: productName })
  const [confirming, setConfirming] = useState(false)

  const groups = useMemo(() => {
    const out = {
      Images: { count: 0, bytes: 0, measured: false },
      'Helm Charts': { count: 0, bytes: 0, measured: false },
      Files: { count: 0, bytes: 0, measured: false },
    }
    for (const a of artifacts.data?.artifacts ?? []) {
      const tile = tileFor(a)
      if (!tile) continue
      out[tile].count += 1
      // WHAT IT WEIGHS, not what its descriptor weighs. `sizeBytes` is the
      // size of the manifest — a couple of kilobytes of JSON — so summing it
      // reported a release of two hundred images as half a megabyte. The
      // server sums the blobs and sends that, and sends nothing for an
      // artifact nobody has walked.
      out[tile].bytes += bytes(a.contentBytes) ?? 0
      // A child the index merely listed has no blobs recorded, so nothing is
      // known about what is under it. Only a fetched manifest has been walked,
      // and only then is the total defensible.
      if (a.fetched) out[tile].measured = true
    }
    return out
  }, [artifacts.data])

  if (pkg.isError) {
    return (
      <>
        <PageHeader title="Package" description="Release detail" />
        <ErrorState error={pkg.error} retry={() => void pkg.refetch()} />
      </>
    )
  }

  const p = pkg.data
  const prod = product.data
  // Analysing walks the manifest tree. Until it has run, the index tells us
  // what its children ARE but nothing about what is inside them — so sizes are
  // not known, and FILES cannot be counted at all: files are layers inside
  // those children rather than children of the index.
  const analysed = Boolean(p?.expandedAt)
  const status = p ? deriveStatus(p, prod) : undefined
  const live = (p?.transfers ?? []).find((t) => isLive(t.state))

  const download = async () => {
    try {
      const result = await runDownload.mutateAsync({ tags: [p!.tag] })
      setConfirming(false)
      message.success(
        result.created?.length
          ? `Download started. It will appear under Downloads.`
          : `This release was already requested; the existing download continues.`,
      )
      navigate('/downloads')
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The download could not be started.')
    }
  }

  const savedBytes = live?.id ? undefined : undefined

  return (
    <>
      <PageHeader
        title={p ? `${prod?.displayName || productName} ${version(p)}` : 'Loading…'}
        description="One release, its contents, and what can be done with it"
        meta={
          p && (
            <Space>
              <StatusBadge status={status!} />
              <VerificationBadge state={verification(p)} />
            </Space>
          )
        }
        extra={
          <Space>
            <Link to={`/compare?product=${encodeURIComponent(productName!)}&from=${encodeURIComponent(p?.tag ?? '')}`}>
              <Button>Compare</Button>
            </Link>
            {live ? (
              <Link to={`/downloads/${live.id}`}>
                <Button type="primary">View download</Button>
              </Link>
            ) : (
              <Tooltip
                title={
                  mayOperate
                    ? 'Downloads the whole release into the internal repositories and configures the mirror OpenShift pulls from.'
                    : 'You do not have permission to start a download.'
                }
              >
                <Button
                  type="primary"
                  icon={<CloudDownloadOutlined />}
                  disabled={!mayOperate || !p}
                  onClick={() => setConfirming(true)}
                >
                  Download
                </Button>
              </Tooltip>
            )}
          </Space>
        }
      />

      <Row gutter={[16, 16]}>
        <Col span={24}>
          {/*
            Two dates on one line, in a strip rather than a card: it is a
            caption for the header above it, not a section of the page.
          */}
          <Card size="small" styles={{ body: { padding: '10px 16px' } }}>
            <ReleaseTimeline
              publishedAt={p?.publishedAt || p?.discoveredAt}
              downloadedAt={p ? downloadedAt(p) : undefined}
              downloading={status === 'DOWNLOADING'}
            />
          </Card>
        </Col>

        <Col xs={24} xl={14}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title="Release" loading={pkg.isLoading}>
              <Descriptions column={2} size="small">
                <Descriptions.Item label="Vendor">
                  <Value>{prod?.sources?.[0]?.vendor || prod?.sources?.[0]?.name}</Value>
                </Descriptions.Item>
                <Descriptions.Item label="Version">
                  <span style={{ fontFamily: mono }}><Value>{p ? version(p) : null}</Value></span>
                </Descriptions.Item>
                <Descriptions.Item label="Published">
                  {p?.publishedAt
                    ? <TimeAgo at={p.publishedAt} />
                    : <NA reason="The publisher set no build date, which the OCI specification permits." />}
                </Descriptions.Item>
                <Descriptions.Item label="Discovered"><TimeAgo at={p?.discoveredAt} /></Descriptions.Item>
                <Descriptions.Item label="Artifacts"><Value>{formatCount(p?.artifactCount)}</Value></Descriptions.Item>
                <Descriptions.Item label="Total size">
                  <Value reason="The manifest tree has not been walked yet, so the size underneath is not known. Measure the release to establish it.">
                    {formatBytes(p?.totalBytes)}
                  </Value>
                </Descriptions.Item>
                <Descriptions.Item label="Digest" span={2}>
                  <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>
                    <Value>{p?.manifestDigest}</Value>
                  </Typography.Text>
                </Descriptions.Item>
              </Descriptions>

              <MeasurePanel
                analysed={analysed}
                artifactCount={p?.artifactCount}
                inspect={inspect}
                disabled={!mayOperate || !p}
              />
            </Card>

            <Card
              title="Contents"
              extra={
                <Typography.Text type="secondary">
                  <Value>{formatCount(p?.artifactCount)}</Value> artifacts
                  {p?.totalBytes ? ` · ${formatBytes(p.totalBytes) ?? ''}` : ''}
                </Typography.Text>
              }
              loading={artifacts.isLoading}
            >
              <Row gutter={16} style={{ marginBottom: 12 }}>
                {(['Images', 'Helm Charts', 'Files'] as const).map((kind) => {
                  // Files are LAYERS inside the release's manifests, not
                  // children of its index, so before the walk there is nothing
                  // to count — which is a different statement from "there are
                  // none", and the tile has to make that difference.
                  const countable = kind !== 'Files' || analysed
                  const sized = groups[kind].measured
                  // FILES ARE COUNTED AS FILES. One `generic` artifact holds
                  // however many named layers the publisher put in it, so the
                  // number of file-KIND artifacts was never the number of
                  // files — it was two, on a release with two hundred.
                  const count = kind === 'Files' && analysed
                    ? (files.data?.files.length ?? 0)
                    : groups[kind].count

                  return (
                    <Col span={8} key={kind}>
                      <Card size="small">
                        <Space direction="vertical" size={0} style={{ width: '100%' }}>
                          <Space size={6}>
                            <Icon as={ARTIFACT_ICONS[kind]} size={15} title={kind} />
                            <Typography.Text type="secondary">{kind}</Typography.Text>
                          </Space>

                          {countable ? (
                            <Typography.Title level={4} style={{ margin: 0 }}>
                              {count}
                            </Typography.Title>
                          ) : (
                            <div style={{ margin: '2px 0' }}>
                              <NA reason="Files are layers inside this release's manifests. They are only listed once the release has been analysed." />
                            </div>
                          )}

                          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                            {sized ? (
                              <Value>{formatBytes(groups[kind].bytes || null)}</Value>
                            ) : (
                              <NA reason="The size under each artifact is only known once the manifest tree has been walked. Analyse the package to establish it." />
                            )}
                          </Typography.Text>
                        </Space>
                      </Card>
                    </Col>
                  )
                })}
              </Row>

              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                A release is downloaded whole — individual artifacts are not selectable.
                {!analysed && ' Analyse the package to list its files and establish its size.'}
              </Typography.Text>

              <Tabs
                size="small"
                style={{ marginTop: 8 }}
                items={(['Images', 'Helm Charts', 'Files'] as const).map((kind) => ({
                  key: kind,
                  label: (
                    <Space size={6}>
                      <Icon as={ARTIFACT_ICONS[kind]} title={kind} />
                      {/*
                        No count on Files until the tree has been walked. One
                        `generic` artifact may hold fifty files, so the number
                        of file-kind ARTIFACTS is not the number of files, and
                        printing it here would contradict the tile beside it.
                      */}
                      {kind}
                      {kind === 'Files'
                        ? analysed && ` ( ${files.data?.files.length ?? 0} )`
                        : ` ( ${groups[kind].count} )`}
                    </Space>
                  ),
                  children: kind === 'Files' && analysed ? (
                    <FileTree files={files.data?.files ?? []} />
                  ) : kind === 'Files' && !analysed ? (
                    <EmptyStateCard
                      title="Files are not listed yet"
                      explanation="Files live inside this release's manifests as layers, so listing them means walking the tree. Analysing the package reads it from the vendor registry and records what is there."
                      action={
                        <MeasurePanel
                          analysed={analysed}
                          artifactCount={p?.artifactCount}
                          inspect={inspect}
                          disabled={!mayOperate || !p}
                        />
                      }
                    />
                  ) : (
                    <ComponentTable
                      artifacts={(artifacts.data?.artifacts ?? []).filter((a) => tileFor(a) === kind)}
                      kind={kind}
                    />
                  ),
                }))}
              />
            </Card>
          </Space>
        </Col>

        <Col xs={24} xl={10}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title="Saved (already present)">
              {savedBytes !== undefined ? (
                <SavedPanel
                  savedBytes={formatBytes(savedBytes) ?? ''}
                  totalBytes={formatBytes(p?.totalBytes) ?? undefined}
                />
              ) : (
                // No number at all rather than a number-shaped dash: how much
                // of a release the destination already holds cannot be known
                // without asking it, and a download is what asks.
                <Typography.Text type="secondary" style={{ fontSize: 13 }}>
                  Existing artifacts are detected and skipped, so a download moves considerably less
                  than the release weighs. How much is already present is measured during the
                  download and reported there — it cannot be known before one runs.
                </Typography.Text>
              )}
            </Card>

            <Card title="Verification">
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                <VerificationBadge state={p ? verification(p) : 'UNKNOWN'} />
                {(p?.related ?? []).filter((r) => r.role === 'SIGNATURE').map((sig) => (
                  <Descriptions key={sig.digest} column={1} size="small">
                    <Descriptions.Item label="Signature type"><Value>{sig.blobMediaType || sig.mediaType}</Value></Descriptions.Item>
                    <Descriptions.Item label="Confirmed"><TimeAgo at={sig.resolvedAt} /></Descriptions.Item>
                  </Descriptions>
                ))}
                {p && verification(p) === 'UNKNOWN' && (
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    This source does not publish signatures in a layout we can discover. That is not the
                    same as the release being unsigned.
                  </Typography.Text>
                )}
              </Space>
            </Card>

            <Card title="Locations">
              <Space direction="vertical" size={10} style={{ width: '100%' }}>
                <LocationChip locations={p ? deriveLocations(p, prod) : []} />
                {(prod?.targets ?? []).map((t) => (
                  <div key={t.name}>
                    <TargetTag target={t} />
                    <div>
                      <RepoLink url={t.registry ? `https://${t.registry.replace(/^https?:\/\//, '')}/${t.repository ?? ''}` : undefined} />
                    </div>
                  </div>
                ))}
              </Space>
            </Card>
          </Space>
        </Col>
      </Row>

      <Modal
        open={confirming}
        title={`Download ${prod?.displayName || productName} ${p ? version(p) : ''}?`}
        okText="Download this release"
        confirmLoading={runDownload.isPending}
        onOk={() => void download()}
        onCancel={() => setConfirming(false)}
      >
        <Typography.Paragraph>
          The whole release is brought into the internal repositories, and the mirror that OpenShift
          pulls from is configured as part of the same operation. Artifacts already present are
          detected and skipped.
        </Typography.Paragraph>
        <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
          {p?.totalBytes
            ? `Up to ${formatBytes(p.totalBytes) ?? ''} across ${formatCount(p.artifactCount) ?? 0} artifacts.`
            : `${formatCount(p?.artifactCount) ?? 0} artifacts. The total size has not been measured yet.`}
        </Typography.Paragraph>
      </Modal>
    </>
  )
}
