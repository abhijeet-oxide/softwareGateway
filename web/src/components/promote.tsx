import { useEffect, useMemo, useState } from 'react'
import { App, Button, Checkbox, Modal, Select, Skeleton, Space, Tag, Typography } from 'antd'
import { useNavigate } from 'react-router-dom'
import { usePromote, usePromotionOptions } from '../api/queries'
import { Icon, RocketIcon, environmentIcon } from './icons'
import { InlineNotice, c, envHex, isProductionEnv, mono } from '../uikit'
import type { PromotionDestination, PromotionOptionsResponse } from '../api/types'

/**
 * Promoting a release, as one dialog.
 *
 * # What this screen is actually deciding
 *
 * Not "copy some bytes". A promotion is the moment a release that has been
 * sitting in lab becomes the thing production pulls, and it is usually
 * irreversible in the only sense that matters: clusters start using it. So the
 * dialog is built around the three facts that decide whether somebody presses
 * the button, in the order they think of them:
 *
 *   1. WHERE IT IS NOW. Not a form field - the release is somewhere, and the
 *      dialog states it. It only becomes a question when several targets hold
 *      the release, and then it is a real question rather than a default,
 *      because `lab-eu -> production` and `lab-us -> production` are different
 *      promotions and the system refuses to guess between them.
 *   2. WHERE IT CAN GO, and which of those already have it. A destination that
 *      already holds this release is shown and disabled rather than hidden:
 *      "production already has 23.8.1076" is the answer somebody came here
 *      for as often as the promotion is.
 *   3. WHAT WILL HAPPEN. Seconds, or a copy. Both are correct and one is very
 *      much faster, and an operator planning a maintenance window needs to
 *      know which before they start rather than by watching.
 *
 * # Why the method is not decided here
 *
 * Every answer comes from `promotionOptions`, computed by the same plugin
 * claim the expander will make. Working it out in the browser - two JFrog
 * targets on one host, so it must be instant - would be a second
 * implementation of a rule that lives in Go, and the copy that drifted would
 * be the one on screen while the transfer did something else.
 */

