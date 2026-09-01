import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { App, Button, Card, Col, Popover, Progress, Row, Segmented, Select, Space, Tooltip, Tree, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { FolderOutlined, SwapOutlined } from '../icons'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import {
  useCompare, useCompareProgress, useCompareSecurity, usePackages, usePackageSecurity, useProduct,
  useProducts, useSyncPackageSecurity,
} from '../api/queries'
import { hasSecurityData, kindName, matches, packageReference, version } from '../domain/derive'
import { selectionHref } from '../domain/compare'
import { bytes, formatBytes, formatCount, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { ErrorState, SearchBar } from '../components/layout'
import { WorkingBar } from '../components/progress'
import { ARTIFACT_ICONS, Icon } from '../components/icons'
import { SecurityComparison } from '../components/securitycompare'
import { c, EmptyState, mono, StatusPill, type PillTone } from '../uikit'
import type {
  CompareFile, CompareProgressSide, CompareResponse, CompareRow, CompareVerdict, Package,
  Repository,
} from '../api/types'

/**
 * Page 5 - Compare.
 *
 * Answers: what is different between these two, exactly?
 *
 * # This page no longer asks the question
 *
 * It used to open on a form: a product select, a two-position mode switch, two
 * release dropdowns, a swap button and sometimes a fourth select for the
 * source. Six controls to express "these two" - and the two dropdowns were the
 * worst of it, because a list of two hundred releases rendered as a name and a
 * version in a 320-pixel box gave a reader no way to tell which two they wanted.
 * Everything needed to DECIDE - what is in production, what arrived last week,
 * what has vulnerabilities - was on the packages listing they had just left.
 *
 * So the choosing happens there (see domain/compare.ts), and this page is the
 * ANSWER. It arrives with a pair in its URL, runs the comparison, and shows it.
 * Arriving without one sends the reader back to the listing, in selection mode,
 * because that is where the question is now asked.
 *
 * # The one form that survives
 *
 * A LOCATION comparison - "did this release arrive intact" - is a question
 * about one release rather than about two, so it cannot be expressed by ticking
 * two rows. It keeps two selects, reached from a release's own row menu, and
 * they are two controls about a release already named rather than four about a
 * release that is not.
 */

/**
 * The configured SOURCE a release was discovered in.
 *
 * The API needs an endpoint NAME, and a package records a repository PATH. A
 * product with more than one source will not infer which end is meant - it
 * refuses rather than guessing - so the path is matched back to the source
 * that declares it, which is knowable and saves asking the reader for
 * something the release already implies.
 */
function sourceNameFor(
  sources: Repository[] | undefined, repositoryPath: string | undefined,
): string | undefined {
  if (!repositoryPath) return undefined
  for (const s of sources ?? []) {
    if (s.repository === repositoryPath) return s.name
    if (s.repositories?.includes(repositoryPath)) return s.name
  }
  return undefined
}

/*
 * How a release is identified in these selects: `repository:tag`, which is the
 * reference the API resolves and the only unambiguous one. A vendor publishes
 * one version tag into every repository a product watches, so a select keyed on
 * the tag alone had ten options with the same value and the same label, and
 * picking any of them asked the server a question it correctly refused to
 * answer.
 *
 * The same function the download button names a release with - there is one
 * way to say which package you mean, and two would drift.
 */
const refOf = packageReference

/**
 * Which comparison is on screen.
 *
 * Two views of ONE selection, not two pages. Somebody who has just waited for a
 * comparison of two releases and now wants the security answer should not
 * re-choose the same two releases in a different place - and a security answer
 * about a different pair from the one above it would be worse than no answer.
 */
type View = 'package' | 'security'

/**
 * One end of a comparison: which package, and which version of it.
 *
 * Two lines rather than one string, because the package name is sixty
 * characters of vendor reference and the version is the part somebody is
 * comparing. Joined, the version falls off the end of the line.
 */
function ComparedEnd({ pkg, fallback }: { pkg?: Package; fallback: string }) {
  if (!pkg) {
    return (
      <Typography.Text strong style={{ fontFamily: mono, fontSize: 13 }}>{fallback}</Typography.Text>
    )
  }
  const name = pkg.displayRepository || pkg.sourceRepository || ''
  return (
    <Space direction="vertical" size={0} style={{ lineHeight: 1.3 }}>
      <Typography.Text strong style={{ fontFamily: mono, fontSize: 13 }}>
        {version(pkg)}
      </Typography.Text>
      {name && (
        <Typography.Text type="secondary" style={{ fontSize: 11, fontFamily: mono }}>
          {name}
        </Typography.Text>
      )}
    </Space>
  )
}

/**
 * Why the vulnerability comparison cannot be given, in one sentence.
 *
 * Names the release rather than saying "one of them", because the reader's next
 * action is to sync THAT one - and a message that makes them work out which is
 * a message that costs a click to be useful.
 */
function unscannedReason(unscanned: Package[] | undefined): string {
  if (!unscanned || unscanned.length === 0) return ''
  if (unscanned.length === 2) return 'Neither release has been scanned yet.'
  return `${version(unscanned[0]!)} has not been scanned yet.`
}

/**
 * What the comparison is actually doing.
 *
 * # Why a real bar and not an animation
 *
 * The work is countable: each side reads a known set of manifests and then
 * probes a known set of component names. An animation says only "something is
 * happening", which is exactly what it would say if nothing were - and on a
 * request that legitimately runs for minutes, that is the difference between
 * waiting and giving up.
 *
 * # Per side, named as the reader named them
 *
 * The two ends are walked concurrently against different registries and one of
 * them is usually the slow one, so a single merged number hides which. Each row
 * is labelled with the PACKAGE and VERSION the reader picked - the server's own
 * label for a side is its source, `cfx-near`, which is the same word on both
 * rows of a version comparison and identifies neither.
 *
 * # The three things somebody watching a four-minute bar wants
 *
 * Where it has got to, whether it is going as fast as it can, and whether it is
 * re-reading something it already has. All three are here: the position, the
 * concurrency each side is running at, and whether that release had been
 * analysed before this started.
 */
function ComparisonProgress({
  token, elapsedSeconds, base, against,
}: {
  token: string | undefined
  elapsedSeconds: number
  /** The releases the reader picked, in order. Absent in locations mode. */
  base?: Package
  against?: Package
}) {
  const progress = useCompareProgress(token)
  const sides = progress.data?.sides ?? []

  // Round trips, summed. The unit is the same on both sides and in every phase,
  // so the total grows as work is discovered and the count never drops - which
  // is what a bar filling, emptying and filling again was.
  const done = sides.reduce((n, s) => n + s.done, 0)
  const total = sides.reduce((n, s) => n + s.total, 0)
  const estimated = sides.some((s) => s.estimated)
  const percent = total > 0 ? Math.min(99, (done / total) * 100) : 0
  const parallel = Math.max(0, ...sides.map((s) => s.concurrency ?? 0))

  const picked: Record<string, Package | undefined> = { a: base, b: against }


  return (
    <Space direction="vertical" size={10} style={{ width: '100%' }}>
      {sides.length === 0 ? (
        <WorkingBar
          label="Analysing packages"
          detail="Fetching package contents. This may take a while."
          elapsedSeconds={elapsedSeconds}
        />
      ) : (
        <>
          <Progress
            percent={Number(percent.toFixed(1))}
            status="active"
            format={() => `${formatCount(done)} of ${formatCount(total)}${estimated ? '+' : ''}`}
          />

          {sides.map((side) => (
            <SideProgress
              key={side.key ?? side.side}
              side={side}
              pkg={side.key ? picked[side.key] : undefined}
              fallback={side.side}
            />
          ))}
        </>
      )}

      <Space size={16} wrap style={{ rowGap: 4 }}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {formatDuration(elapsedSeconds)} elapsed
        </Typography.Text>
        {parallel > 0 && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {parallel} requests in parallel per side
          </Typography.Text>
        )}
        <CacheNote base={base} against={against} />
      </Space>
    </Space>
  )
}

/** One end: what it is, where it has got to, and what it is doing. */
function SideProgress({
  side, pkg, fallback,
}: {
  side: CompareProgressSide
  pkg?: Package
  fallback: string
}) {
  const percent = side.total > 0 ? Math.min(99, (side.done / side.total) * 100) : 0

  return (
    <Row gutter={12} align="middle" wrap={false}>
      <Col flex="0 0 240px" style={{ minWidth: 0 }}>
        <Typography.Text
          style={{ fontFamily: mono, fontSize: 12, display: 'block' }}
          ellipsis={{ tooltip: pkg ? `${pkg.displayRepository || pkg.sourceRepository}:${version(pkg)}` : fallback }}
        >
          {pkg ? version(pkg) : fallback}
        </Typography.Text>
        <Typography.Text
          type="secondary"
          style={{ fontSize: 11, display: 'block' }}
          ellipsis
        >
          {pkg ? (pkg.displayRepository || pkg.sourceRepository) : ''}
        </Typography.Text>
      </Col>
      <Col flex="auto" style={{ minWidth: 0 }}>
        <Progress
          percent={Number(percent.toFixed(0))}
          size="small"
          status={side.phase === 'finished' ? 'success' : 'active'}
          showInfo={false}
        />
      </Col>
      <Col flex="0 0 220px">
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          {side.phase} · {formatCount(side.done)}
          {side.total > 0 && ` of ${formatCount(side.total)}${side.estimated ? '+' : ''}`}
        </Typography.Text>
      </Col>
    </Row>
  )
}

/**
 * Whether this comparison is re-reading something it already has.
 *
 * The question behind "I analysed the package and this is no faster". Analysis
 * records a release's manifest tree, and a comparison reads it from there
 * rather than from the registry - so a release that has been analysed costs
 * hundreds fewer round trips, and one that has not is being analysed right now
 * for everything that asks next.
 *
 */
function CacheNote({ base, against }: { base?: Package; against?: Package }) {
  const picked = [base, against].filter((p): p is Package => Boolean(p))
  if (picked.length === 0) return null

  const analysed = picked.filter((p) => p.expandedAt).length
  const note =
    analysed === picked.length
      ? 'Manifests found in cache, comparing the releases'
      : analysed === 0
        ? 'No cache for either release, reading their manifests now'
        : 'One release found in cache; reading the other now'

  return (
    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
      {note}
    </Typography.Text>
  )
}

/*
  The four verdicts, and the ONE place their word and their colour are decided.

  `tone` rather than a component-library preset colour: the presets are Ant
  Design's own scales, which no palette reaches and no theme moves, and they
  were the last thing on this page still saying green in a colour nothing else
  on screen used. The design system's own chip takes a tone.

  The words stay this page's - "Changed" rather than the kit's "Modified", and
  amber rather than the kit's blue - because the composition band, the bucket
  figures and this column all read from here, and a chip that disagreed with
  the bar above it would be worse than one that is merely not shared.
*/
const VERDICT: Record<CompareVerdict, { label: string; tone: PillTone; statColour?: string }> = {
  same: { label: 'Unchanged', tone: 'neutral' },
  changed: { label: 'Changed', tone: 'pending', statColour: c.pending },
  'only-a': { label: 'Removed', tone: 'danger', statColour: c.danger },
  'only-b': { label: 'Added', tone: 'ok', statColour: c.ok },
}

type Mode = 'versions' | 'locations'


/**
 * The answer, once there is one.
 *
 * # What a comparison is actually asked
 *
 * Not "how many things differ" - that is a number somebody reads once and can
 * act on never. It is asked because a release is about to be shipped, or has
 * just landed somewhere and might be wrong, and the questions are: what KIND of
 * thing changed, HOW MUCH of it, and WHICH ones. So the summary counts by
 * bucket and by kind and by size, and the table lists every component rather
 * than the differing ones with a footnote about the rest.
 */
function ComparisonReport({ report }: { report: CompareResponse }) {
  const [search, setSearch] = useState('')
  const [impact, setImpact] = useState<'all' | CompareVerdict>('all')
  /*
   * FILES ARE A TYPE, not a second card.
   *
   * They were pulled out into a card of their own because the answer for them
   * is a directory tree and the answer for images is a table. That is a
   * difference in how one type is BEST SHOWN, and it does not make files a
   * different question - a reader asking what changed wants images, charts and
   * files in one place, filtered the same way.
   *
   * So the card keeps one type filter and swaps its own body: components for
   * the two kinds a table suits, a tree for the one it does not.
   */
  const [kind, setKind] = useState<'all' | 'image' | 'chart' | 'file'>('all')

  /*
   * THE RELEASE'S CONTENT, not its scaffolding.
   *
   * The index changes whenever anything under it changes - that is what a
   * digest of a list of digests does - and the signature changes because it
   * signs the index. Reporting either as a difference is reporting that the
   * comparison worked, in two rows that appear at the top of every result and
   * mean nothing to anybody.
   */
  const content = useMemo(
    () => report.rows.filter((r) => r.type !== 'index' && r.type !== 'signature'),
    [report.rows])

  const rows = useMemo(() => {
    const byImpact = impact === 'all' ? content : content.filter((r) => r.verdict === impact)
    const byKind = kind === 'all' ? byImpact : byImpact.filter((r) => r.type === kind)
    if (!search.trim()) return byKind
    return byKind.filter((r) => matches(
      search, r.name, r.type, r.a?.tag, r.b?.tag, r.a?.digest, r.b?.digest))
  }, [content, impact, kind, search])

  /*
   * THE FILES, from every component rather than from the file-kind ones.
   *
   * A file lives inside whatever component carries it, and a vendor's
   * configuration bundle is a `file` component while a Helm chart's values are
   * inside a `chart`. Filtering the components to `file` first would answer a
   * narrower question than the one the tab asks.
   *
   * Filtered by the SAME impact segment as the components, so Added, Removed
   * and Changed mean one thing on this card whichever type is selected.
   */
  const allFiles = useMemo(() => {
    const out: FileEntry[] = []
    for (const row of content) {
      for (const f of row.files ?? []) out.push({ ...f, component: row.name })
    }
    return out
  }, [content])

  const fileCount = allFiles.length
  const fileCounts = useMemo(() => {
    const out: Record<string, number> = {}
    for (const f of allFiles) out[f.verdict] = (out[f.verdict] ?? 0) + 1
    return out
  }, [allFiles])
  const shownFiles = useMemo(() => {
    const byImpact = impact === 'all' ? allFiles : allFiles.filter((f) => f.verdict === impact)
    if (!search.trim()) return byImpact
    return byImpact.filter((f) => matches(search, f.path, f.component))
  }, [allFiles, impact, search])

  // Counted from the rows rather than from the report's totals, so the buckets
  // and their breakdowns cannot disagree with the table under them.
  const buckets = useMemo(() => summarise(content), [content])

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <CompositionBand buckets={buckets} />

      <Card
        title={
          /*
            The count and the search on ONE line.

            The box had a band of its own between the header and the table, and
            the enhanced table puts its own controls in a band under that - so
            there were three strips of chrome, two of them nearly empty, before
            the reader reached a row. What somebody is looking AT and what they
            are looking FOR belong on the same line.
          */
          <Space size={12} wrap>
            <span>
              {kind === 'file'
                ? `Files (${formatCount(fileCount)})`
                : `Components (${formatCount(content.length)})`}
            </span>
            <SearchBar
              value={search}
              onChange={setSearch}
              placeholder={kind === 'file'
                ? 'Search files by path or component'
                : 'Search by name, tag or digest'}
              matched={kind === 'file' ? shownFiles.length : rows.length}
              total={kind === 'file' ? fileCount : content.length}
              width={260}
              style={{ marginBottom: 0 }}
            />
          </Space>
        }
        extra={
          <Space size={12} wrap>
            <Segmented
              size="small"
              value={impact}
              onChange={(v) => setImpact(v as typeof impact)}
              /*
                The counts follow the TYPE. Added, Removed, Changed and
                Unchanged mean the same thing on both views - the numbers
                beside them are components on one and files on the other, and a
                count that did not change when the view did would be labelling
                the wrong population.
              */
              options={(['all', 'only-b', 'only-a', 'changed', 'same'] as const).map((v) =>
                v === 'all'
                  ? { value: v, label: 'All' }
                  : {
                      value: v,
                      label: `${VERDICT[v].label} ${formatCount(
                        kind === 'file' ? fileCounts[v] ?? 0 : buckets[v].count)}`,
                    })}
            />
            <Segmented
              size="small"
              value={kind}
              onChange={(v) => setKind(v as typeof kind)}
              options={[
                { value: 'all', label: 'Everything' },
                /*
                  Files included: selecting it swaps the body of this card for
                  the file tree, rather than showing two file COMPONENTS and
                  their layer digests - the OCI view of a thing whose whole
                  point is the files inside it.
                */
                ...(['image', 'chart', 'file'] as const).map((k) => {
                  const name = kindName(k)
                  const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]
                  return {
                    value: k,
                    label: (
                      <Space size={4}>
                        {icon && <Icon as={icon} size={13} title={name} />}
                        {name}
                      </Space>
                    ),
                  }
                }),
              ]}
            />
          </Space>
        }
        styles={{ body: { padding: 0 } }}
      >
        {kind === 'file' ? (
          <div style={{ padding: '8px 16px 16px' }}>
            <FileDifferences files={shownFiles} total={fileCount} />
          </div>
        ) : (
        <DataTable<CompareRow>
          tableEnhancedKey="compare-rows"
          allow_export
          size="small"
          dataSource={rows}
          rowKey={(r) => `${r.type}-${r.name}-${r.verdict}-${r.a?.digest ?? ''}-${r.b?.digest ?? ''}`}
          pagination={{ pageSize: 25, showSizeChanger: false, size: 'small' }}
          scroll={{ x: 1200 }}
          locale={{
            emptyText: (
              <EmptyState title="Nothing matches this filter" />
            ),
          }}
          columns={[
            {
              title: 'Name',
              width: 300,
              render: (_, r) => (
                <Space direction="vertical" size={0} style={{ minWidth: 0 }}>
                  <Tooltip title={r.name}>
                    <Typography.Text style={{ fontSize: 13, maxWidth: 280 }} ellipsis>
                      {r.name || <NA reason="This component carries no name of its own." />}
                    </Typography.Text>
                  </Tooltip>
                  {(r.b?.tag || r.a?.tag) && (
                    <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 11 }}>
                      {r.b?.tag || r.a?.tag}
                    </Typography.Text>
                  )}
                </Space>
              ),
            },
            {
              title: 'Type',
              width: 130,
              render: (_, r) => {
                const name = kindName(r.type)
                const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]
                return (
                  <Space size={6}>
                    {icon && <Icon as={icon} size={15} title={name} />}
                    <Typography.Text style={{ fontSize: 12 }}>{name}</Typography.Text>
                  </Space>
                )
              },
            },
            {
              // "Impact", not "verdict": the question is what this change does
              // to whoever consumes the release, not what the comparison ruled.
              title: 'Impact',
              width: 120,
              render: (_, r) => (
                <StatusPill tone={VERDICT[r.verdict]?.tone ?? 'neutral'} style={{ marginInlineEnd: 0 }}>
                  {VERDICT[r.verdict]?.label ?? r.verdict}
                </StatusPill>
              ),
            },
            {
              title: 'Size',
              width: 170,
              align: 'right',
              render: (_, r) => <SizeCell row={r} />,
            },
            {
              // The tag, before and after, in a column of its own.
              //
              // A component's tag is what a consumer pulls, and a release that
              // moved a component from 1.2.3 to 1.2.4 changed the thing anybody
              // will actually type. It was reachable only by expanding a row,
              // which is three clicks to learn something the row had space for.
              title: 'Tag',
              width: 190,
              render: (_, r) => <TagCell row={r} />,
            },
            {
              // The digests, which are the only unambiguous statement of what
              // changed - and for a `changed` row, both of them, because "it
              // changed" without saying from what to what is not a finding
              // anybody can act on.
              title: 'Digest',
              width: 220,
              render: (_, r) => <DigestCell row={r} />,
            },
          ]}
        />
        )}
      </Card>
    </Space>
  )
}

