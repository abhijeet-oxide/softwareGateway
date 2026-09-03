import { Button, Card, Empty, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { useMemo, useState } from 'react'

import type {
  SecuritySourceComparison, SecuritySourceCounts,
} from '../api/types'
import { c, severity as severityColour } from '../uikit'
import { kevColour, providerName } from './securitykev'

/**
 * What each scanner found, and what only it found.
 *
 * # The question this page exists to answer
 *
 * Running two scanners is a cost - two integrations, two credentials, two lots
 * of somebody's capacity - and the only justification for it is that they
 * disagree usefully. "Anchore found 402 advisories Xray did not, two of them
 * known-exploited" is that justification stated. Without a view like this, the
 * second scanner is a number that went up and nobody can say whether it was
 * worth switching on.
 *
 * # Why it is a panel and not a filter
 *
 * Because the filter already exists - the segmented control on the findings
 * table answers "show me what Anchore says". This answers a different question,
 * about the SCANNERS rather than about the release, and it is read once when
 * somebody is deciding whether to keep both rather than every time they open a
 * release. So it sits under its own heading, folded away, and the filter stays
 * where the findings are.
 *
 * # Why "enriched" is on it
 *
 * Because a scanner whose exclusive-finding count is zero has still earned its
 * place if it supplied the fix version, the description or the exploited flag
 * for several thousand findings the other one graded blind. A comparison that
 * could only count rows would recommend switching that scanner off.
 */
export function SourceComparisonPanel({ sources, comparison, onFilter }: {
  sources: SecuritySourceCounts[]
  comparison?: SecuritySourceComparison
  /** Narrow the findings table to one scanner's exclusive advisories. */
  onFilter?: (provider: string) => void
}) {
  // Nothing to compare. A panel about disagreement, on a deployment with one
  // scanner, is a heading over a table of one row saying it agrees with itself.
  if (sources.length < 2) return null

  return (
    <Card
      size="small"
      id="security-sources"
      title={
        <Space size={8}>
          <span>Scanner comparison</span>
          <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
            what each one found that the others did not
          </Typography.Text>
        </Space>
      }
      styles={{ body: { paddingTop: 10 } }}
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <SourceTable sources={sources} onFilter={onFilter} />
        <ExclusiveKevs sources={sources} comparison={comparison} />
        <AgreementLine sources={sources} comparison={comparison} />
      </Space>
    </Card>
  )
}

function SourceTable({ sources, onFilter }: {
  sources: SecuritySourceCounts[]
  onFilter?: (provider: string) => void
}) {
  return (
    <Table<SecuritySourceCounts>
      size="small"
      rowKey="provider"
      dataSource={sources}
      pagination={false}
      columns={[
        {
          title: 'Scanner',
          dataIndex: 'provider',
          render: (_: unknown, row) => (
            <Space size={6}>
              <Typography.Text strong>{row.label || providerName(row.provider)}</Typography.Text>
              {row.coverage && row.coverage.scanned < row.coverage.scannable && (
                /*
                  Per-scanner coverage, because it is NOT the release's.
                  Anchore may have analysed 140 of 157 images while Xray has
                  indexed all of them, and one figure for both is a lie about
                  whichever it does not describe - which on this table, where
                  the whole point is comparing them, is the one place it would
                  actually mislead somebody.
                */
                <Tooltip
                  title={
                    `${row.label} has results for ${row.coverage.scanned} of `
                    + `${row.coverage.scannable} images. Its numbers cover only those.`
                  }
                >
                  <Tag color="orange" style={{ marginInlineEnd: 0, fontSize: 11 }}>
                    {row.coverage.scanned}/{row.coverage.scannable} images
                  </Tag>
                </Tooltip>
              )}
            </Space>
          ),
        },
        {
          title: 'Advisories',
          dataIndex: 'uniqueCves',
          align: 'right' as const,
          width: 110,
          render: (n: number) => <Numeric value={n} />,
        },
        {
          title: (
            <Tooltip title="Advisories this scanner reported and no other scanner did.">
              <span>Only here</span>
            </Tooltip>
          ),
          dataIndex: 'onlyHere',
          align: 'right' as const,
          width: 120,
          render: (n: number, row) => (
            n > 0 && onFilter
              ? (
                <Button type="link" size="small" style={{ padding: 0 }} onClick={() => onFilter(row.provider)}>
                  {n.toLocaleString()}
                </Button>
              )
              : <Numeric value={n} />
          ),
        },
        {
          title: (
            <Tooltip
              title={
                'Known-exploited advisories only this scanner reported. The number that decides '
                + 'whether a second scanner earned its place: four thousand extra lows nobody will '
                + 'read and two exploited advisories nobody else saw look identical in "only here".'
              }
            >
              <span>Exploited only here</span>
            </Tooltip>
          ),
          dataIndex: 'kevOnly',
          align: 'right' as const,
          width: 160,
          render: (n: number) => (
            n > 0
              ? <Typography.Text strong style={{ color: kevColour.fill }}>{n.toLocaleString()}</Typography.Text>
              : <Numeric value={0} />
          ),
        },
        {
          title: (
            <Tooltip
              title={
                'Advisories another scanner also reported, where this one supplied a fact the '
                + 'other lacked - a fix version, a description, a CVSS vector, an exploited flag. '
                + 'The honest defence of a scanner whose "only here" is zero.'
              }
            >
              <span>Enriched</span>
            </Tooltip>
          ),
          dataIndex: 'enriched',
          align: 'right' as const,
          width: 110,
          render: (n: number) => <Numeric value={n} />,
        },
        {
          title: 'Severity',
          key: 'severity',
          width: 220,
          render: (_: unknown, row) => (
            <Space size={4} wrap>
              {(['critical', 'high', 'medium', 'low'] as const).map((sev) => {
                const n = row.counts.bySeverity[sev]
                if (!n) return null
                return (
                  <Tag
                    key={sev}
                    color={severityColour[sev]}
                    style={{ marginInlineEnd: 0, fontSize: 11 }}
                  >
                    {n.toLocaleString()}
                  </Tag>
                )
              })}
            </Space>
          ),
        },
      ]}
    />
  )
}

function Numeric({ value }: { value: number }) {
  return (
    <Typography.Text
      type={value === 0 ? 'secondary' : undefined}
      style={{ fontVariantNumeric: 'tabular-nums' }}
    >
      {value.toLocaleString()}
    </Typography.Text>
  )
}

/**
 * The advisories one scanner reported as exploited and the other did not
 * mention at all.
 *
 * Listed in full rather than counted, and never truncated, because there are
 * never four thousand of them and because each one is a specific thing to go
 * and look at. This is the single most valuable output of running two scanners
 * and it is four rows long.
 */
function ExclusiveKevs({ sources, comparison }: {
  sources: SecuritySourceCounts[]
  comparison?: SecuritySourceComparison
}) {
  const entries = useMemo(() => {
    const out: { provider: string; label: string; cves: string[] }[] = []
    for (const src of sources) {
      const cves = comparison?.kevOnlyIn?.[src.provider] ?? []
      if (cves.length > 0) out.push({ provider: src.provider, label: src.label, cves })
    }
    return out
  }, [sources, comparison])

  if (entries.length === 0) return null

  return (
    <div
      style={{
        background: kevColour.soft,
        border: `1px solid ${kevColour.border}`,
        borderRadius: 6,
        padding: '10px 12px',
      }}
    >
      <Typography.Text strong style={{ color: kevColour.fill, fontSize: 12.5 }}>
        Exploited vulnerabilities only one scanner reported
      </Typography.Text>
      <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: '4px 0 8px' }}>
        These are on a known-exploited catalogue and the other scanner did not mention them. Each
        one is a specific thing to check rather than a difference in feed coverage.
      </Typography.Paragraph>
      <Space direction="vertical" size={6} style={{ width: '100%' }}>
        {entries.map((e) => (
          <div key={e.provider}>
            <Typography.Text style={{ fontSize: 12 }}>
              Only {e.label || providerName(e.provider)}:{' '}
            </Typography.Text>
            <Space size={4} wrap>
              {e.cves.map((cve) => (
                <Tag key={cve} style={{ marginInlineEnd: 0, fontSize: 11, fontFamily: 'monospace' }}>
                  {cve}
                </Tag>
              ))}
            </Space>
          </div>
        ))}
      </Space>
    </div>
  )
}