export function PromoteButton({
  product, reference, repository, packageLabel, disabled, disabledReason, size,
}: {
  product: string
  reference: string
  repository?: string
  /** What the release is called, for the dialog's title. */
  packageLabel: string
  disabled?: boolean
  disabledReason?: string
  /** `small` for a table row, where every other control is small too. */
  size?: 'small' | 'middle'
}) {
  const [open, setOpen] = useState(false)

  return (
    <>
      {/*
        ORANGE, and it is the only orange verb in the application.

        Promotion is the one action here that changes what production pulls.
        Download brings bytes into a lab and is reversible by ignoring them;
        this is the step somebody schedules a window for. Green would file it
        with the safe verbs and red would read as a failure, so it takes the
        warm colour that means "consequential, and going ahead" - the same one
        the Promoted stage wears on the release timeline.

        Outlined rather than solid: the page's primary button is already spoken
        for, and two solid buttons side by side compete for the same glance.
      */}
      <Button
        size={size}
        color="orange"
        variant="outlined"
        icon={<Icon as={RocketIcon} title="Promote" />}
        disabled={disabled}
        title={disabled ? disabledReason : undefined}
        onClick={() => setOpen(true)}
      >
        Promote
      </Button>
      {/*
        MOUNTED ONLY WHILE OPEN, which is what keeps the options query from
        firing on every release page somebody merely looks at. The answer needs
        the transfer history and a plugin claim per target; it is cheap, and it
        is not free, and nobody has asked for it until they open this.
      */}
      {open && (
        <PromoteModal
          product={product}
          reference={reference}
          repository={repository}
          packageLabel={packageLabel}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  )
}

function PromoteModal({
  product, reference, repository, packageLabel, onClose,
}: {
  product: string
  reference: string
  repository?: string
  packageLabel: string
  onClose: () => void
}) {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const options = usePromotionOptions(product, reference, repository)
  const promote = usePromote(product)

  const [from, setFrom] = useState<string>()
  const [to, setTo] = useState<string[]>()

  // The server's defaults, taken ONCE. Re-applying them on every render would
  // undo a selection the moment a refetch landed underneath it.
  useEffect(() => {
    const data = options.data
    if (!data) return
    setFrom((current) => current ?? data.defaultOrigin)
    setTo((current) => current ?? data.defaultDestinations ?? [])
  }, [options.data])

  const data = options.data
  const origin = from ?? data?.defaultOrigin ?? ''
  const chosen = to ?? []

  const destinations = useMemo(
    () => (data?.destinations ?? []).filter((d) => d.name !== origin),
    [data, origin],
  )
  const selectable = useMemo(
    () => destinations.filter(available),
    [destinations],
  )

  const send = async () => {
    try {
      const result = await promote.mutateAsync({
        package: qualified(reference, repository),
        from: origin || undefined,
        to: chosen,
      })
      onClose()
      const ids = result.transferIds ?? []
      message.success(
        result.created
          ? `Promoting ${packageLabel} to ${listNames(chosen)}.`
          : `This promotion was already requested; the existing one continues.`,
      )
      // Straight to the transfer when there is ONE. With several there is no
      // single page to go to, and choosing one of them arbitrarily would hide
      // the other two behind a back button.
      navigate(ids.length === 1 ? `/downloads/${ids[0]}` : '/downloads')
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The promotion could not be started.')
    }
  }

  const blocked = data && !data.promotable

  return (
    <Modal
      open
      width={620}
      title={`Promote ${packageLabel}`}
      okText={okText(chosen)}
      okButtonProps={{ disabled: Boolean(blocked) || chosen.length === 0 || !origin }}
      confirmLoading={promote.isPending}
      onOk={() => void send()}
      onCancel={onClose}
    >
      {options.isLoading && <Skeleton active paragraph={{ rows: 4 }} />}

      {options.isError && (
        <InlineNotice tone="danger" action={
          <Button size="small" onClick={() => void options.refetch()}>Try again</Button>
        }>
          Where this release can go could not be read.
        </InlineNotice>
      )}

      {/*
        WHEN IT CANNOT HAPPEN, only the reason. The origin line and the
        destination list are the two halves of a choice nobody has, and
        rendering them greyed out beside a sentence explaining that they are
        unusable is three ways of saying one thing.
      */}
      {blocked && <InlineNotice tone="warn">{data?.reason}</InlineNotice>}

      {data && !blocked && (
        <>
          <Origin
            data={data}
            value={origin}
            onChange={(name) => {
              setFrom(name)
              // A destination that is now the origin cannot also be one.
              setTo((current) => (current ?? []).filter((t) => t !== name))
            }}
          />

          {(
            <>
              <Typography.Text
                type="secondary"
                style={{ display: 'block', margin: '16px 0 8px' }}
              >
                Promote it to
              </Typography.Text>

              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                {destinations.map((d) => (
                  <DestinationRow
                    key={d.name}
                    destination={d}
                    checked={chosen.includes(d.name)}
                    onToggle={(on) =>
                      setTo((current) => {
                        const next = new Set(current ?? [])
                        if (on) next.add(d.name)
                        else next.delete(d.name)
                        return [...next]
                      })
                    }
                  />
                ))}
              </Space>

              {selectable.length === 0 && (
                <InlineNotice tone="info">
                  Every other target already has this release.
                </InlineNotice>
              )}

              {!data.analysed && selectable.length > 0 && (
                /*
                  Said here rather than only in the per-row reason, because it
                  is the one thing on this screen the reader can ACT on to make
                  the promotion faster, and it applies to every row at once.
                */
                <InlineNotice tone="info">
                  Not analysed - this release will be copied. Analyse it first to relocate instead.
                </InlineNotice>
              )}
            </>
          )}
        </>
      )}
    </Modal>
  )
}

/**
 * Where the release is being promoted FROM.
 *
 * A SENTENCE when it is not a choice, and a control only when it is. The
 * origin of a promotion is a fact about the release - it is wherever it was
 * downloaded to - and rendering a fact as a dropdown with one option invites
 * the reader to wonder what the others are.
 */
function Origin({
  data, value, onChange,
}: {
  data: PromotionOptionsResponse
  value: string
  onChange: (name: string) => void
}) {
  const holders = data.origins.filter((o) => o.holds)

  if (holders.length === 0) {
    return (
      <InlineNotice tone="warn">
        This release has not been downloaded to any target yet.
      </InlineNotice>
    )
  }

  const only = holders.length === 1 ? holders[0] : undefined
  if (only) {
    // A FLEX ROW, not inline text. Four things of different kinds - a label, a
    // name, a tag and a path - spaced by an explicit gap rather than by
    // whatever whitespace survives JSX. Inline, the tag butted straight up
    // against the name and the path sat a stray space away from it.
    return (
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8 }}>
        <Typography.Text type="secondary">From</Typography.Text>
        <Typography.Text strong>{only.name}</Typography.Text>
        {only.environment && <EnvironmentTag environment={only.environment} />}
        <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 12 }}>
          {only.registry}{only.repository ? `/${only.repository}` : ''}
        </Typography.Text>
      </div>
    )
  }

  return (
    <>
      <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>
        Several targets hold this release. Choose which one to promote from.
      </Typography.Text>
      <Select
        style={{ width: '100%' }}
        value={value || undefined}
        placeholder="Promote from"
        onChange={onChange}
        options={holders.map((o) => ({
          value: o.name,
          label: o.environment ? `${o.name} (${o.environment})` : o.name,
        }))}
      />
    </>
  )
}

/**
 * One destination, with what promoting into it would do.
 *
 * The whole row is the control, because a checkbox with three lines of text
 * beside it has a hit area the size of the checkbox and everything a reader
 * would sensibly click on is inert.
 */