/**
 * WHICH FILES changed, as a directory tree.
 *
 * # Why this is the answer and the component table is not
 *
 * "cfx-5000-product/k8s changed" says a bundle of four hundred configuration
 * files is not byte-identical, which anybody could have guessed from the
 * version number. The question is which of the four hundred, and it is
 * answerable exactly: an OCI artifact names one file per layer and states its
 * content digest, so the two manifests already hold it.
 *
 * # Why a tree and not a list
 *
 * The paths are hierarchical because the publisher wrote them that way, and a
 * flat list of four hundred rows answers "did the network configuration change"
 * by making somebody read all of them. The shape is the vendor's.
 *
 * Files from every component are merged into ONE tree, which is what makes it
 * read like a release rather than like a table of manifests. Where two
 * components carry the same path they are separate leaves, and the component is
 * named on the row.
 */
function FileDifferences({ files, total }: { files: FileEntry[]; total: number }) {
  const tree = useMemo(() => buildFileTree(files), [files])

  if (total === 0) {
    return (
      <Typography.Text type="secondary" style={{ fontSize: 13 }}>
        Nothing here names a file. A component's files are the layers its
        publisher titled - every configuration bundle, release note and script
        shipped as an OCI artifact carries them - and an image layer names
        nothing, so a comparison of images alone has no file-level account to
        give.
      </Typography.Text>
    )
  }

  if (files.length === 0) {
    return <EmptyState title="No file matches this filter" />
  }

  return (
    <Tree
      treeData={tree}
      showLine={{ showLeafIcon: false }}
      // Open where the reader can take it in, collapsed where they cannot: a
      // release that rewrote four hundred files should not render four hundred
      // rows before anybody has asked for one.
      defaultExpandAll={files.length <= 80}
      selectable={false}
      style={{ maxHeight: 520, overflow: 'auto' }}
    />
  )
}

