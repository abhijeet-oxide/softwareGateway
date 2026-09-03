import { Alert, Space, Tag, Tooltip, Typography } from 'antd'
import type { ReactNode } from 'react'

import type { SecurityEPSS, SecuritySeverityCounts, Severity } from '../api/types'
import { SEVERITIES } from '../api/types'
import { c, severity as severityColour } from '../uikit'

/**
 * Known-exploited vulnerabilities, which are a different kind of fact from
 * every other number on this page.
 *
 * # Why they get their own colour, their own badge and their own segment
 *
 * Because severity is a judgement and this is a report. "Critical" is somebody's
 * assessment that a vulnerability WOULD be bad to exploit; a KEV is a record
 * that somebody HAS exploited it, in the wild, against somebody. A release with
 * nine hundred criticals and four KEVs has four things to do first, and a page
 * that renders those four as four more criticals has told the reader nothing
 * they can act on.
 *
 * So: exploited findings sort above everything, wear a badge that is not a
 * severity tag, and have a segment of their own that answers "show me only
 * those" in one click - because that is the question somebody arrives with on
 * the day a catalogue is updated.
 *
 * # The colour, and why it is not red
 *
 * Critical is already red. A KEV badge in the same red beside a red severity
 * tag reads as emphasis on the severity rather than as a second fact, and on a
 * high or a medium - where it matters most, because that is where a reader
 * would otherwise skip it - it reads as a mis-coloured severity. Magenta is
 * not on the severity ladder at all, which is the point: it cannot be mistaken
 * for a grade.
 */

/** The exploited palette. Deliberately outside the severity ladder. */
export const kevColour = {
  fill: '#a1006b',
  soft: 'rgba(161, 0, 107, 0.10)',
  border: 'rgba(161, 0, 107, 0.35)',
}

/**
 * The badge that goes beside a vulnerability's identifier.
 *
 * Short, because it sits in a table cell beside a CVE and a severity and a
 * package name, and a badge reading "Known exploited vulnerability" would be
 * wider than the identifier it qualifies. The tooltip carries the sentence.
 */
export function KevTag({ source, epss, compact }: {
  source?: string
  epss?: SecurityEPSS
  /** Compact drops the label to a dot, for a dense table. */
  compact?: boolean
}) {
  const said = source ? `${providerName(source)} reports this` : 'This is'
  const percentile = epss?.percentile !== undefined
    ? ` It scores in the top ${Math.max(1, Math.round((1 - epss.percentile) * 100))}% of `
      + 'vulnerabilities most likely to be exploited in the next 30 days.'
    : ''

  return (
    <Tooltip
      title={
        `${said} on a known-exploited vulnerability catalogue: it is not a prediction that this `
        + `could be attacked, it is a record that it has been.${percentile}`
      }
    >
      <Tag
        color={kevColour.fill}
        style={{
          marginInlineEnd: 0,
          fontWeight: 600,
          fontSize: compact ? 10 : 11,
          lineHeight: '16px',
          paddingInline: compact ? 5 : 7,
        }}
      >
        {compact ? 'KEV' : 'Exploited'}
      </Tag>
    </Tooltip>
  )
}

/**
 * The banner at the top of a release with exploited vulnerabilities in it.
 *
 * # Why a banner and not a card among the cards
 *
 * Because the summary cards answer "how much is wrong with this release" and
 * this answers "is anything in this release being attacked right now". The
 * second is not a bigger version of the first: it does not scale with the
 * release's size, it is usually zero, and when it is not it should be the first
 * thing on the page rather than the fourth.
 *
 * Renders NOTHING at zero, and that is why `capable` exists.
 */
