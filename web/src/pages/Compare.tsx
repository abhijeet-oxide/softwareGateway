import { useEffect, useMemo, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import { App, Button, Card, Col, Popover, Progress, Row, Segmented, Select, Space, Tag, Tooltip, Tree, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { FolderOutlined, SwapOutlined } from '../icons'
import { useSearchParams } from 'react-router-dom'
import {
  useCompare, useCompareProgress, useCompareSecurity, usePackages, useProduct, useProducts,
  useSyncPackageSecurity,
} from '../api/queries'
import { kindName, matches, packageReference, version } from '../domain/derive'
import { bytes, formatBytes, formatCount, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { ErrorState, SearchBar } from '../components/layout'
import { WorkingBar } from '../components/progress'
import { ARTIFACT_ICONS, Icon } from '../components/icons'
import { SecurityComparison } from '../components/securitycompare'
import { c, EmptyState, mono } from '../uikit'
import type {
  CompareFile, CompareProgressSide, CompareResponse, CompareRow, CompareVerdict, Package,
  Repository,
} from '../api/types'

/**
 * Page 5 - Compare.
 *
 * Answers: what is different between these two, exactly?
 *
 * # The shape of the question
 *
 * A comparison has two axes and the API takes them separately: WHICH VERSION
 * (`against`) and WHERE (`from` and `to`). Naming only a version compares two
 * releases in one place; naming only a place compares one release in two;
 * naming both answers each at once. The form is arranged the same way, so what
 * is being asked stays legible as it is assembled.
 *
 * # Why the mode selector exists
 *
 * Most comparisons are one of two questions - "what changed between these
 * releases" and "did this release arrive intact" - and each needs only half
 * the form. Offering four selectors for both made the common case look like
 * the hard case.
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

/** The option list for a release select: the name, the version, and both searchable. */
function releaseOptions(releases: Package[]) {
  return releases.map((r) => ({
    value: refOf(r),
    // The VERSION alone, because a select box is one line and the version is
    // the half that identifies the release to a reader. The package name is
    // rendered under it - in the list and in the closed box - where a second
    // line has room for it. Joining the two into one string produced
    // `cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wn…`, which is neither.
    label: version(r),
    name: r.displayRepository || r.sourceRepository || '',
    version: version(r),
    tag: r.tag,
  }))
}

type ReleaseOption = ReturnType<typeof releaseOptions>[number]

/**
 * Version above, package name below - the two things that identify a release.
 *
 * Used for the options AND for the closed box, so what a reader picked still
 * says which package it was. A select showing only a version is ambiguous by
 * construction here: the same tag exists in ten repositories.
 */
function renderReleaseOption(option: Pick<ReleaseOption, 'version' | 'name'>) {
  /*
   * PLAIN DIVS, and every one of them bounded.
   *
   * These two lines go inside the select's closed box, which is a fixed width
   * with `overflow: hidden` on ONE line of text. A flex container of unbounded
   * children ignores that entirely, and a vendor reference like
   * `cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wnv5a0csd0003c-ncm` ran
   * straight out of the box and across the page.
   *
   * `minWidth: 0` on the wrapper is the part that is easy to leave out and
   * without which none of the rest works: a block child's default minimum size
   * is its content, so the ellipsis has nothing to clip against.
   */
  const line: CSSProperties = {
    overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
  }

  return (
    <div style={{ minWidth: 0, maxWidth: '100%', overflow: 'hidden' }}>
      <div style={{ ...line, fontFamily: mono, fontSize: 12, lineHeight: '16px' }}>
        {option.version}
      </div>
      {option.name && (
        <div
          style={{ ...line, fontSize: 11, lineHeight: '14px', color: c.text2 }}
          title={option.name}
        >
          {option.name}
        </div>
      )}
    </div>
  )
}

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

/** The closed box, showing the same two lines as the option that filled it. */
function releaseLabelRender(releases: Package[], value: unknown, fallback: ReactNode) {
  const chosen = releases.find((r) => refOf(r) === value)
  if (!chosen) return fallback
  return renderReleaseOption({
    version: version(chosen),
    name: chosen.displayRepository || chosen.sourceRepository || '',
  })
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

const VERDICT: Record<CompareVerdict, { label: string; colour: string; statColour?: string }> = {
  same: { label: 'Unchanged', colour: 'default' },
  changed: { label: 'Changed', colour: 'orange', statColour: c.pending },
  'only-a': { label: 'Removed', colour: 'red', statColour: c.danger },
  'only-b': { label: 'Added', colour: 'green', statColour: c.ok },
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
          kind === 'file'
            ? `Files (${formatCount(fileCount)})`
            : `Components (${formatCount(content.length)})`
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
        <div style={{ padding: '12px 16px 0' }}>
          <SearchBar
            value={search}
            onChange={setSearch}
            placeholder={kind === 'file'
              ? 'Search files by path or component'
              : 'Search by name, tag or digest'}
            matched={kind === 'file' ? shownFiles.length : rows.length}
            total={kind === 'file' ? fileCount : content.length}
            width={340}
          />
        </div>

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
                <Tag color={VERDICT[r.verdict]?.colour} style={{ marginInlineEnd: 0 }}>
                  {VERDICT[r.verdict]?.label ?? r.verdict}
                </Tag>
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
        <Tag color={meta?.colour} style={{ marginInlineEnd: 0 }}>{meta?.label}</Tag>
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
          overflow: 'hidden', background: c.surface2,
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
  const [params, setParams] = useSearchParams()
  const { message } = App.useApp()

  const products = useProducts()
  const productList = products.data?.products ?? []
  const product = params.get('product') ?? productList[0]?.productId

  const detail = useProduct(product)
  const packages = usePackages(product, { pageSize: 200 })
  const releases = packages.data?.packages ?? []

  const [mode, setMode] = useState<Mode>('versions')
  /*
   * The incoming link carries a TAG, and a tag is not a reference.
   *
   * One version tag exists in every repository a product watches, so `?from=`
   * on its own does not name a package - and a select keyed on
   * `repository:tag` had nothing matching it, which is why the box showed a
   * bare version with no package name beside it. The repository travels in the
   * URL now; the effect below resolves the pair, and falls back to the first
   * release with that tag for a link written before it did.
   */
  const [left, setLeft] = useState<string | undefined>(undefined)
  const [right, setRight] = useState<string>()
  const [fromEndpoint, setFromEndpoint] = useState<string>()
  const [toEndpoint, setToEndpoint] = useState<string>()
  const [sourceOverride, setSourceOverride] = useState<string>()
  // Collapsed once there is a report. The form is scaffolding for the answer:
  // leaving four selectors above it pushes the thing somebody waited minutes
  // for below the fold, and every one of those selectors is now describing a
  // comparison that has already been run.
  const [settled, setSettled] = useState(false)
  // Elapsed while it runs. Both releases are read live from their registries,
  // so a comparison takes as long as those registries take - and a button that
  // says `loading` for four minutes with nothing beside it is the shape of
  // something broken.
  const [startedAt, setStartedAt] = useState<number>()
  const [elapsed, setElapsed] = useState(0)
  // Minted here, per run: the comparison's response is the report, so there is
  // nothing for the server to hand an id back in. Sending one is what makes
  // progress readable while the request is open.
  const [token, setToken] = useState<string>()
  // The first release is almost always the one somebody means, and having to
  // pick it before the page does anything is a step with no decision in it.
  useEffect(() => {
    if (left || releases.length === 0) return

    const tag = params.get('from')
    const repository = params.get('repository')
    const asked = tag
      ? releases.find((r) => r.tag === tag && (!repository || r.sourceRepository === repository))
        ?? releases.find((r) => r.tag === tag)
      : undefined

    setLeft(refOf(asked ?? releases[0]!))
  }, [releases, left, params])

  const compare = useCompare()
  const compareRunning = compare.isPending
  const report = compare.data

  /*
   * The security comparison runs SEPARATELY and on demand.
   *
   * Not folded into the package comparison, because the two have different
   * costs and different failure modes: one walks two manifest trees, the other
   * queries a scanner, and a scanner that is down must not fail the comparison
   * that never needed it. Somebody who only wants to know what changed pays
   * for what changed.
   */
  const [view, setView] = useState<View>('package')
  const compareSecurity = useCompareSecurity()
  const syncSecurity = useSyncPackageSecurity()

  useEffect(() => {
    if (!compareRunning || !startedAt) return
    const id = setInterval(() => setElapsed((Date.now() - startedAt) / 1000), 500)
    return () => clearInterval(id)
  }, [compareRunning, startedAt])

  const endpoints = [
    { value: '', label: 'Vendor (where it was discovered)' },
    ...(detail.data?.targets ?? []).map((t) => ({
      value: t.name,
      label: `${t.name}${t.environment ? ` · ${t.environment}` : ''}`,
    })),
  ]

  /*
   * BOTH ENDS ARE PACKAGES, and they are not necessarily the same package.
   *
   * This named the comparison once, on the assumption that a version
   * comparison is two versions of ONE package. It is not: a product publishes
   * nine differently-named packages that share a version string, and comparing
   * `cfx-5000-k8s` against `cfx-5000-k8s-215952-edgenac-…-ncm` is the ordinary
   * case. Naming the comparison after the left end left the right end reading
   * `cfx-near 25.7_mp2604_2131` - a source and a version, and no package at
   * all.
   */
  const leftPkg = releases.find((r) => refOf(r) === left)
  const rightPkg = releases.find((r) => refOf(r) === right)

  const sources = detail.data?.sources ?? []
  // Where the release was found, matched back to its configured source. The
  // default end for a version comparison, overridable below for a product
  // whose sources this cannot match - one that discovers its repositories
  // from the registry catalog declares none to match against.
  const discoveredIn = sourceNameFor(sources, leftPkg?.sourceRepository)
  const versionEnd = sourceOverride ?? discoveredIn ?? sources[0]?.name

  const ready = Boolean(
    product && left &&
    (mode === 'versions' ? right && versionEnd : toEndpoint !== undefined),
  )

  const run = async () => {
    if (!product || !left) return
    setStartedAt(Date.now())
    setElapsed(0)
    const progressToken = crypto.randomUUID()
    setToken(progressToken)
    try {
      await compare.mutateAsync({
        product,
        ref: left,
        repository: leftPkg?.sourceRepository,
        body: {
          // A version comparison names the other release; a location
          // comparison names the other place. Both may be sent, and then the
          // answer covers both at once.
          // A version comparison still has to NAME its end: a product with
          // several sources will not guess which one, and both sides of a
          // version comparison are the same place.
          against: mode === 'versions' ? right : undefined,
          from: mode === 'versions' ? versionEnd : (fromEndpoint || undefined),
          to: mode === 'versions' ? versionEnd : (toEndpoint || undefined),
          // OFF by default, and this is the difference between a comparison
          // that answers in seconds and one that reads like a hang. Opening
          // layer archives to name the files inside them means downloading
          // them; on a release of a few hundred components that is the whole
          // cost of the request. Asked for explicitly, below.
          progressToken,
        },
      })
      setSettled(true)
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The comparison could not be run.')
    }
  }

  /**
   * Run the security comparison for the pair already on screen.
   *
   * Idempotent from the user's side: switching to the security view runs it
   * once and switching back and forth does not re-run it. A re-run is what the
   * refresh on the release's own security panel is for.
   */
  const runSecurity = async () => {
    if (!product || !left || !right) return
    try {
      await compareSecurity.mutateAsync({
        product,
        ref: left,
        repository: leftPkg?.sourceRepository,
        body: { against: rightPkg?.tag ?? right },
      })
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The security comparison could not be run.')
    }
  }

  /**
   * Sync one end from the comparison, then re-run it.
   *
   * A comparison against a release nobody scanned is inconclusive, and the
   * useful thing to offer on that screen is the sync - not a verdict repeating
   * that it cannot say. The re-run is deliberate rather than automatic: a sync
   * is minutes, and a page that silently re-compared would look stuck.
   */
  const syncEnd = (end: 'a' | 'b') => {
    const target = end === 'a' ? leftPkg : rightPkg
    if (!product || !target) return
    syncSecurity.mutate({
      product,
      ref: packageReference(target),
      repository: target.sourceRepository,
    }, {
      onSuccess: (res) => message.info(res.started
        ? `Syncing ${target.tag}. Re-run the comparison once it finishes.`
        : `A sync is already running for ${target.tag}.`),
      onError: (e) => message.error(e instanceof Error ? e.message : 'The sync could not be started.'),
    })
  }

  const showSecurity = () => {
    setView('security')
    if (!compareSecurity.data && !compareSecurity.isPending) void runSecurity()
  }

  const swap = () => {
    if (mode === 'versions') {
      setLeft(right)
      setRight(left)
    } else {
      setFromEndpoint(toEndpoint)
      setToEndpoint(fromEndpoint)
    }
  }

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
  }


  if (products.isError) {
    return (
      <>
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>

      {settled && report ? (
        <Card size="small" style={{ marginBottom: 16 }}>
          <Space size={16} wrap style={{ width: '100%', justifyContent: 'space-between' }}>
            {/*
              EACH END NAMED IN FULL: its package, then its version. A label
              like `cfx-near 25.7_mp2604_2131` is a source and a version and no
              package at all, and the two ends are frequently different
              packages.
            */}
            <Space size={12} wrap align="center">
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>Comparing</Typography.Text>
              <ComparedEnd pkg={leftPkg} fallback={report.a.label} />
              <SwapOutlined style={{ color: c.text3 }} />
              <ComparedEnd pkg={rightPkg} fallback={report.b.label} />
            </Space>
            <Button onClick={() => setSettled(false)}>Change selection</Button>
          </Space>
        </Card>
      ) : (
      <Card style={{ marginBottom: 16 }}>
        <Space direction="vertical" size={14} style={{ width: '100%' }}>
          <Space size={12} wrap>
            <Select
              style={{ minWidth: 200 }}
              value={product}
              onChange={(v) => { update('product', v); setLeft(undefined); setRight(undefined) }}
              loading={products.isLoading}
              options={productList.map((p) => ({ value: p.productId, label: p.displayName || p.productId }))}
            />
            <Segmented
              value={mode}
              onChange={(v) => setMode(v as Mode)}
              options={[
                { value: 'versions', label: 'Two versions' },
                { value: 'locations', label: 'Two locations' },
              ]}
            />
          </Space>

          {/*
            Wraps rather than sitting in fixed thirds: at 1280 the three
            selectors and the button no longer fit on one line, and the page
            used to overflow instead of reflowing.
          */}
          <Row gutter={[12, 12]} align="bottom">
            <Col xs={24} md={10} lg={8}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {mode === 'versions' ? 'Base version' : 'Base location'}
              </Typography.Text>
              {mode === 'versions' ? (
                <Select<string, ReleaseOption>
                  className="slm-release-select"
                  style={{ width: '100%' }}
                  placeholder="Choose a release"
                  value={left}
                  onChange={setLeft}
                  showSearch
                  // Over the NAME and the version both. A product has several
                  // packages and each has many versions, so searching one of
                  // the two finds half of what somebody types.
                  filterOption={(input, option) =>
                    matches(input, option?.name, option?.version, option?.tag)}
                  optionRender={(option) => renderReleaseOption(option.data)}
                  labelRender={(item) => releaseLabelRender(releases, item.value, item.label)}
                  styles={{ popup: { root: { minWidth: 320 } } }}
                  loading={packages.isLoading}
                  options={releaseOptions(releases)}
                />
              ) : (
                <Select
                  style={{ width: '100%' }}
                  value={fromEndpoint ?? ''}
                  onChange={setFromEndpoint}
                  options={endpoints}
                />
              )}
            </Col>

            <Col xs={24} md={4} lg={2} style={{ textAlign: 'center' }}>
              <Button icon={<SwapOutlined />} onClick={swap} aria-label="Swap sides" block />
            </Col>

            <Col xs={24} md={10} lg={8}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                Compare against
              </Typography.Text>
              {mode === 'versions' ? (
                <Select<string, ReleaseOption>
                  className="slm-release-select"
                  style={{ width: '100%' }}
                  placeholder="Choose a release"
                  value={right}
                  onChange={setRight}
                  showSearch
                  filterOption={(input, option) =>
                    matches(input, option?.name, option?.version, option?.tag)}
                  optionRender={(option) => renderReleaseOption(option.data)}
                  labelRender={(item) => releaseLabelRender(releases, item.value, item.label)}
                  styles={{ popup: { root: { minWidth: 320 } } }}
                  loading={packages.isLoading}
                  options={releaseOptions(releases)}
                />
              ) : (
                <Select
                  style={{ width: '100%' }}
                  value={toEndpoint ?? ''}
                  onChange={setToEndpoint}
                  options={endpoints}
                />
              )}
            </Col>

            {mode === 'versions' && sources.length > 1 && (
              <Col xs={24} md={10} lg={6}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>In</Typography.Text>
                <Select
                  style={{ width: '100%' }}
                  value={versionEnd}
                  onChange={setSourceOverride}
                  options={sources.map((src) => ({ value: src.name, label: src.name }))}
                />
              </Col>
            )}

            <Col xs={24} lg={mode === 'versions' && sources.length > 1 ? 24 : 6}>
              <Button
                type="primary"
                block
                disabled={!ready}
                loading={compare.isPending}
                onClick={() => void run()}
              >
                Compare
              </Button>
            </Col>
          </Row>

          {compareRunning && (
            <ComparisonProgress
              token={token}
              elapsedSeconds={elapsed}
              base={releases.find((r) => refOf(r) === left)}
              against={releases.find((r) => refOf(r) === right)}
            />
          )}

        </Space>
      </Card>
      )}

      {compare.isError && <ErrorState error={compare.error} retry={() => void run()} />}

      {/*
        The switch between the two answers, above both of them.

        Only once a comparison has been run: before that there is nothing to
        switch between, and a segmented control over an empty page is a control
        that teaches nothing.
      */}
      {report && mode === 'versions' && (
        <Card size="small" style={{ marginBottom: 16 }}>
          <Space size={12} wrap style={{ width: '100%', justifyContent: 'space-between' }}>
            <Segmented
              value={view}
              onChange={(v) => (v === 'security' ? showSecurity() : setView('package'))}
              options={[
                { value: 'package', label: 'Package comparison' },
                { value: 'security', label: 'Security comparison' },
              ]}
            />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {view === 'package'
                ? 'What the two releases hold, component by component.'
                : 'How the security posture changed from the base release to the new one.'}
            </Typography.Text>
          </Space>
        </Card>
      )}

      {report && view === 'package' && <ComparisonReport report={report} />}

      {view === 'security' && (
        <>
          {/*
            A comparison over stored data is two indexed reads, so there is no
            position worth reporting - a plain loading card is the honest shape
            and a progress bar would be theatre.
          */}
          {compareSecurity.isPending && <Card loading />}
          {compareSecurity.isError && (
            <ErrorState error={compareSecurity.error} retry={() => void runSecurity()} />
          )}
          {compareSecurity.data && product && left && (
            <SecurityComparison
              product={product}
              baseRef={left}
              againstRef={rightPkg?.tag ?? right ?? ''}
              repository={leftPkg?.sourceRepository}
              report={compareSecurity.data}
              onSync={syncEnd}
            />
          )}
        </>
      )}

    </>
  )
}