/** One file, and which component it came from. */
interface FileEntry extends CompareFile {
  component: string
}

interface FileNode {
  key: string
  title: ReactNode
  children?: FileNode[]
}

/**
 * Paths into a tree, directories before files and both alphabetical.
 *
 * A directory says how many files under it DIFFER, which is the number that
 * decides whether it is worth opening.
 */
function buildFileTree(files: FileEntry[]): FileNode[] {
  interface Dir {
    dirs: Map<string, Dir>
    files: { entry: FileEntry; name: string }[]
    differing: number
  }
  const root: Dir = { dirs: new Map(), files: [], differing: 0 }

  for (const entry of files) {
    const parts = entry.path.split('/').filter(Boolean)
    const name = parts.pop() ?? entry.path
    const differs = entry.verdict !== 'same' ? 1 : 0

    let node = root
    node.differing += differs
    for (const part of parts) {
      let next = node.dirs.get(part)
      if (!next) {
        next = { dirs: new Map(), files: [], differing: 0 }
        node.dirs.set(part, next)
      }
      node = next
      node.differing += differs
    }
    node.files.push({ entry, name })
  }

  const render = (dir: Dir, prefix: string): FileNode[] => {
    const out: FileNode[] = []
    for (const [name, child] of [...dir.dirs.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
      out.push({
        key: `${prefix}/${name}`,
        title: (
          <Space size={6}>
            <FolderOutlined style={{ color: c.text3 }} />
            <Typography.Text style={{ fontSize: 13 }}>{name}</Typography.Text>
            {child.differing > 0 && (
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                {child.differing} changed
              </Typography.Text>
            )}
          </Space>
        ),
        children: render(child, `${prefix}/${name}`),
      })
    }
    for (const leaf of [...dir.files].sort((a, b) => a.name.localeCompare(b.name))) {
      out.push({
        key: `${prefix}/${leaf.name}-${leaf.entry.component}-${leaf.entry.digestA ?? ''}${leaf.entry.digestB ?? ''}`,
        title: <FileRow entry={leaf.entry} name={leaf.name} />,
      })
    }
    return out
  }

  return render(root, '')
}

/** One file: what happened to it, what it weighs, and what it hashes to. */
function FileRow({ entry, name }: { entry: FileEntry; name: string }) {
  const meta = VERDICT[entry.verdict]
  const before = bytes(entry.sizeA)
  const after = bytes(entry.sizeB)

  return (
    <Space size={8} style={{ flexWrap: 'wrap' }}>
      <Icon as={ARTIFACT_ICONS.Files} size={13} title="File" />
      <Typography.Text
        style={{
          fontSize: 13,
          // Struck through where it is gone. A removed file read identically
          // to a present one with a tag beside it, and the tag is the part
          // somebody scanning a list of two hundred does not read.
          textDecoration: entry.verdict === 'only-a' ? 'line-through' : undefined,
        }}
      >
        {name}
      </Typography.Text>

      {entry.verdict !== 'same' && (
        <StatusPill tone={meta?.tone ?? 'neutral'} size="sm" style={{ marginInlineEnd: 0 }}>
          {meta?.label}
        </StatusPill>
      )}

      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {entry.verdict === 'changed' && before !== null && after !== null && before !== after
          ? `${formatBytes(before)} → ${formatBytes(after)}`
          : formatBytes(after ?? before)}
      </Typography.Text>

      {/*
        The digests, on hover. They are the proof - two files with the same
        digest ARE the same file - and they are also forty characters that
        would bury the name they belong to.
      */}
      <Tooltip
        title={
          <Space direction="vertical" size={2}>
            <span style={{ fontSize: 11 }}>{entry.component}</span>
            {entry.digestA && <span style={{ fontSize: 11 }}>before {entry.digestA}</span>}
            {entry.digestB && <span style={{ fontSize: 11 }}>after&nbsp; {entry.digestB}</span>}
          </Space>
        }
      >
        <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 10 }}>
          {(entry.digestB || entry.digestA || '').slice(7, 19)}
        </Typography.Text>
      </Tooltip>
    </Space>
  )
}