export function KevBanner({ kevs, fixable, severity, capable, onShow }: {
  kevs: number
  fixable: number
  severity: SecuritySeverityCounts
  /**
   * Whether any scanner that answered has an exploited-vulnerability feed.
   *
   * Without this the banner cannot be drawn honestly at zero, so it is not
   * drawn at all - see KevAbsence for the sentence that replaces it.
   */
  capable: boolean
  onShow: () => void
}) {
  if (kevs === 0 || !capable) return null

  const worst = SEVERITIES.filter((sev) => severity[sev] > 0)
  return (
    <Alert
      type="error"
      showIcon={false}
      style={{
        marginBottom: 12,
        background: kevColour.soft,
        borderColor: kevColour.border,
      }}
      message={
        <Space size={10} wrap align="center">
          <span style={{ fontWeight: 600, color: kevColour.fill }}>
            {kevs.toLocaleString()} known-exploited{' '}
            {kevs === 1 ? 'vulnerability' : 'vulnerabilities'}
          </span>
          {worst.length > 0 && (
            <Space size={4} wrap>
              {worst.map((sev) => (
                <Tag
                  key={sev}
                  color={severityColour[sev]}
                  style={{ marginInlineEnd: 0, fontSize: 11 }}
                >
                  {severity[sev].toLocaleString()} {sev}
                </Tag>
              ))}
            </Space>
          )}
          <Typography.Link onClick={onShow} style={{ fontSize: 12.5 }}>
            Show them
          </Typography.Link>
        </Space>
      }
      description={
        <Typography.Text type="secondary" style={{ fontSize: 12.5 }}>
          {fixable > 0
            ? `${fixable.toLocaleString()} of them ${fixable === 1 ? 'has' : 'have'} a fixed `
              + 'version to ask the vendor for. These are not a prediction that this release could '
              + 'be attacked; they are advisories somebody has already been attacked through.'
            : 'None of them has a fixed version yet. These are not a prediction that this release '
              + 'could be attacked; they are advisories somebody has already been attacked '
              + 'through, so the answer is a mitigation rather than an upgrade.'}
        </Typography.Text>
      }
    />
  )
}

/**
 * The sentence that goes where "0 known-exploited" would, on a deployment
 * whose scanners cannot answer the question.
 *
 * # Why this exists at all
 *
 * Because zero means two different things and only one of them is good news.
 * With a scanner that carries a known-exploited catalogue, zero is a genuine
 * and reassuring result. With only a scanner that does not, zero is "nobody
 * checked" - and drawing that as a clean bill of health is exactly the failure
 * the scanned/not-scanned distinction exists to prevent, one level up.
 *
 * So a release scanned only by a scanner without the feed says so, and names
 * the one that has it.
 */
export function KevAbsence({ capable, providers }: {
  capable: boolean
  providers: string[]
}) {
  if (capable || providers.length === 0) return null
  return (
    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
      {providers.map(providerName).join(' and ')}{' '}
      {providers.length === 1 ? 'does' : 'do'} not report known-exploited vulnerabilities, so this
      release has not been checked against a catalogue of them. Enabling Anchore for the repository
      this release lands in adds that.
    </Typography.Text>
  )
}

/**
 * The count for the Exploited segment's label.
 *
 * Its own function because the label has to distinguish three states in a
 * handful of characters: some (`Exploited (4)`), none from a scanner that
 * looked (`Exploited (0)`), and none because nothing can look - where the
 * segment is not offered at all and this returns null.
 */
export function kevSegmentLabel(kevs: number, capable: boolean): ReactNode {
  if (!capable) return null
  return (
    <span style={{ color: kevs > 0 ? kevColour.fill : undefined, fontWeight: kevs > 0 ? 600 : undefined }}>
      Exploited ({kevs.toLocaleString()})
    </span>
  )
}

/**
 * EPSS as a sentence rather than a probability.
 *
 * 0.00042 is a number almost nobody can act on; "bottom 12%" is a number
 * everybody can. The raw score stays in the tooltip for the people who work in
 * it, because they exist and they are the ones who asked for the field.
 */
export function EpssText({ epss }: { epss?: SecurityEPSS }) {
  if (!epss) return null
  const pct = epss.percentile !== undefined
    ? `top ${Math.max(1, Math.round((1 - epss.percentile) * 100))}%`
    : `${(epss.score * 100).toFixed(2)}%`
  return (
    <Tooltip
      title={
        `EPSS estimates a ${(epss.score * 100).toFixed(2)}% chance of exploitation in the next 30 `
        + 'days. It is a model, not an observation - unlike the exploited flag beside it.'
      }
    >
      <Typography.Text type="secondary" style={{ fontSize: 11.5, color: c.text2 }}>
        EPSS {pct}
      </Typography.Text>
    </Tooltip>
  )
}

/** A scanner's name in the words the interface shows. */
export function providerName(provider: string): string {
  switch (provider) {
    case 'jfrog-xray': return 'JFrog Xray'
    case 'anchore': return 'Anchore'
    case 'astra': return 'Astra'
    default: return provider
  }
}

/** Whether a severity is worth colouring a KEV row by. */
export function kevSeverityColour(sev: Severity): string {
  return severityColour[sev]
}
