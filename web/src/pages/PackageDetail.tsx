import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import type { UseMutationResult } from '@tanstack/react-query'
import {
  Alert, App, Button, Card, Col, Descriptions, Divider, Empty, Modal, Row, Space, Table, Tabs, Tag,
  Tooltip, Tree, Typography,
} from 'antd'
import { FolderOutlined, SafetyCertificateOutlined, SyncOutlined } from '@ant-design/icons'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  packageFileDownloadUrl, useArtifacts, useInspectPackage, usePackage, usePackageFiles,
  useProduct, useRunDownload,
} from '../api/queries'
import { useCan, useIdentity } from '../auth/permissions'
import {
  deriveStatus, downloadedAt, failureReason, isLive, matches, packageReference, repositoryUrl,
  titleCase, verification, version,
} from '../domain/derive'
import { bytes, formatBytes, formatCount, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { AnalyzeIcon, ARTIFACT_ICONS, DownloadIcon, Icon } from '../components/icons'
import { WorkingBar } from '../components/progress'
import {
  AnalysisTag, RepoLink, StatusBadge, TimeAgo, TransferStateTag, VerificationBadge,
} from '../components/chips'
import {
  EmptyStateCard, ErrorState, PageHeader, ReleaseTimeline, SearchBar,
} from '../components/layout'
import { FileViewer, looksBinary } from '../components/filecontent'
import { SecurityTab } from '../components/securitypanel'
import { mono } from '../theme'
import type {
  Artifact, InspectPackageResponse, Package, PackageFile, PackageTransfer, Product, RelatedArtifact,
} from '../api/types'

/**
 * Page 3 - Package (release detail).
 *
 * Answers: everything about this one release, and what can I do with it?
 *
 * Two rules bite hardest here:
 *   - the release downloads WHOLE. The contents are shown and nothing in them
 *     is selectable, because cherry-picking a release is not a thing the
 *     system does.
 *   - every fact on it is one this Coordinator can answer for. What a download
 *     would save, where a release might land, which targets exist - those are
 *     questions about a transfer and they are answered on a transfer's page.
 *     They were three cards here, and all three said "we cannot know yet".
 */

/**
 * Analysing a release, with something to look at while it happens.
 *
 * # Why this is a panel and not a button
 *
 * Analysing walks the release's whole manifest tree against the vendor
 * registry - for the 260-artifact releases this system is built for, that is
 * hundreds of round trips and takes a while. The button reported none of it:
 * it spun in place and the page changed silently, which is indistinguishable
 * from nothing happening.
 *
 * # Why there is no percentage
 *
 * The API has no progress feed for this, and it cannot sensibly have one: the
 * size of the tree is the thing being discovered, so there is no denominator
 * until the work is finished. <WorkingBar> is the honest shape - it shows that
 * work is running and how long it has been running, and states no position.
 *
 * What it CAN report is the outcome, and that is worth reporting in full: a
 * measurement that fetched nothing because the tree was already recorded is a
 * different event from one that read three hundred manifests, and both look
 * identical if all you do is refresh the page.
 */
function MeasurePanel({
  analysed, analysing, artifactCount, inspect, disabled,
}: {
  analysed: boolean
  /** Somebody else is already walking it - discovery, or another reader. */
  analysing?: boolean
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

  // HANDED OFF. The server answers immediately and the walk runs behind it, so
  // there is no result to report here - the release's own state is the report,
  // and it is read by the caller and passed back in as `analysing`. Held until
  // that arrives, so the panel does not blink through "not analysed" between
  // the response and the refetch that notices.
  if (inspect.data?.started && !analysed) {
    return <AnalysingBar requested />
  }

  if (inspect.isPending) {
    // Measured against a registry that could not be reached: the call runs for
    // at least ninety seconds before anything comes back. A bar that says
    // nothing after the first minute is indistinguishable from a hang, so the
    // wording escalates rather than repeating itself.
    const scope = artifactCount
      ? `Reading the manifest tree from the vendor registry - ${artifactCount} artifacts to walk.`
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
            ? 'Already analyzed - the vendor registry was not contacted'
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

  /*
   * SOMEBODY IS ALREADY DOING IT.
   *
   * Discovery walks recently published releases on its own, and a second walk
   * of the same release is a second conversation with the vendor's registry
   * for an answer already on its way. The claim is held in the database, so the
   * server would refuse this anyway - offering it and then refusing it is a
   * worse way to say the same thing.
   */
  if (analysing) {
    return <AnalysingBar />
  }

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
        Walks the manifest tree in the vendor registry to establish what this release contains -
        its files, and what each part weighs. Nothing is downloaded.
      </Typography.Text>
    </Space>
  )
}

/**
 * A walk that is running somewhere else.
 *
 * No elapsed time, deliberately: the walk may have been started by discovery
 * ten minutes ago or by this reader ten seconds ago, and this page knows only
 * that it is happening. A timer counting from the moment the page opened would
 * be a number about the page rather than about the work.
 */
function AnalysingBar({ requested }: { requested?: boolean }) {
  return (
    <div style={{ marginTop: 12 }}>
      <WorkingBar
        label="Analyzing this release"
        detail={
          requested
            ? 'Reading the manifest tree from the vendor registry. It carries on if you '
              + 'leave this page, and the contents appear here when the walk finishes.'
            : 'Reading the manifest tree from the vendor registry. Its contents appear '
              + 'here when the walk finishes.'
        }
      />
    </div>
  )
}

/**
 * What the release is CALLED - `orbs/cfx-5000-k8s`.
 *
 * The vendor's shortened spelling where there is one, for the same reason
 * `version` prefers `displayTag`: it is what the reader has in their hand.
 */
function packageName(p: Package): string {
  return p.displayRepository || p.sourceRepository || p.name
}

/** Who published it, in the vendor's own name where the source declares one. */
function vendorName(product?: Product): string | null {
  const source = product?.sources?.[0]
  if (!source) return null
  return source.vendor ? titleCase(source.vendor) : source.name
}

/** The release's signature, where one was discovered. */
function signature(p?: Package): RelatedArtifact | undefined {
  return (p?.related ?? []).find((r) => r.role === 'SIGNATURE')
}

/**
 * The three kinds a user thinks in, from the kind the API derived.
 *
 * # Why this no longer guesses
 *
 * It used to read the media type and call anything image-ish an image. Every
 * part of a vendor orb is served as `image.manifest.v1+json` with no
 * artifactType, so every chart was counted as an image and Helm Charts read
 * zero on every release. The API now classifies - using the vendor's own
 * annotations where the OCI fields cannot answer - and this only maps its
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
      // `artifact` - content that declared nothing about itself - and any
      // kind this client has not been taught. Counted as Files rather than
      // silently dropped, since it IS part of the release.
      return a.kind ? 'Files' : null
  }
}

/**
 * The name a person recognises.
 *
 * The vendor's ref name is the only thing here that reads like a name -
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
 * The vendor writes them as one string - `cfx-5000-product/bgcf:2511.174.0` -
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
            // string and they are two facts - what the component is, and which
            // version of it this release carries - and in one cell the version
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
 * already hierarchical - the publisher wrote them that way - so the shape is
 * the vendor's, not one this page invented.
 *
 * # What it can show, and what it cannot
 *
 * Every layer that carries `org.opencontainers.image.title` IS one named file,
 * and analysis recorded that name. A layer with no title is a tar of an unknown
 * number of paths - an image layer - and is not listed at all: a row called
 * `layer sha256:…` would be a summary pretending to be an answer, and counting
 * them at the foot only told a reader about something they had not asked
 * about and could not act on.
 */
function FileTree({
  files, onView, product, reference, repository, downloadEnabled,
}: {
  files: PackageFile[]
  onView: (file: PackageFile) => void
  product: string
  reference: string
  repository?: string
  downloadEnabled: boolean
}) {
  const [search, setSearch] = useState('')

  const shown = useMemo(
    () => (search.trim() ? files.filter((f) => matches(search, f.path, f.component)) : files),
    [files, search])

  const tree = useMemo(
    () => buildTree(shown, onView, product, reference, repository, downloadEnabled),
    [shown, onView, product, reference, repository, downloadEnabled])

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
function buildTree(
  files: PackageFile[], onView: (file: PackageFile) => void,
  product: string, reference: string, repository: string | undefined, downloadEnabled: boolean,
): TreeNode[] {
  interface Leaf {
    /** The file itself, with the path the publisher wrote. */
    file: PackageFile
    /** Its name within this directory - what the row shows. */
    name: string
  }
  interface Dir {
    dirs: Map<string, Dir>
    files: Leaf[]
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
    // The FULL path is kept on the leaf. The row shows the name, and opening
    // the file needs to say which one - two files called `values.yaml` under
    // different directories are different files.
    node.files.push({ file, name })
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
    for (const leaf of [...dir.files].sort((a, b) => a.name.localeCompare(b.name))) {
      out.push({
        key: `${prefix}/${leaf.name}-${leaf.file.digest}`,
        title: (
          <Space size={6}>
            <Icon as={ARTIFACT_ICONS.Files} size={13} title="File" />
            <Typography.Text style={{ fontSize: 13 }}>{leaf.name}</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {formatBytes(bytes(leaf.file.sizeBytes))}
            </Typography.Text>
            {/*
              The content is not held here - a release is tens of gigabytes and
              this is deliberately not a copy of it - so this fetches the one
              blob from the vendor registry when somebody asks for it. A file
              that is never text (an archive, an image, a compiled artifact,
              a signature) has nothing for a modal to show, so only Download
              is offered for it - a plain link to the streaming endpoint,
              gated by coordinator.files.downloadEnabled.
            */}
            {!looksBinary(leaf.file.path, leaf.file.mediaType) && (
              <Button
                type="link"
                size="small"
                style={{ padding: 0, height: 'auto', fontSize: 12 }}
                onClick={(e) => {
                  e.stopPropagation()
                  onView(leaf.file)
                }}
              >
                View
              </Button>
            )}
            {downloadEnabled && (
              <a
                href={packageFileDownloadUrl(product, reference, leaf.file.digest, repository)}
                onClick={(e) => e.stopPropagation()}
                style={{ fontSize: 12 }}
              >
                Download
              </a>
            )}
          </Space>
        ),
      })
    }
    return out
  }

  return render(root, '')
}

/**
 * Where this release has been sent, and how each attempt went.
 *
 * Reads what the package already carries rather than fetching: the transfer
 * history travels on the release, and a second request to render a tab nobody
 * may open is a request nobody asked for.
 *
 * A failure keeps its reason VERBATIM, including whatever the registry said.
 * That is what makes an entitlement refusal debuggable weeks after the transfer
 * that hit it, and paraphrasing it here would throw the only useful part away.
 */
function DownloadsTab({ transfers, onDownload, downloadHref, mayOperate }: {
  transfers: PackageTransfer[]
  /** Starts one, when there is none and the reader may. */
  onDownload?: () => void
  /** Where the existing download is, when there is one. */
  downloadHref?: string
  mayOperate?: boolean
}) {
  if (transfers.length === 0) {
    return (
      <EmptyStateCard
        title="This release has not been downloaded"
        explanation="Nothing has been transferred from the vendor registry for this release yet. Starting a download brings the whole release into the internal repositories."
        /*
          THE BUTTON, not a shrug.

          An empty state that explains what would appear here and offers no way
          to make it appear sends the reader back to the top of the page to find
          the control they were already looking for. The header keeps its
          Download button - this is the same act, offered where the absence is.
        */
        action={
          downloadHref
            ? (
              <Link to={downloadHref}>
                <Button icon={<Icon as={DownloadIcon} title="Download" />}>View download</Button>
              </Link>
            )
            : (
              <Tooltip
                title={mayOperate
                  ? 'Downloads the whole release into the internal repositories and configures the mirror OpenShift pulls from.'
                  : 'You do not have permission to start a download.'}
              >
                <Button
                  type="primary"
                  icon={<Icon as={DownloadIcon} title="Download" />}
                  disabled={!mayOperate || !onDownload}
                  onClick={onDownload}
                >
                  Download this release
                </Button>
              </Tooltip>
            )
        }
      />
    )
  }
  return (
    <Card size="small" styles={{ body: { padding: 0 } }}>
      <Table<PackageTransfer>
        size="small"
        rowKey={(t) => t.id}
        dataSource={transfers}
        pagination={false}
        scroll={{ x: 'max-content' }}
        columns={[
          {
            title: 'Destination',
            render: (_, t) => <Typography.Text style={{ fontFamily: mono }}>{t.target}</Typography.Text>,
          },
          {
            title: 'State',
            width: 170,
            /*
              The transfer's OWN state, not the release's. A release is
              downloaded to several destinations and each attempt has its own
              outcome; collapsing them into one status is what made a page say
              "downloaded" while one of three destinations had failed.

              As the SAME BADGE the Downloads page uses for the same fact. It
              was the only status in the application rendered as plain text -
              which made a failed transfer read exactly like a successful one
              until somebody read the word, and made this table look like a
              different application from the page its rows link to.
            */
            render: (_, t) => (
              <Space direction="vertical" size={2}>
                <TransferStateTag state={t.state} />
                {t.failureReason && (
                  <Typography.Text type="danger" style={{ fontSize: 11 }} ellipsis={{ tooltip: t.failureReason }}>
                    {t.failureReason}
                  </Typography.Text>
                )}
              </Space>
            ),
          },
          { title: 'Started', width: 150, render: (_, t) => <TimeAgo at={t.createdAt} /> },
          { title: 'Finished', width: 150, render: (_, t) => <TimeAgo at={t.completedAt} /> },
          {
            // Named, because a column with no heading reads as one somebody
            // forgot to fill in - and the button under it is a link to another
            // page rather than a "view" of the row it sits on.
            title: 'Action',
            width: 150,
            render: (_, t) => (
              <Link to={`/downloads/${t.id}`}>
                <Button size="small" icon={<Icon as={DownloadIcon} title="Download" />}>
                  View downloads
                </Button>
              </Link>
            ),
          },
        ]}
      />
    </Card>
  )
}

function FactTag({ label, value, mono: isMono, onClick }: {
  label: string
  value?: string
  mono?: boolean
  onClick?: () => void
}) {
  if (!value) return null
  return (
    <Tag
      style={{
        margin: 0, background: '#FFFFFF', cursor: onClick ? 'pointer' : undefined,
        paddingInline: 8,
      }}
      onClick={onClick}
    >
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>{label} </Typography.Text>
      <Typography.Text style={{ fontSize: 12, fontFamily: isMono ? mono : undefined }}>{value}</Typography.Text>
    </Tag>
  )
}

/**
 * The vulnerability count for the header, or the reason there is not one.
 *
 * Never a bare zero. "0" beside a release nobody has scanned is the single
 * misreading this whole feature exists to prevent, and the header is the most
 * quoted line on the page.
 */
function vulnerabilityFact(p: Package): string | undefined {
  const s = p.security
  if (!s || !s.canSync) return undefined
  switch (s.state) {
    case 'syncing':
      // Never a bare "Syncing" for a sync nobody is running. See
      // PackageSecuritySummary.stalled.
      return s.stalled ? 'Sync interrupted' : 'Syncing'
    case '':
      return 'Not synced'
    case 'failed':
      return s.syncedAt ? `${s.counts.total.toLocaleString()} (last sync failed)` : 'Sync failed'
    default:
      return s.scanned === 0 ? 'No results' : s.counts.total.toLocaleString()
  }
}

export default function PackageDetail() {
  const { product: productName, reference } = useParams()
  const [params, setParams] = useSearchParams()
  const navigate = useNavigate()
  const { message } = App.useApp()

  // A version tag is not unique within a product - vendors publish the same
  // tag into every repository - so the repository travels with the link and is
  // what makes this lookup answerable. See releaseHref in domain/derive.
  const repository = params.get('repository') ?? undefined

  const product = useProduct(productName)
  const pkg = usePackage(productName, reference, repository)
  const artifacts = useArtifacts(productName, reference, repository)
  const files = usePackageFiles(productName, reference, repository)
  const inspect = useInspectPackage(productName!, reference!, repository)
  const runDownload = useRunDownload(productName!)

  // Which tab is open, in the URL so a link to a release's security is a link.
  const tab = params.get('tab') ?? 'details'
  const switchTab = (key: string) => {
    const next = new URLSearchParams(params)
    if (key === 'details') next.delete('tab')
    else next.set('tab', key)
    setParams(next, { replace: true })
  }

  const mayOperate = useCan('operate', { product: productName })
  const { who } = useIdentity()
  const downloadEnabled = who?.features?.fileDownloads ?? false
  const [confirming, setConfirming] = useState(false)
  const [viewing, setViewing] = useState<PackageFile>()

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
      // size of the manifest - a couple of kilobytes of JSON - so summing it
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
        <ErrorState error={pkg.error} retry={() => void pkg.refetch()} />
      </>
    )
  }

  const p = pkg.data
  const prod = product.data
  // Analysing walks the manifest tree. Until it has run, the index tells us
  // what its children ARE but nothing about what is inside them - so sizes are
  // not known, and FILES cannot be counted at all: files are layers inside
  // those children rather than children of the index.
  const analysed = Boolean(p?.expandedAt)
  // Somebody is walking it RIGHT NOW - discovery's background analyser, or
  // another person. What the page offers has to say so, or it invites a second
  // walk of the same release against the same registry.
  const analysing = p?.analysisState === 'analyzing'
  const status = p ? deriveStatus(p, prod) : undefined
  const live = (p?.transfers ?? []).find((t) => isLive(t.state))
  const failed = (p?.transfers ?? []).find((t) => t.state === 'FAILED')
  const succeeded = (p?.transfers ?? []).find((t) => t.state === 'SUCCEEDED')
  const existingDownload = live ?? failed ?? succeeded

  const download = async () => {
    try {
      // Qualified by repository - see packageReference. This page knows which
      // package it is showing; the version alone does not.
      const result = await runDownload.mutateAsync({ tags: [packageReference(p!)] })
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

  return (
    <>
      <PageHeader
        back={{ to: '/packages', label: 'All packages' }}
        /*
          THE RELEASE FIRST, the product second.

          The title was the product and the version, which is the one pairing
          that does not identify anything: a product publishes ten packages and
          they all carry the same version tag. What a reader came here for is
          `orbs/cfx-5000-k8s:25.7_mp2604pp3_2204` - the package and the version
          together - and which product it belongs to is context for that, not a
          replacement for it.
        */
        title={p ? `${packageName(p)}:${version(p)}` : 'Loading…'}
        /*
          THE PRODUCT AND THE BADGES ON ONE LINE.

          They were two things a line apart: the product name under the title,
          and a card below it holding the same badges plus four labelled facts.
          The card was a box around a single row - a heading's worth of chrome
          for one line of text - and it pushed the tabs, which are what the page
          is actually for, a hundred pixels down the screen.

          Both halves say what this release IS, so they are one line: what it
          belongs to, then its state and its shape.
        */
        description={
          <div
            style={{
              display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '6px 10px', marginTop: 2,
            }}
          >
            <Typography.Text type="secondary">{prod?.displayName || productName}</Typography.Text>
            {p && (
              <>
                <Divider type="vertical" style={{ margin: 0 }} />
                <StatusBadge status={status!} reason={failureReason(p)} />
                <AnalysisTag pkg={p} />
                <VerificationBadge state={verification(p)} />
                <FactTag label="Size" value={formatBytes(p.totalBytes) ?? undefined} />
                <FactTag label="Artifacts" value={formatCount(p.artifactCount) ?? undefined} />
                <FactTag
                  label="Vulnerabilities"
                  value={vulnerabilityFact(p)}
                  onClick={() => switchTab('security')}
                />
              </>
            )}
          </div>
        }
        extra={
          <Space>
            <Link to={`/packages/compare?product=${encodeURIComponent(productName!)}&from=${encodeURIComponent(p?.tag ?? '')}`}>
              <Button>Compare</Button>
            </Link>
            {existingDownload ? (
              <Link to={`/downloads/${existingDownload.id}`}>
                <Button type="primary" icon={<Icon as={DownloadIcon} title="Download" />}>View download</Button>
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
                  icon={<Icon as={DownloadIcon} title="Download" />}
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
            WHEN, on the left, as a line rather than as a card.

            It sat on the right of the badge card, which put the two dates in a
            release's life in the corner a reader's eye reaches last, boxed in
            chrome that said nothing. It is a timeline: it belongs at the start
            of the line, and it needs no border to be one.
          */}
          <div style={{ paddingBottom: 4 }}>
            <ReleaseTimeline
              publishedAt={p?.publishedAt || p?.discoveredAt}
              downloadedAt={p ? downloadedAt(p) : undefined}
              downloading={status === 'DOWNLOADING'}
            />
          </div>
        </Col>

        <Col span={24}>
          {/*
            TABS, not a stack of cards.

            The page carries five different kinds of answer about one release -
            what it is, what is in it, what is wrong with it, where it has been
            sent - and stacking them meant the last of them lived below three
            screens of the others. A reader who came to check the security of a
            release should not scroll past its manifest tree to reach it.

            The tab is in the URL, so "the security of this release" is a link
            somebody can send.
          */}
          <Tabs
            activeKey={tab}
            onChange={switchTab}
            items={[
              {
                key: 'details',
                /*
                  ICONS ON THE TABS, because they are three different KINDS of
                  answer - what it is, what is wrong with it, where it has been
                  sent - and three words in a row at the same weight make a
                  reader read all three every time. A mark each is what makes
                  the second visit a glance.
                */
                label: (
                  <Space size={6}>
                    <Icon as={ARTIFACT_ICONS.Index} title="Details" />
                    Details
                  </Space>
                ),
                children: (
                  <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Card title="Release" loading={pkg.isLoading}>
                    <Descriptions column={2} size="small">
                      {/*
                        WHERE IT IS, first and clickable. This was a "Locations" card
                        of its own listing every configured target - which is a fact
                        about the product, repeated on every one of its releases, and
                        said nothing about whether THIS release is at any of them.
                        What belongs to a release is the one place it was published,
                        and that is a link.
                      */}
                      <Descriptions.Item label="Location">
                        <RepoLink
                          url={repositoryUrl(prod?.sources?.[0], p?.sourceRepository)}
                          label={vendorName(prod) ?? undefined}
                        />
                      </Descriptions.Item>
                      <Descriptions.Item label="Package">
                        <span style={{ fontFamily: mono }}><Value>{p ? packageName(p) : null}</Value></span>
                      </Descriptions.Item>
                      <Descriptions.Item label="Version">
                        <span style={{ fontFamily: mono }}><Value>{p ? version(p) : null}</Value></span>
                      </Descriptions.Item>
                      <Descriptions.Item label="Product">
                        <Value>{prod?.displayName || productName}</Value>
                      </Descriptions.Item>
                      <Descriptions.Item label="Published">
                        {p?.publishedAt
                          ? <TimeAgo at={p.publishedAt} />
                          : <NA reason="The publisher set no build date, which the OCI specification permits." />}
                      </Descriptions.Item>
                      <Descriptions.Item label="Discovered"><TimeAgo at={p?.discoveredAt} /></Descriptions.Item>
                      <Descriptions.Item label="Total size">
                        <Value reason="The manifest tree has not been walked yet, so the size underneath is not known. Measure the release to establish it.">
                          {formatBytes(p?.totalBytes)}
                        </Value>
                      </Descriptions.Item>
                      {/*
                        Verification, in the release rather than in a card of its own.
                        It was three lines in a panel with a heading, and the heading
                        was the largest thing in it: whether a release is signed is a
                        property OF the release, and it belongs on the same list as
                        its digest.
                      */}
                      <Descriptions.Item label="Signature">
                        <VerificationBadge state={p ? verification(p) : 'UNKNOWN'} />
                      </Descriptions.Item>
                      <Descriptions.Item label="Signature type" span={2}>
                        <Value reason="No signature was discovered for this release, so there is no type to report.">
                          {signature(p)?.blobMediaType || signature(p)?.mediaType || null}
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
                      analysing={analysing}
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
                          // to count - which is a different statement from "there are
                          // none", and the tile has to make that difference.
                          const countable = kind !== 'Files' || analysed
                          const sized = groups[kind].measured
                          // FILES ARE COUNTED AS FILES. One `generic` artifact holds
                          // however many named layers the publisher put in it, so the
                          // number of file-KIND artifacts was never the number of
                          // files - it was two, on a release with two hundred.
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
                                      <NA />
                                    </div>
                                  )}

                                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                                    {sized ? (
                                      <Value>{formatBytes(groups[kind].bytes || null)}</Value>
                                    ) : (
                                      <NA />
                                    )}
                                  </Typography.Text>

                                  {/*
                                    ONE SENTENCE INSTEAD OF THREE TOOLTIPS. Both lines
                                    above read `N/A` before the walk, each with its own
                                    explanation on hover - which is a fact hidden behind
                                    a gesture nobody makes on a tile that looks broken.
                                    What is missing and what to do about it are the same
                                    answer, and it is short.
                                  */}
                                  {(!countable || !sized) && (
                                    <Typography.Text
                                      type="secondary"
                                      italic
                                      style={{ fontSize: 11, lineHeight: '14px' }}
                                    >
                                      {analysing
                                        ? 'analyzing this release now'
                                        : 'analyze the package to view details'}
                                    </Typography.Text>
                                  )}
                                </Space>
                              </Card>
                            </Col>
                          )
                        })}
                      </Row>

                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
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
                            <FileTree
                              files={files.data?.files ?? []}
                              onView={setViewing}
                              product={productName!}
                              reference={reference!}
                              repository={repository}
                              downloadEnabled={downloadEnabled}
                            />
                          ) : kind === 'Files' && !analysed ? (
                            <EmptyStateCard
                              title="Files are not listed yet"
                              explanation="Files live inside this release's manifests as layers, so listing them means walking the tree. Analysing the package reads it from the vendor registry and records what is there."
                              action={
                                <MeasurePanel
                                  analysed={analysed}
                                  analysing={analysing}
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
                ),
              },
              {
                key: 'security',
                /*
                  The count rides on the label, because the question the tab
                  answers is usually "how bad is it" and making somebody open a
                  tab to find out is one click too many. Absent - not zero -
                  where nothing has been synced.
                */
                label: (
                  <Space size={6}>
                    <SafetyCertificateOutlined />
                    Security
                    {p?.security?.state === 'synced' && (
                      <Typography.Text
                        type={p.security.counts.total > 0 ? undefined : 'secondary'}
                        style={{ fontSize: 12 }}
                      >
                        ({p.security.counts.total.toLocaleString()})
                      </Typography.Text>
                    )}
                    {/*
                      A spinner, not the word "syncing…".

                      A tab label is a NAME. Appending a lowercase participle to
                      one turns it into a sentence fragment that changes length
                      as the state changes, so the tabs beside it move; and the
                      running state is exactly what an icon says better than a
                      word. The word is still there for anyone who needs it, in
                      the tooltip and on the panel the tab opens.
                    */}
                    {p?.security?.state === 'syncing' && !p.security.stalled && (
                      <Tooltip title="A vulnerability sync is in progress">
                        <SyncOutlined spin style={{ fontSize: 12 }} />
                      </Tooltip>
                    )}
                  </Space>
                ),
                children: productName && reference
                  ? <SecurityTab product={productName} reference={reference} repository={repository} />
                  : null,
              },
              {
                key: 'downloads',
                label: (
                  <Space size={6}>
                    <Icon as={DownloadIcon} title="Downloads" />
                    Downloads
                    {p?.transfers?.length ? `(${p.transfers.length})` : ''}
                  </Space>
                ),
                children: (
                  <DownloadsTab
                    transfers={p?.transfers ?? []}
                    // The empty state OFFERS the download rather than
                    // describing one. See DownloadsTab.
                    onDownload={existingDownload ? undefined : () => setConfirming(true)}
                    downloadHref={existingDownload ? `/downloads/${existingDownload.id}` : undefined}
                    mayOperate={mayOperate && Boolean(p)}
                  />
                ),
              },
            ]}
          />
        </Col>
      </Row>

      <FileViewer
        product={productName}
        reference={reference}
        repository={repository}
        path={viewing?.path}
        digest={viewing?.digest}
        onClose={() => setViewing(undefined)}
      />

      <Modal
        open={confirming}
        title={p ? `Download ${packageName(p)}:${version(p)}?` : 'Download this release?'}
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