/**
 * WHAT THE RELEASE IS MADE OF, as one band.
 *
 * This was four cards - Added, Removed, Changed, Unchanged - each with a big
 * number and a byte count under it. On an ordinary point release three of the
 * four read zero, so three quarters of the widest row on the page was spent
 * drawing 38px zeroes, and the one figure that mattered had no more weight than
 * the three that did not.
 *
 * A bar makes the shape of the release legible instead: a wide "unchanged"
 * segment is a point release, a wide "changed" segment is a rebuild, and an
 * "added" segment is a new component somebody has to go and look at. The four
 * counts stay, underneath, at a size proportional to how often they are not
 * zero - and each is still the popover it always was, naming what is inside it.
 *
 * The same shape as the vulnerability comparison's set bar, deliberately: two
 * views of one comparison should not speak two visual languages.
 */
function CompositionBand({ buckets }: { buckets: Record<CompareVerdict, Bucket> }) {
  const order = ['only-b', 'changed', 'same', 'only-a'] as const
  const total = order.reduce((n, v) => n + buckets[v].count, 0) || 1

  return (
    <Card size="small" styles={{ body: { padding: '18px 22px' } }}>
      <div
        style={{
          display: 'flex', width: '100%', height: 12, borderRadius: 6,
          overflow: 'hidden', background: c.track,
        }}
      >
        {order.map((verdict, i) => {
          const n = buckets[verdict].count
          if (!n) return null
          return (
            <Tooltip
              key={verdict}
              title={`${VERDICT[verdict].label}: ${formatCount(n)} · ${formatBytes(buckets[verdict].bytes) ?? 'size unknown'}`}
            >
              <div
                className="slm-meter-seg"
                style={{
                  width: `${(n / total) * 100}%`,
                  background: BAND_COLOUR[verdict],
                  transformOrigin: 'left',
                  animation: `slm-grow 460ms cubic-bezier(0.16,1,0.3,1) ${i * 60}ms both`,
                }}
              />
            </Tooltip>
          )
        })}
      </div>

      <div
        style={{
          display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))',
          gap: 16, marginTop: 14,
        }}
      >
        {order.map((verdict) => (
          <BucketFigure key={verdict} bucket={buckets[verdict]} verdict={verdict} />
        ))}
      </div>
    </Card>
  )
}