/**
 * How much the scanners agreed on, in one line.
 *
 * A sentence rather than a chart, because the shape of the answer is always the
 * same - a large overlap and two small exclusive sets - and a chart of that is
 * three numbers drawn as a picture of three numbers.
 */
function AgreementLine({ sources, comparison }: {
  sources: SecuritySourceCounts[]
  comparison?: SecuritySourceComparison
}) {
  if (!comparison) return null
  const total = sources.reduce((n, s) => Math.max(n, s.uniqueCves), 0)
  if (total === 0) return null

  const exclusive = sources
    .filter((s) => s.onlyHere > 0)
    .map((s) => `${s.onlyHere.toLocaleString()} only in ${s.label || providerName(s.provider)}`)

  return (
    <Typography.Text type="secondary" style={{ fontSize: 12, color: c.text2 }}>
      {comparison.shared.toLocaleString()} advisories were reported by every scanner
      {exclusive.length > 0 && `, ${exclusive.join(', ')}`}.
      {comparison.truncated && ' Some lists above are capped; the export carries all of them.'}
    </Typography.Text>
  )
}

/**
 * The empty state for a release with one scanner, on a deployment that has two
 * configured.
 *
 * Shown where the comparison would be, because "this release was scanned by
 * one of your two scanners" is worth saying: it means the other has not run,
 * not that the two agree.
 */
export function SourceComparisonPending({ configured, answered }: {
  configured: string[]
  answered: string[]
}) {
  const [dismissed, setDismissed] = useState(false)
  const missing = configured.filter((p) => !answered.includes(p))
  if (dismissed || missing.length === 0 || answered.length === 0) return null

  return (
    <Card size="small" styles={{ body: { padding: '12px 14px' } }}>
      <Empty
        image={null}
        styles={{ image: { display: 'none' } }}
        description={
          <Space direction="vertical" size={4}>
            <Typography.Text style={{ fontSize: 13 }}>
              {missing.map(providerName).join(' and ')}{' '}
              {missing.length === 1 ? 'has' : 'have'} not answered for this release yet.
            </Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Sync the release to ask{' '}
              {missing.length === 1 ? 'it' : 'them'}. Until then these numbers are{' '}
              {answered.map(providerName).join(' and ')} alone, and there is nothing to compare.
            </Typography.Text>
          </Space>
        }
      />
      <div style={{ textAlign: 'right' }}>
        <Button size="small" type="text" onClick={() => setDismissed(true)}>Dismiss</Button>
      </div>
    </Card>
  )
}
