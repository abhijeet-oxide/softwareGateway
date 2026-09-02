import { useEffect, useState } from 'react'
import { Alert, Button, Card, Drawer, Space, Tooltip, Typography } from 'antd'
import { DownloadOutlined, FileTextOutlined } from '../icons'
import { useComplianceExcerpt, renderedManifestUrl } from '../api/queries'
import { fetchText } from '../api/client'
import { formatBytes, formatCount } from '../domain/format'
import { c, mono } from '../uikit'
import type { ComplianceExcerpt, ComplianceResult } from '../api/types'

/**
 * The manifest a finding is about, shown beside the finding.
 *
 * # Why a finding has to be shown and not only stated
 *
 * "Deployment cfx-crds container main: securityContext.runAsNonRoot - runAsUser
 * 0" is a precise claim, and the first thing anybody does with a precise claim
 * about their own software is doubt it. Checking it from what the run RECORDED
 * means pulling the chart out of the registry, installing the same helm and
 * rendering it again with the same pinned versions. Nobody does that. So every
 * disputed finding was settled by whether the vendor trusted the tool, which is
 * not a technical conversation and does not converge.
 *
 * These are the exact bytes the checks were evaluated against, kept from the
 * run - not a re-render, which could differ from what was judged and would
 * therefore not be evidence.
 *
 * # Why the line numbers are the document's own
 *
 * They are what makes the excerpt and the download the same artifact. A number
 * quoted out of this into a mail has to point at the same line of the file the
 * vendor opens, or the excerpt is a screenshot rather than a reference.
 */
export function EvidencePanel({ product, reference, repository, result, onOpenManifest }: {
  product: string
  reference: string
  repository?: string
  result: ComplianceResult
  /**
   * Opens the whole manifest, on this page.
   *
   * A link rather than a navigation, because the reader is mid-triage: they
   * are on a filtered view they reached by narrowing three times, and a new
   * tab answers "what does this chart render" at the cost of the view.
   */
  onOpenManifest?: (document: string) => void
}) {
  const excerpt = useComplianceExcerpt(product, reference, result.seq, { repository })

  if (excerpt.isLoading) {
    return (
      <Card size="small" title="The rendered manifest">
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          Reading it…
        </Typography.Text>
      </Card>
    )
  }

  /*
   * NOT AVAILABLE, and why - which is usually not a fault.
   *
   * The commonest case by far is an undecided check: it is addressed to a chart
   * that never rendered, so there is no manifest, and the reason there is
   * nothing to show is the same reason the check could not be decided. The
   * server says which case applies; repeating its sentence is better than
   * inventing a generic one here.
   */
  if (excerpt.isError || !excerpt.data) {
    return (
      <Card size="small" title="The rendered manifest">
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {excerpt.error instanceof Error
            ? excerpt.error.message
            : 'This run kept no manifest for this result.'}
        </Typography.Text>
      </Card>
    )
  }

  const data = excerpt.data
  return (
    <Card
      size="small"
      title={
        <Space size={8} wrap>
          <span>The rendered manifest</span>
          <Typography.Text type="secondary" style={{ fontSize: 11, fontWeight: 400 }}>
            as checked, not re-rendered
          </Typography.Text>
        </Space>
      }
      extra={
        <Space size={12}>
          {onOpenManifest ? (
            <Typography.Link
              style={{ fontSize: 12 }}
              onClick={() => onOpenManifest(data.document)}
            >
              View full manifest
            </Typography.Link>
          ) : (
            <Typography.Link
              href={renderedManifestUrl(product, reference, {
                repository, document: data.document,
              })}
              target="_blank"
              rel="noreferrer"
              style={{ fontSize: 12 }}
            >
              View full manifest
            </Typography.Link>
          )}
          <Typography.Link
            href={renderedManifestUrl(product, reference, {
              repository, document: data.document, download: true,
            })}
            style={{ fontSize: 12 }}
          >
            Download
          </Typography.Link>
        </Space>
      }
      styles={{ body: { padding: 0 } }}
    >
      <EvidenceHeader excerpt={data} />
      <ExcerptLines excerpt={data} />
      <EvidenceFooter excerpt={data} />
    </Card>
  )
}