/** The segment colours, matched to the verdict vocabulary already in use. */
const BAND_COLOUR: Record<CompareVerdict, string> = {
  'only-b': c.ok,
  changed: c.pending,
  same: c.text3,
  'only-a': c.danger,
}

/**
 * One bucket's count, its weight and what it is made of.
 *
 * A zero is drawn quietly rather than dropped: "nothing was removed" is a fact
 * a release manager wants stated, and a row that omits it reads as a row that
 * forgot to check.
 */
function BucketFigure({ bucket, verdict }: { bucket: Bucket; verdict: CompareVerdict }) {
  const meta = VERDICT[verdict]
  const kinds = Object.entries(bucket.byKind)
    .filter(([, v]) => v.count > 0)
    .sort((a, b) => b[1].count - a[1].count)
  const empty = bucket.count === 0

  return (
    <Popover
      placement="bottom"
      title={`${meta.label} - what it is made of`}
      content={
        kinds.length === 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            Nothing in this bucket.
          </Typography.Text>
        ) : (
          <Space direction="vertical" size={4} style={{ minWidth: 220 }}>
            {kinds.map(([kind, v]) => {
              const name = kindName(kind)
              const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]
              return (
                <Space key={kind} size={8} style={{ width: '100%', justifyContent: 'space-between' }}>
                  <Space size={6}>
                    {icon && <Icon as={icon} size={14} title={name} />}
                    <Typography.Text style={{ fontSize: 12 }}>{name}</Typography.Text>
                  </Space>
                  <Typography.Text style={{ fontSize: 12 }}>
                    {formatCount(v.count)} · <Value>{formatBytes(v.bytes)}</Value>
                  </Typography.Text>
                </Space>
              )
            })}
          </Space>
        )
      }
    >
      <div style={{ minWidth: 0 }}>
        <div
          style={{
            display: 'flex', alignItems: 'baseline', gap: 7,
            fontVariantNumeric: 'tabular-nums',
          }}
        >
          <span
            aria-hidden
            style={{
              width: 8, height: 8, borderRadius: 2, flex: 'none',
              background: empty ? c.borderStrong : BAND_COLOUR[verdict],
            }}
          />
          <span
            style={{
              fontSize: 22, fontWeight: 600, lineHeight: 1, letterSpacing: '-0.02em',
              color: empty ? c.text2 : c.text,
            }}
          >
            {formatCount(bucket.count)}
          </span>
          <span style={{ fontSize: 12.5, color: c.text2 }}>{meta.label.toLowerCase()}</span>
        </div>
        {!empty && (
          <Typography.Text
            type="secondary"
            style={{ fontSize: 11.5, display: 'block', marginTop: 5, marginInlineStart: 15 }}
          >
            <Value reason="Nothing in this bucket has a size we could read.">
              {formatBytes(bucket.bytes)}
            </Value>
          </Typography.Text>
        )}
      </div>
    </Popover>
  )
}

/** What a component weighs, and what it weighed before. */
function SizeCell({ row }: { row: CompareRow }) {
  const a = bytes(row.a?.size)
  const b = bytes(row.b?.size)

  if (row.verdict === 'changed' && a !== undefined && b !== undefined && a !== b) {
    const delta = b - a
    return (
      <Space direction="vertical" size={0}>
        <Typography.Text style={{ fontSize: 12 }}>{formatBytes(b)}</Typography.Text>
        <Typography.Text
          type={delta > 0 ? 'warning' : 'success'}
          style={{ fontSize: 11 }}
        >
          {delta > 0 ? '+' : '−'}{formatBytes(Math.abs(delta))} from {formatBytes(a)}
        </Typography.Text>
      </Space>
    )
  }
  return <Value>{formatBytes(b ?? a)}</Value>
}

/**
 * What this component is CALLED, before and after.
 *
 * The digest says whether it changed; the tag says what a consumer pulls. Both
 * belong in the row: a release that moved a component from 1.2.3 to 1.2.4 has
 * changed the thing anybody will actually type, and a row that showed only
 * digests made that invisible without expanding it.
 *
 * Unchanged rows show the one tag, plainly. Both sides carrying the same tag is
 * the ordinary case and printing it twice would be noise.
 */
