import { Button, Space, Tooltip, Typography } from 'antd'
import { Link } from 'react-router-dom'

import type { Package } from '../api/types'
import type { Pick, Selection } from '../domain/compare'
import { comparisonHref, complete } from '../domain/compare'
import { version } from '../domain/derive'
import { CloseCircleOutlined, ScaleOutlined, SwapOutlined } from '../icons'
import { c, mono } from '../uikit'

/**
 * The bar that says what is being compared, above the listing that chooses it.
 *
 * # What a reader has to be able to tell at a glance
 *
 * Three things, and the old page answered none of them well: which release is
 * the base, which it is being compared against, and what to do next. It showed
 * two dropdowns with a swap button between them, so "what have I chosen" and
 * "what am I choosing" were the same control - and a dropdown that is both a
 * question and its answer reads as neither.
 *
 * Here they are separate. The bar STATES the selection; the table below it is
 * where the choosing happens. Each slot is either a release, named in full, or
 * an instruction saying that this is the one to pick next.
 *
 * # Why the second slot says what to do rather than sitting empty
 *
 * Because an empty box is ambiguous - it might be broken, it might be optional,
 * it might be waiting. "Select the second package" is a sentence, and a reader
 * who has just ticked one box knows exactly what the page wants from them.
 */
export function CompareSelectionBar({
  selection, packagesFor, productName, onReset, onCancel, onSwap,
}: {
  selection: Selection
  /** Resolves a pick back to the listed release, for its name and version. */
  packagesFor: (pick: Pick) => Package | undefined
  /**
   * The product a half-made selection is locked to, as a person names it.
   *
   * A comparison covers one product - two products are two sets of
   * repositories under two sets of credentials - so once one end is chosen the
   * other products' rows stop being selectable. Saying which product beats
   * leaving a reader to discover it by clicking rows that do nothing.
   */
  productName?: string
  onReset: () => void
  onCancel: () => void
  onSwap: () => void
}) {
  const a = selection.a ? packagesFor(selection.a) : undefined
  const b = selection.b ? packagesFor(selection.b) : undefined
  const ready = complete(selection)
  const chosen = (selection.a ? 1 : 0) + (selection.b ? 1 : 0)

  return (
    <div
      // Sticky, because the choosing happens by scrolling a table of two
      // hundred rows. A bar that scrolled away would leave a reader ticking a
      // second box with no idea what the first one was.
      style={{
        position: 'sticky', top: 0, zIndex: 5,
        display: 'flex', flexWrap: 'wrap', gap: 16, alignItems: 'center',
        justifyContent: 'space-between',
        padding: '12px 16px', marginBottom: 12,
        background: c.brandSoft, border: `1px solid ${c.brandBorder}`, borderRadius: 8,
      }}
    >
      <Space size={14} align="center" wrap style={{ minWidth: 0 }}>
        <ScaleOutlined style={{ color: c.brand }} />
        <Slot
          label="Comparing"
          pkg={a}
          fallback="Select the first package"
          position={1}
        />
        {/*
          The swap is offered only once both ends exist. Before that it is a
          control that can only rearrange nothing, and a disabled button beside
          an instruction is noise on top of the instruction.
        */}
        {ready ? (
          <Tooltip title="Compare them the other way round">
            {/*
              An icon-only button needs a name of its own. A tooltip is a
              hover affordance and contributes nothing to the accessibility
              tree, so without this the control announces as "button" - and
              the one control on this bar that reverses the meaning of the
              whole comparison is the worst one to leave unnamed.
            */}
            <Button
              size="small"
              type="text"
              icon={<SwapOutlined />}
              onClick={onSwap}
              aria-label="Compare them the other way round"
            />
          </Tooltip>
        ) : (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>against</Typography.Text>
        )}
        <Slot
          label={selection.a ? 'Against' : 'Then'}
          pkg={b}
          fallback={selection.a ? 'Select the second package' : 'the second package'}
          position={2}
          muted={!selection.a}
        />
      </Space>

      <Space size={8} wrap>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {chosen === 0
            ? 'Tick two rows below'
            : chosen === 1
              // Named, because once one end is chosen the rows of every other
              // product stop being selectable - and a row that will not tick,
              // with the reason only on hover, reads as a broken row.
              ? productName
                ? `One more, from ${productName}`
                : 'One more'
              : 'Ready'}
        </Typography.Text>
        <Button size="small" onClick={onReset} disabled={chosen === 0}>Clear</Button>
        <Button size="small" icon={<CloseCircleOutlined />} onClick={onCancel}>Cancel</Button>
        {ready && selection.a && selection.b ? (
          <Link to={comparisonHref(selection.a, selection.b, selection.intent)}>
            <Button size="small" type="primary">
              {selection.intent === 'vulnerabilities' ? 'Compare' : 'Compare'}
            </Button>
          </Link>
        ) : (
          <Tooltip title="Pick two releases of the same product">
            <Button size="small" type="primary" disabled icon={<ScaleOutlined />}>Compare</Button>
          </Tooltip>
        )}
      </Space>
    </div>
  )
}

/**
 * One end of the comparison: a numbered slot holding a release or an
 * instruction.
 *
 * The number is not decoration. Two boxes side by side with the same styling
 * are two boxes; "1" and "2" say that one is filled before the other, which is
 * the only thing about this interaction a reader has to learn.
 */
function Slot({ label, pkg, fallback, position, muted }: {
  label: string
  pkg?: Package
  fallback: string
  position: number
  muted?: boolean
}) {
  const filled = Boolean(pkg)
  return (
    <Space size={8} align="center" style={{ minWidth: 0 }}>
      <span
        aria-hidden
        style={{
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          width: 18, height: 18, borderRadius: '50%', flex: '0 0 auto',
          fontSize: 11, fontWeight: 600,
          background: filled ? c.brand : 'transparent',
          color: filled ? '#fff' : c.text3,
          border: filled ? 'none' : `1px dashed ${c.border}`,
        }}
      >
        {position}
      </span>
      <Space direction="vertical" size={0} style={{ minWidth: 0 }}>
        <Typography.Text
          type="secondary"
          style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: '0.04em' }}
        >
          {label}
        </Typography.Text>
        {pkg ? (
          <Space size={8} align="center" wrap style={{ minWidth: 0 }}>
            {/*
              The NAME and the version, both. A product publishes nine
              differently-named packages that share one version string, so a
              slot reading "25.7_mp2604_2131" names about a ninth of a release.
            */}
            <Typography.Text
              strong
              style={{ fontSize: 13, maxWidth: 260 }}
              ellipsis={{ tooltip: pkg.displayRepository || pkg.sourceRepository || pkg.tag }}
            >
              {pkg.displayRepository || pkg.sourceRepository || pkg.tag}
            </Typography.Text>
            <Typography.Text style={{ fontFamily: mono, fontSize: 12, color: c.text2 }}>
              {version(pkg)}
            </Typography.Text>
          </Space>
        ) : (
          <Typography.Text
            style={{ fontSize: 13, color: muted ? c.text3 : c.text2 }}
          >
            {fallback}
          </Typography.Text>
        )}
      </Space>
    </Space>
  )
}