/** Which document this is, since a chart's stream covers many templates. */
function EvidenceHeader({ excerpt }: { excerpt: ComplianceExcerpt }) {
  return (
    <div
      style={{
        padding: '6px 10px',
        borderBottom: `1px solid ${c.border}`,
        fontFamily: mono,
        fontSize: 11,
        color: c.text2,
        overflowX: 'auto',
        whiteSpace: 'nowrap',
      }}
    >
      {excerpt.chart
        ? <>{excerpt.chart}{excerpt.chartVersion && `:${excerpt.chartVersion}`}</>
        : excerpt.sourceFile}
      <span style={{ color: c.text3 }}>
        {' · '}lines {excerpt.startLine}–{excerpt.startLine + excerpt.lines.length - 1}
        {' of '}{formatCount(excerpt.totalLines)}
      </span>
    </div>
  )
}

/**
 * The lines, numbered as they are in the document.
 *
 * The focus line is marked; nothing else is. There is no syntax highlighting
 * here on purpose - the one thing this view has to communicate is WHICH LINE,
 * and a screenful of coloured keys and strings competes with the only mark that
 * matters.
 */
function ExcerptLines({ excerpt }: { excerpt: ComplianceExcerpt }) {
  const focus = excerpt.focusLine ?? 0
  const near = focus > 0 ? 0 : (excerpt.nearLine ?? 0)
  const width = String(excerpt.startLine + excerpt.lines.length).length

  return (
    <div
      style={{
        overflowX: 'auto',
        maxHeight: 420,
        overflowY: 'auto',
        background: c.surface2,
        fontFamily: mono,
        fontSize: 12,
        lineHeight: '18px',
      }}
    >
      {excerpt.lines.map((line, i) => {
        const n = excerpt.startLine + i
        const marked = n === focus
        const anchored = n === near
        return (
          <div
            key={n}
            style={{
              display: 'flex',
              whiteSpace: 'pre',
              background: marked
                ? c.dangerBg
                : anchored
                  ? c.markBg
                  : undefined,
              borderLeft: marked
                ? `3px solid ${c.danger}`
                : anchored
                  ? `3px solid ${c.markBd}`
                  : '3px solid transparent',
            }}
          >
            <span
              style={{
                color: c.text3,
                userSelect: 'none',
                textAlign: 'right',
                minWidth: `${width + 1}ch`,
                paddingRight: 10,
                paddingLeft: 6,
              }}
            >
              {n}
            </span>
            <span style={{ paddingRight: 12 }}>{line === '' ? ' ' : line}</span>
          </div>
        )
      })}
    </div>
  )
}

/**
 * What the marks mean, and what they do NOT mean.
 *
 * A field that is absent is half of every run, and there is no line for it. The
 * panel says that rather than leaving a reader to conclude the highlight is
 * missing because something broke - and rather than putting the highlight on a
 * plausible line, which would make this a claim about the document that is
 * false.
 */
function EvidenceFooter({ excerpt }: { excerpt: ComplianceExcerpt }) {
  const focus = excerpt.focusLine ?? 0
  const near = excerpt.nearLine ?? 0

  return (
    <div style={{ padding: '6px 10px', borderTop: `1px solid ${c.border}` }}>
      <Space direction="vertical" size={2} style={{ width: '100%' }}>
        {focus > 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: 11 }}>
            Line <b>{focus}</b> is <span style={{ fontFamily: mono }}>{excerpt.locus}</span>.
          </Typography.Text>
        ) : (
          <Typography.Text type="secondary" style={{ fontSize: 11 }}>
            <span style={{ fontFamily: mono }}>{excerpt.locus || 'The field'}</span> is not in
            this manifest, which is what the finding says
            {near > 0 && <> — line <b>{near}</b> is as far as that path goes</>}.
          </Typography.Text>
        )}
        {excerpt.truncated && (
          <Typography.Text type="warning" style={{ fontSize: 11 }}>
            This document was cut at this Coordinator's evidence budget, so it is not the
            whole of what was rendered.
          </Typography.Text>
        )}
      </Space>
    </div>
  )
}