function TagCell({ row }: { row: CompareRow }) {
  const a = row.a?.tag
  const b = row.b?.tag

  if (a && b && a !== b) {
    return (
      <Space direction="vertical" size={0}>
        <Typography.Text delete type="secondary" style={{ fontFamily: mono, fontSize: 11 }}>
          {a}
        </Typography.Text>
        <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{b}</Typography.Text>
      </Space>
    )
  }
  const tag = b ?? a
  return tag
    ? <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{tag}</Typography.Text>
    : <NA reason="This component answers to no tag of its own; it is addressed by digest." />
}

/**
 * The digests, short enough to read and complete enough to act on.
 *
 * A `changed` row shows both: "it changed" without saying from what to what is
 * not something anybody can take to a vendor. Added and removed show the one
 * digest that exists, labelled by which side it is on.
 */
function DigestCell({ row }: { row: CompareRow }) {
  const line = (label: string, digest: string | undefined, colour?: string) =>
    digest ? (
      <Tooltip title={digest} key={label}>
        <Typography.Text style={{ fontSize: 11 }} type={colour as never}>
          <Typography.Text type="secondary" style={{ fontSize: 10 }}>{label} </Typography.Text>
          <span style={{ fontFamily: mono }}>{shortDigest(digest)}</span>
        </Typography.Text>
      </Tooltip>
    ) : null

  switch (row.verdict) {
    case 'changed':
      return (
        <Space direction="vertical" size={0}>
          {line('was', row.a?.digest)}
          {line('now', row.b?.digest)}
        </Space>
      )
    case 'only-a':
      return line('removed', row.a?.digest) ?? <NA />
    case 'only-b':
      return line('added', row.b?.digest) ?? <NA />
    default:
      return line('both', row.a?.digest ?? row.b?.digest) ?? <NA />
  }
}

/** One bucket's totals, and the same totals per kind. */
interface Bucket {
  count: number
  bytes: number
  byKind: Record<string, { count: number; bytes: number }>
}

/**
 * Counts and sizes per bucket, and per kind within each bucket.
 *
 * The side each bucket is measured from is the side it EXISTS on: a removed
 * component's size is what the first side had, an added one's is what the
 * second has, and a changed one is measured by what it is now - the size of
 * what somebody would be shipping.
 */
function summarise(rows: CompareRow[]): Record<CompareVerdict, Bucket> {
  const empty = (): Bucket => ({ count: 0, bytes: 0, byKind: {} })
  const out: Record<CompareVerdict, Bucket> = {
    same: empty(), changed: empty(), 'only-a': empty(), 'only-b': empty(),
  }

  for (const row of rows) {
    const bucket = out[row.verdict]
    if (!bucket) continue
    const size = row.verdict === 'only-a'
      ? bytes(row.a?.size) ?? 0
      : bytes(row.b?.size) ?? bytes(row.a?.size) ?? 0

    bucket.count += 1
    bucket.bytes += size
    const kind = bucket.byKind[row.type] ?? { count: 0, bytes: 0 }
    kind.count += 1
    kind.bytes += size
    bucket.byKind[row.type] = kind
  }
  return out
}

/** sha256:abcd… - enough to recognise, short enough for a column. */
function shortDigest(digest: string): string {
  const hex = digest.includes(':') ? digest.split(':')[1] ?? '' : digest
  return hex.slice(0, 12)
}

