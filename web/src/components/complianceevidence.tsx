import { Alert, Button, Card, Space, Tooltip, Typography } from 'antd'
import { useComplianceExcerpt, renderedManifestUrl } from '../api/queries'
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
export function EvidencePanel({ product, reference, repository, result }: {
  product: string
  reference: string
  repository?: string
  result: ComplianceResult
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
          <Typography.Link
            href={renderedManifestUrl(product, reference, {
              repository, document: data.document,
            })}
            target="_blank"
            rel="noreferrer"
            style={{ fontSize: 12 }}
          >
            Open whole
          </Typography.Link>
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
export function RenderedManifestsAction({ product, reference, repository, documents, bytes }: {
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
        'The manifests this run judged, in one file: every chart rendered with the pinned '
        + 'Kubernetes version, plus anything the release ships as a plain manifest. Exactly '
        + 'what the checks read, not a fresh render.'
      }
    >
      <Button
        size="small"
        href={renderedManifestUrl(product, reference, { repository, download: true })}
      >
        Download rendered manifests
        <Typography.Text type="secondary" style={{ fontSize: 11, marginLeft: 6 }}>
          {formatCount(documents)} · {formatBytes(bytes)}
        </Typography.Text>
      </Button>
    </Tooltip>
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
      message="This run kept no rendered manifests"
      description={
        'Findings can be read but not verified against the text they came from. Either this '
        + 'run predates manifests being kept, a later run has superseded it, or this '
        + "Coordinator has coordinator.compliance.evidencePerRelease set below zero. Re-check "
        + 'the release to produce them.'
      }
    />
  )
}