/**
 * Every rendered manifest of a release, as one file.
 *
 * # Why the whole release and not a chart at a time
 *
 * Because of who reads it. A release manager reads excerpts one row at a time;
 * a vendor engineer is sent a report and needs the manifests it is about, and
 * assembling those from ninety-seven links is not a thing anybody does. One
 * file, with each document named and the run that produced it stated at the
 * top, is the artifact that conversation actually needs.
 */
export function DownloadManifestsButton({ product, reference, repository, documents, bytes }: {
  product: string
  reference: string
  repository?: string
  documents: number
  bytes: number
}) {
  if (documents === 0) return null
  return (
    <Tooltip
      title={
        `${formatCount(documents)} documents, ${formatBytes(bytes)}. Every chart rendered with `
        + 'the pinned Kubernetes version, plus anything the release ships as a plain manifest. '
        + 'Exactly what the checks read, not a fresh render.'
      }
    >
      {/*
        A BUTTON WITH THE ICON EVERY OTHER DOWNLOAD IN THIS PRODUCT USES, and a
        label that is two words.

        It used to say "Download rendered manifests" with the count and the byte
        size printed inside the button in a second colour - a control carrying a
        sentence, sized differently from the two beside it, on a row of buttons.
        The numbers are a fact about what will arrive, which is what a tooltip
        is for; the button says what pressing it does.
      */}
      <Button icon={<DownloadOutlined />} href={renderedManifestUrl(product, reference, { repository, download: true })}>
        Download manifests
      </Button>
    </Tooltip>
  )
}

/**
 * The two things a reader does with one chart's manifest: read it, or keep it.
 *
 * Together in one cell because they are one decision made twice, and separate
 * from the release-wide download because a vendor engineer owns one chart and
 * does not want the other ninety-six.
 *
 * The absence is drawn too, and the two absences are different: a chart that
 * rendered and whose manifest was not retained can be produced again by
 * re-checking, and a chart that never rendered has no output to retain.
 */
export function ManifestLinks({
  available, rendered, document, product, reference, repository, onOpen,
}: {
  available: boolean
  rendered: boolean
  document: string
  product: string
  reference: string
  repository?: string
  onOpen: (document: string) => void
}) {
  if (!available) {
    return (
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {rendered ? 'Not retained' : 'No output'}
      </Typography.Text>
    )
  }
  return (
    <Space size={4}>
      <Button size="small" type="text" icon={<FileTextOutlined />} onClick={() => onOpen(document)}>
        View
      </Button>
      <Tooltip title="Download this chart's rendered manifest">
        <Button
          size="small"
          type="text"
          icon={<DownloadOutlined />}
          href={renderedManifestUrl(product, reference, {
            repository, document, download: true,
          })}
        />
      </Tooltip>
    </Space>
  )
}

/**
 * Said where the coverage table is, when a run kept nothing to show.
 *
 * Its own notice rather than silence: "no evidence" and "no findings" are
 * different statements, and a reader who opens a finding expecting the manifest
 * and gets a sentence per row deserves to have been told once at the top.
 */
export function NoEvidenceNotice({ checked }: { checked: boolean }) {
  if (!checked) return null
  return (
    <Alert
      type="info"
      showIcon
      message="No rendered manifests retained for this run"
      description={
        'Findings can be read but not verified against the manifests they were derived '
        + 'from. Either this run predates manifest retention, a later run has superseded it, '
        + 'or coordinator.compliance.evidencePerRelease is set below zero on this '
        + 'Coordinator. Re-check the release to produce them.'
      }
    />
  )
}