function DestinationRow({
  destination: d, checked, onToggle,
}: {
  destination: PromotionDestination
  checked: boolean
  onToggle: (on: boolean) => void
}) {
  const usable = available(d)
  const production = isProductionEnv(d.environment)

  return (
    <div
      role="presentation"
      onClick={() => usable && onToggle(!checked)}
      style={{
        display: 'flex', alignItems: 'flex-start', gap: 10, padding: '10px 12px', borderRadius: 8,
        border: `1px solid ${checked ? c.brandBorder : c.border}`,
        background: checked ? c.brandSoft : c.surface,
        cursor: usable ? 'pointer' : 'default',
        opacity: usable ? 1 : 0.6,
      }}
    >
      <Checkbox
        checked={checked}
        disabled={!usable}
        onChange={(e) => onToggle(e.target.checked)}
        onClick={(e) => e.stopPropagation()}
        // Aligned to the FIRST LINE, not the middle. The rows are different
        // heights - a destination with a two-line explanation is twice the
        // height of one without - and a centred checkbox drifts down the card
        // until it no longer reads as belonging to the name at the top of it.
        style={{ marginTop: 3 }}
      />
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8 }}>
          <Typography.Text strong={production}>{d.name}</Typography.Text>
          {d.environment && <EnvironmentTag environment={d.environment} />}
          {d.promotionOnly && (
            <Tag
              style={{ marginInlineEnd: 0 }}
              title="Reachable only by promotion - a vendor can never replicate straight into it"
            >
              Promotion only
            </Tag>
          )}
          <MethodTag destination={d} />
        </div>

        <Typography.Text
          type="secondary"
          style={{ display: 'block', fontFamily: mono, fontSize: 12, marginTop: 2 }}
        >
          {d.registry}{d.repository ? `/${d.repository}` : ''}
        </Typography.Text>

        {/*
          WHY, in words. On a copy this is the diagnosis - two Artifactory
          hosts, a target typed `generic`, a release nobody has analysed - and
          two of those three are configuration mistakes worth fixing. "COPY" on
          its own tells nobody which.
        */}
        {rowNote(d) && (
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: 12, marginTop: 4 }}>
            {rowNote(d)}
          </Typography.Text>
        )}
      </div>
    </div>
  )
}

/**
 * How it gets there, as a tag.
 *
 * Colour on the fast one only. Both are correct and both work, so a copy is
 * not a warning - it is the ordinary answer, and painting it amber would
 * teach readers to distrust a perfectly good promotion.
 */
function MethodTag({ destination: d }: { destination: PromotionDestination }) {
  if (!available(d)) {
    return (
      <Tag color={d.state === 'IN_FLIGHT' ? 'processing' : 'green'} style={{ marginInlineEnd: 0 }}>
        {d.state === 'IN_FLIGHT' ? 'In progress' : 'Already there'}
      </Tag>
    )
  }
  if (d.method === 'RELOCATE') {
    return <Tag color="green" style={{ marginInlineEnd: 0 }}>Relocate</Tag>
  }
  return <Tag style={{ marginInlineEnd: 0 }}>Copy</Tag>
}

function EnvironmentTag({ environment }: { environment: string }) {
  const mark = environmentIcon(environment)
  return (
    <Tag
      style={{ marginInlineEnd: 0, borderColor: envHex(environment), color: envHex(environment) }}
      icon={mark ? <Icon as={mark} title={environment} /> : undefined}
    >
      {environment}
    </Tag>
  )
}

/** A destination can be chosen when it is enabled and does not already hold it. */
function available(d: PromotionDestination): boolean {
  return !d.unavailable && d.state === 'ABSENT'
}

/**
 * The row's explanatory line.
 *
 * A destination that already holds the release says so and stops - the method
 * it WOULD have used is not a fact about anything that is going to happen, and
 * printing it invites the reader to weigh an option they do not have.
 */
function rowNote(d: PromotionDestination): string | undefined {
  if (d.unavailable) return d.unavailable
  if (d.state === 'PRESENT') return 'This release is already here.'
  if (d.state === 'IN_FLIGHT') return 'A transfer is putting it here now.'
  return d.methodReason
}

function okText(chosen: string[]): string {
  if (chosen.length <= 1) return 'Promote'
  return `Promote to ${chosen.length} targets`
}

function listNames(names: string[]): string {
  if (names.length <= 1) return names[0] ?? 'the destination'
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`
}

/**
 * The package reference, qualified by repository.
 *
 * A version tag is not unique within a product - a vendor publishes the same
 * tag into every repository it owns - so the repository has to travel with the
 * reference or the API answers with an ambiguity error rather than a guess.
 */
function qualified(reference: string, repository?: string): string {
  if (!repository || reference.includes('/')) return reference
  return `${repository}:${reference}`
}