export default function Compare() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const { message } = App.useApp()

  const product = params.get('product') ?? undefined
  // The pair, as the listing wrote it. `from` is the old link's spelling and is
  // read so a bookmark made before the selection moved still lands somewhere
  // sensible rather than on an empty page.
  const leftRef = params.get('a') ?? params.get('from') ?? undefined
  const rightRef = params.get('b') ?? undefined
  const mode: Mode = params.get('mode') === 'locations' ? 'locations' : 'versions'

  const products = useProducts()
  const detail = useProduct(product)
  const packages = usePackages(product, { pageSize: 200 })
  const releases = packages.data?.packages ?? []

  /*
   * A page with no pair has nothing to show and no way to ask for one.
   *
   * Sent back to the listing in selection mode rather than rendering an empty
   * state with a link in it: the reader's next action is the same either way,
   * and one of the two spellings of it costs them a click.
   */
  const incomplete = mode === 'versions' && !(product && leftRef && rightRef)
  useEffect(() => {
    if (!incomplete) return
    const q = new URLSearchParams({ compare: '1' })
    if (product) q.set('cmp', product)
    if (leftRef) q.set('a', leftRef)
    navigate(`/packages?${q.toString()}`, { replace: true })
  }, [incomplete, product, leftRef, navigate])

  const leftPkg = releases.find((r) => refOf(r) === leftRef)
  const rightPkg = releases.find((r) => refOf(r) === rightRef)

  /*
   * The report opens on the answer that was ASKED for.
   *
   * The intent was chosen on the listing, before the two releases - see
   * domain/compare.ts - and carrying it here is what stops the reader choosing
   * it twice, once to narrow the list and again on the tab strip.
   */
  const [view, setView] = useState<View>(
    params.get('view') === 'security' ? 'security' : 'package',
  )

  /*
   * Whether the vulnerability answer can be given at all.
   *
   * BOTH ends, because a comparison is a difference and a difference needs two
   * sides. One release scanned and the other not produces a verdict where every
   * finding reads as introduced - which is not a fact about the release, it is
   * a fact about what nobody scanned, and it is the most dangerous thing this
   * page could say wrongly.
   *
   * Absent while the listing loads, which is why this is not a plain boolean:
   * disabling the tab because the data has not arrived yet, and then enabling
   * it a second later, is a control that flickers.
   */
  const unscanned = useMemo(() => {
    if (!leftPkg || !rightPkg) return undefined
    return [leftPkg, rightPkg].filter((p) => !hasSecurityData(p))
  }, [leftPkg, rightPkg])
  const securityBlocked = Boolean(unscanned && unscanned.length > 0)

  /*
   * A link asking for the security view of a pair that cannot answer falls back
   * rather than erroring.
   *
   * That happens for an honest reason - a bookmark from before a sync was
   * cleared - and the useful response is the comparison that CAN be given, with
   * the other tab disabled and saying why.
   */
  useEffect(() => {
    if (securityBlocked && view === 'security') setView('package')
  }, [securityBlocked, view])

  const sources = detail.data?.sources ?? []
  // Where the release was found, matched back to its configured source. The
  // API needs an endpoint NAME and a package records a repository PATH, and a
  // product with several sources refuses to guess between them.
  const discoveredIn = sourceNameFor(sources, leftPkg?.sourceRepository)
  const [sourceOverride, setSourceOverride] = useState<string>()
  const versionEnd = sourceOverride ?? discoveredIn ?? sources[0]?.name
  // Asked ONLY when it genuinely cannot be worked out: several sources, and
  // none of them claiming this release's repository. One select that appears
  // for the products that need it beats a fourth control on every comparison.
  const ambiguousSource = mode === 'versions' && sources.length > 1 && !discoveredIn

  const [fromEndpoint, setFromEndpoint] = useState<string>()
  const [toEndpoint, setToEndpoint] = useState<string>()

  const [startedAt, setStartedAt] = useState<number>()
  const [elapsed, setElapsed] = useState(0)
  // Minted here, per run: the comparison's response IS the report, so there is
  // nothing for the server to hand an id back in. Sending one is what makes
  // progress readable while the request is open.
  const [token, setToken] = useState<string>()

  const compare = useCompare()
  const compareRunning = compare.isPending
  const report = compare.data

  /*
   * The comparison runs from `enabled`, not from a click or an effect: asking
   * for the vulnerability view IS asking for the comparison, and a reader who
   * arrives on that view from the listing should not have to ask twice. It is
   * keyed on the pair, so switching back to contents and returning is free.
   */
  const compareSecurity = useCompareSecurity(
    {
      product,
      ref: leftRef,
      repository: leftPkg?.sourceRepository,
      against: rightPkg?.tag ?? rightRef,
    },
    // Not until the listing has answered. Both the repository and the tag this
    // comparison is keyed on come from the resolved pair, so running before it
    // arrives runs the comparison TWICE - once against the raw refs and again
    // against the resolved ones - and the first of those is three seconds of a
    // hundred and seventy thousand findings that nothing will ever read.
    view === 'security' && !securityBlocked && !packages.isPending,
  )
  const syncSecurity = useSyncPackageSecurity()

  useEffect(() => {
    if (!compareRunning || !startedAt) return
    const id = setInterval(() => setElapsed((Date.now() - startedAt) / 1000), 500)
    return () => clearInterval(id)
  }, [compareRunning, startedAt])

  const run = useCallback(async () => {
    if (!product || !leftRef) return
    setStartedAt(Date.now())
    setElapsed(0)
    const progressToken = crypto.randomUUID()
    setToken(progressToken)
    try {
      await compare.mutateAsync({
        product,
        ref: leftRef,
        repository: leftPkg?.sourceRepository,
        body: {
          // A version comparison names the other release; a location
          // comparison names the other place. A version comparison still has
          // to NAME its end - a product with several sources will not guess -
          // and both sides of one are the same place.
          against: mode === 'versions' ? rightRef : undefined,
          from: mode === 'versions' ? versionEnd : (fromEndpoint || undefined),
          to: mode === 'versions' ? versionEnd : (toEndpoint || undefined),
          progressToken,
        },
      })
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The comparison could not be run.')
    }
  }, [product, leftRef, rightRef, leftPkg, mode, versionEnd, fromEndpoint, toEndpoint, compare, message])

  /*
   * Runs itself on arrival, and runs only what was ASKED for.
   *
   * The pair was chosen on the previous page and confirmed with a button called
   * Compare, so a second button here saying the same word would be asking the
   * reader to agree with themselves.
   *
   * Which comparison runs follows the view, and that matters because the two
   * cost wildly different things. The contents comparison walks two manifest
   * trees against their registries and takes minutes; the vulnerability one is
   * two indexed reads of what a sync already stored. Somebody who asked about
   * vulnerabilities should not wait on a registry walk they did not ask for -
   * and if they switch tabs afterwards, the other one starts then.
   *
   * The key guards against React's development double-mount and against the
   * effect firing again when an unrelated piece of state moves.
   */
  const ranFor = useRef<string>('')
  useEffect(() => {
    if (mode !== 'versions' || view !== 'package' || !product || !leftRef || !rightRef) return
    // The source is part of the request, so a comparison must not start until
    // it is known - the packages and the product load separately.
    if (sources.length > 0 && !versionEnd) return
    const key = `${product}|${leftRef}|${rightRef}|${versionEnd ?? ''}`
    if (ranFor.current === key) return
    ranFor.current = key
    void run()
  }, [mode, view, product, leftRef, rightRef, versionEnd, sources.length, run])

  /**
   * Sync one end from the comparison, then re-run it.
   *
   * A comparison against a release nobody scanned is inconclusive, and the
   * useful thing to offer on that screen is the sync - not a verdict repeating
   * that it cannot say. The re-run is deliberate rather than automatic: a sync
   * is minutes, and a page that silently re-compared would look stuck.
   */
  /*
   * Fetch what is missing, then carry on - without the reader doing anything
   * else.
   *
   * It used to say "re-run the comparison once it finishes", which is a task
   * handed back to somebody who had already asked for the thing. The sync now
   * only asks the scanner about images it has no answer for, so filling a gap
   * of one or two images is seconds rather than minutes; this watches the
   * release until the run settles and re-runs the comparison itself.
   */
  const [syncingEnd, setSyncingEnd] = useState<'a' | 'b' | null>(null)
  const watched = syncingEnd === 'a' ? leftPkg : syncingEnd === 'b' ? rightPkg : undefined
  const watchedSecurity = usePackageSecurity(
    product,
    watched ? packageReference(watched) : undefined,
    { repository: watched?.sourceRepository, enabled: Boolean(watched) },
  )
  const watchedState = watchedSecurity.data?.sync.state
  useEffect(() => {
    if (!syncingEnd || !watchedSecurity.data) return
    if (watchedState === 'syncing' && !watchedSecurity.data.sync.stalled) return
    setSyncingEnd(null)
    void compareSecurity.refetch()
  }, [syncingEnd, watchedState, watchedSecurity.data, compareSecurity])

  const syncEnd = (end: 'a' | 'b') => {
    const target = end === 'a' ? leftPkg : rightPkg
    if (!product || !target) return
    syncSecurity.mutate({
      product,
      ref: packageReference(target),
      repository: target.sourceRepository,
    }, {
      onSuccess: (res) => {
        setSyncingEnd(end)
        message.info(res.started
          ? `Fetching the missing results for ${target.tag}. The comparison re-runs on its own.`
          : `A sync is already running for ${target.tag}. The comparison re-runs when it finishes.`)
      },
      onError: (e) => message.error(e instanceof Error ? e.message : 'The sync could not be started.'),
    })
  }

  const showSecurity = () => setView('security')

  /** Back to where choosing happens, with this pair still ticked. */
  const changeSelection = () => {
    navigate(selectionHref({
      active: true,
      // Back into the mode the reader arrived in, so returning to change one
      // end does not silently re-broaden the list they were choosing from.
      intent: view === 'security' ? 'vulnerabilities' : 'contents',
      a: product && leftRef ? { product, ref: leftRef } : undefined,
      b: product && rightRef ? { product, ref: rightRef } : undefined,
    }))
  }

  const endpoints = [
    { value: '', label: 'Vendor (where it was discovered)' },
    ...(detail.data?.targets ?? []).map((t) => ({
      value: t.name,
      label: `${t.name}${t.environment ? ` · ${t.environment}` : ''}`,
    })),
  ]

  if (products.isError) {
    return <ErrorState error={products.error} retry={() => void products.refetch()} />
  }
  // Mid-redirect. Rendering the empty shell would flash a page that is about to
  // be replaced.
  if (incomplete) return null

  return (
    <>
      {/*
        ONE HEADER, and it STATES the comparison rather than asking for it.

        What is being compared, which answer is on screen, and how to change the
        pair. The ends are named in full - each one's package and then its
        version - because a product publishes nine differently-named packages
        that share a version string, and the two ends are frequently different
        packages.
      */}
      <Card size="small" style={{ marginBottom: 16 }} styles={{ body: { padding: 0 } }}>
        <Space
          size={16}
          wrap
          style={{ width: '100%', justifyContent: 'space-between', padding: '12px 16px' }}
        >
          <Space size={12} wrap align="center">
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>Comparing</Typography.Text>
            <ComparedEnd pkg={leftPkg} fallback={report?.a.label ?? leftRef ?? ''} />
            <SwapOutlined style={{ color: c.text3 }} />
            {mode === 'versions'
              ? <ComparedEnd pkg={rightPkg} fallback={report?.b.label ?? rightRef ?? ''} />
              : (
                <Typography.Text strong style={{ fontFamily: mono, fontSize: 13 }}>
                  {toEndpoint || 'the vendor'}
                </Typography.Text>
              )}
          </Space>

          <Space size={8} wrap>
            {ambiguousSource && (
              // Only where the release's repository matches no configured
              // source. Labelled, because "cfx-near" in a bare select beside
              // two release names reads as a third end of the comparison.
              <Space size={6}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>Read from</Typography.Text>
                <Select
                  size="small"
                  style={{ minWidth: 160 }}
                  value={versionEnd}
                  onChange={setSourceOverride}
                  options={sources.map((src) => ({ value: src.name, label: src.name }))}
                />
              </Space>
            )}
            {mode === 'versions' ? (
              <Button onClick={changeSelection}>Change selection</Button>
            ) : (
              <Link to="/packages"><Button>Back to packages</Button></Link>
            )}
          </Space>
        </Space>

        {mode === 'locations' && (
          /*
            The one form that survives, and it is two controls about a release
            that is already named rather than four about one that is not.
          */
          <div
            style={{
              display: 'flex', flexWrap: 'wrap', gap: 12, alignItems: 'center',
              padding: '10px 16px', borderTop: `1px solid ${c.border}`,
            }}
          >
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>From</Typography.Text>
            <Select
              size="small"
              style={{ minWidth: 220 }}
              value={fromEndpoint ?? ''}
              onChange={setFromEndpoint}
              options={endpoints}
            />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>to</Typography.Text>
            <Select
              size="small"
              style={{ minWidth: 220 }}
              value={toEndpoint ?? ''}
              onChange={setToEndpoint}
              options={endpoints}
            />
            <Button
              size="small"
              type="primary"
              loading={compare.isPending}
              disabled={toEndpoint === undefined}
              onClick={() => void run()}
            >
              Compare
            </Button>
          </div>
        )}

        {/*
          The tab strip renders before either answer does. It used to wait for
          the contents report, which was fine while that always ran - and became
          a page with no way to switch views the moment it stopped.
        */}
        {mode === 'versions' && (
          <div
            style={{
              display: 'flex', flexWrap: 'wrap', gap: 12, alignItems: 'center',
              justifyContent: 'space-between',
              padding: '10px 16px', borderTop: `1px solid ${c.border}`,
            }}
          >
            {/*
              The unavailable answer is DISABLED and says why, rather than being
              offered and then refusing. A tab that produces a verdict saying it
              cannot say has spent the reader's click to tell them something the
              tab strip could have.
            */}
            <Segmented
              value={view}
              onChange={(v) => (v === 'security' ? showSecurity() : setView('package'))}
              options={[
                { value: 'package', label: 'Contents' },
                {
                  value: 'security',
                  label: securityBlocked ? (
                    <Tooltip title={unscannedReason(unscanned)}>
                      <span>Vulnerabilities</span>
                    </Tooltip>
                  ) : 'Vulnerabilities',
                  disabled: securityBlocked,
                },
              ]}
            />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {securityBlocked
                ? unscannedReason(unscanned)
                : view === 'package'
                  ? 'What the two releases hold, component by component.'
                  : 'How the security posture changed from the base release to the new one.'}
            </Typography.Text>
          </div>
        )}
      </Card>

      {compareRunning && (
        <Card size="small" style={{ marginBottom: 16 }}>
          <ComparisonProgress
            token={token}
            elapsedSeconds={elapsed}
            base={leftPkg}
            against={rightPkg}
          />
        </Card>
      )}

      {compare.isError && <ErrorState error={compare.error} retry={() => void run()} />}

      {report && view === 'package' && <ComparisonReport report={report} />}

      {view === 'security' && (
        <>
          {/*
            A comparison over stored data is two indexed reads, so there is no
            position worth reporting - a plain loading card is the honest shape
            and a progress bar would be theatre.

            Shown while there is no answer yet, which covers both the request
            being in flight and the moment before it can start - the pair has
            to be resolved from the listing first, and a gap of blank page
            between arriving and asking reads as a page that gave up.
          */}
          {!compareSecurity.data && !compareSecurity.isError && <Card loading />}
          {compareSecurity.isError && !compareSecurity.isFetching && (
            <ErrorState error={compareSecurity.error} retry={() => void compareSecurity.refetch()} />
          )}
          {compareSecurity.data && !compareSecurity.isFetching && product && leftRef && (
            <SecurityComparison
              product={product}
              baseRef={leftRef}
              againstRef={rightPkg?.tag ?? rightRef ?? ''}
              repository={leftPkg?.sourceRepository}
              report={compareSecurity.data}
              onSync={syncEnd}
              syncing={Boolean(syncingEnd) || syncSecurity.isPending}
            />
          )}
        </>
      )}
    </>
  )
}