/**
 * A whole rendered manifest, beside the table rather than instead of it.
 *
 * # Why a drawer and not a new tab
 *
 * The reader is mid-triage. They are on a filtered view of nine hundred
 * findings, at a scroll position they reached by narrowing three times, and the
 * question they want answered is "what does this chart actually render". A new
 * tab answers it and costs them the view; a link that navigates costs them the
 * view AND the back button. Neither is worth the fifty lines this saves.
 *
 * The download is still here, because the moment after "what does it render" is
 * frequently "send it to the vendor".
 */
export function ManifestDrawer({ document, product, reference, repository, onClose }: {
  /** The document key - a chart's name, or a plain manifest's path. Null closes. */
  document: string | null
  product: string
  reference: string
  repository?: string
  onClose: () => void
}) {
  const [text, setText] = useState<string>('')
  const [error, setError] = useState<string>('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (!document) return
    let live = true
    setLoading(true)
    setError('')
    setText('')
    fetchText(renderedManifestUrl(product, reference, { repository, document }))
      .then((body) => { if (live) setText(body) })
      .catch((e: unknown) => {
        if (live) setError(e instanceof Error ? e.message : 'The manifest could not be read.')
      })
      .finally(() => { if (live) setLoading(false) })
    return () => { live = false }
  }, [document, product, reference, repository])

  const lines = text ? text.replace(/\n$/, '').split('\n') : []

  return (
    <Drawer
      open={Boolean(document)}
      onClose={onClose}
      width={880}
      title={
        <Space size={10} wrap>
          <span>Rendered manifest</span>
          <Typography.Text
            type="secondary"
            style={{ fontFamily: mono, fontSize: 12, fontWeight: 400 }}
          >
            {document}
          </Typography.Text>
        </Space>
      }
      extra={
        document && (
          <Space size={10}>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {lines.length > 0 && `${formatCount(lines.length)} lines`}
            </Typography.Text>
            <Button
              size="small"
              href={renderedManifestUrl(product, reference, {
                repository, document, download: true,
              })}
            >
              Download
            </Button>
          </Space>
        )
      }
      styles={{ body: { padding: 0 } }}
    >
      {loading && (
        <div style={{ padding: 16 }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            Loading manifest
          </Typography.Text>
        </div>
      )}
      {error && (
        <div style={{ padding: 16 }}>
          <Typography.Text type="danger" style={{ fontSize: 12 }}>{error}</Typography.Text>
        </div>
      )}
      {!loading && !error && <ManifestBody lines={lines} />}
    </Drawer>
  )
}

/**
 * The manifest, numbered as the download is and split at its documents.
 *
 * A chart's stream is many objects with helm's `# Source:` markers between
 * them, and read as one undifferentiated column of YAML it is unusable at the
 * length a real chart produces. The markers get a rule and emphasis, so the
 * reader scrolls by object rather than by line - and the line numbers stay the
 * document's own, so one quoted into a mail still points at the same place.
 */
function ManifestBody({ lines }: { lines: string[] }) {
  const width = String(lines.length).length

  return (
    <div
      style={{
        fontFamily: mono,
        fontSize: 12,
        lineHeight: '18px',
        overflowX: 'auto',
        background: c.surface2,
        minHeight: '100%',
      }}
    >
      {lines.map((line, i) => {
        const n = i + 1
        const source = line.startsWith('# Source: ')
        const separator = line.trim() === '---'
        return (
          <div
            key={n}
            style={{
              display: 'flex',
              whiteSpace: 'pre',
              background: source ? c.markBg : undefined,
              borderTop: separator ? `1px solid ${c.border}` : undefined,
            }}
          >
            <span
              style={{
                color: c.text3,
                userSelect: 'none',
                textAlign: 'right',
                minWidth: `${width + 1}ch`,
                paddingRight: 10,
                paddingLeft: 8,
                flexShrink: 0,
              }}
            >
              {n}
            </span>
            <span
              style={{
                paddingRight: 12,
                color: source ? c.text : undefined,
                fontWeight: source ? 600 : undefined,
              }}
            >
              {line === '' ? ' ' : line}
            </span>
          </div>
        )
      })}
    </div>
  )
}
