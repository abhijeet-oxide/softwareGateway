import type { ReactNode } from 'react'
import { useEffect, useState } from 'react'
import { Alert, Button, Space, Tag, Typography } from 'antd'
import { initialsOf, useIdentity } from '../auth/permissions'
import { identityClaims, identityProviderName, isSignedIn, signOut } from '../auth/session'
import { formatAbsolute } from '../domain/format'
import { AppearanceSettings, c, mono, SectionCard } from '../uikit'

/**
 * Who the person signed in is, and what that gets them.
 *
 * ONE surface, deliberately. This was four cards in a two-column grid, and the
 * design system's own rule says why that was wrong: a page of identical white
 * rectangles has no order in it, so the reader's own name, the roles they hold
 * and a text-size control all sat at the same distance and the same weight. A
 * profile is one subject. It reads top to bottom - who, then what that allows,
 * then the preferences that are theirs to change.
 *
 * There is no page header either. A titleless one put Sign out alone in an
 * empty band above the card, which is the page's own furniture holding an
 * action that belongs to the ACCOUNT: it sits on the identity band beside the
 * name it ends the session of.
 *
 * Split out of Settings, which had grown into two pages sharing a scroll: a
 * database driver and a worker fleet on the same screen as somebody's own name.
 */
export default function Profile() {
  const { who, loading } = useIdentity()
  /* The Coordinator reports what it verified: a subject, and what that subject
     may do. It cannot report a name, because ZITADEL's ACCESS token does not
     carry one - a name is in the ID token, which is the client's to read. So
     the verified answer wins where it exists and this fills the rest. */
  const claims = identityClaims()
  const provider = useIdentityProviderName()
  const name = who?.name || claims.name
  const email = who?.email || claims.email
  const username = claims.preferredUsername
  const anonymous = Boolean(who && !who.authenticated)
  const productRoles = Object.entries(who?.productRoles ?? {})
  const tenantRoles = who?.roles ?? []
  /* The login name is shown only when it ADDS something.
   *
   * An identity provider is free to make the preferred username the address,
   * and Microsoft does: the page then introduced somebody as
   * `ap999e@att.com  ·  ap999e@att.com`, which reads as a rendering fault and
   * is one. Compared against the address as well as the name, because those
   * are the two things already on screen. */
  const subtitle = [email, extraIdentifier(username, name, email)]
    .filter(Boolean)
    .join('  ·  ')

  return (
    /* Bounded, because a profile read across fifteen hundred pixels is a label
       at one edge and its value at the other. Flush, because the identity band
       runs to the card's own edges - a tinted strip inset by the body padding
       is a rectangle inside a rectangle. */
    <SectionCard padded={false} style={{ maxWidth: 880 }}>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 18,
          padding: '22px 28px',
          background: c.surface2,
          borderBottom: `1px solid ${c.border}`,
        }}
      >
        <Initials of={anonymous ? who?.subject : name || email || username} />
        <div style={{ minWidth: 0, flex: 1 }}>
          <div style={{ fontSize: 20, fontWeight: 600, lineHeight: 1.25, color: c.text }}>
            {loading ? ' ' : anonymous ? 'Anonymous access' : name || username || 'Unnamed account'}
          </div>
          <div style={{ fontSize: 13, color: c.text2, marginTop: 3 }}>
            {/* One line, not two rows of a table: an address and a login name
                are how somebody recognises themselves, not facts to be looked
                up. */}
            {anonymous ? 'No identity was verified' : subtitle || 'No address on record'}
          </div>
          {!anonymous && tenantRoles.length + productRoles.length > 0 && (
            <div style={{ marginTop: 9 }}>
              <Space size={4} wrap>
                {tenantRoles.map((r) => <Tag key={r} style={{ marginInlineEnd: 0 }}>{r}</Tag>)}
                {productRoles.flatMap(([product, roles]) =>
                  roles.map((r) => (
                    <Tag key={`${product}:${r}`} style={{ marginInlineEnd: 0 }}>{`${product}: ${r}`}</Tag>
                  )),
                )}
              </Space>
            </div>
          )}
        </div>
        {isSignedIn() && (
          // The one control here that ENDS something, so it is the one thing
          // said in the danger colour and the only button on the surface.
          <Button danger onClick={() => void signOut()}>
            Sign out
          </Button>
        )}
      </div>

      <div style={{ padding: '4px 28px 24px' }}>
        {anonymous ? (
          <Section label="Authentication" first>
            <Alert
              type="info"
              showIcon
              message="Authentication is not enabled"
              description={
                <Space direction="vertical" size={4}>
                  <Typography.Text>
                    This Coordinator accepts every caller as{' '}
                    <Typography.Text code>{who?.subject}</Typography.Text> with full
                    permissions. The only control protecting it is network isolation.
                  </Typography.Text>
                  <Typography.Text type="secondary">
                    Everything this interface shows and does is already asked through a
                    permission check, so switching authentication on changes what people can
                    do without changing any page.
                  </Typography.Text>
                </Space>
              }
            />
          </Section>
        ) : (
          <>
            <Section label="Sign-in" first>
              <Field label="Method">{methodLabel(who?.method)}</Field>
              <Field label="Tenant">{who?.tenant || 'All tenants'}</Field>
              <Field label="Account id">
                {/* Named for what it is. It was the only thing this page showed,
                    under the label "Signed in as", which made a person's own
                    screen introduce them as a number. */}
                <Typography.Text
                  type="secondary"
                  copyable={Boolean(who?.subject)}
                  style={{ fontFamily: mono, fontSize: 12.5 }}
                >
                  {who?.subject}
                </Typography.Text>
              </Field>
            </Section>

            {who?.method === 'oidc' && (
              <Section label="Directory">
                {/* WHERE THE DETAILS CAME FROM, and which of them arrived.
                    A profile that shows a name and an address from nowhere
                    cannot answer the question it provokes - why is that all
                    there is? - and the answer is never this product's: it is
                    what the directory asserted. So the fields are listed, the
                    ones that did not arrive are named as not arriving, and the
                    source is stated. */}
                <Field label="Source">{provider || 'This system'}</Field>
                <Field label="Full name">
                  <Asserted value={claims.name} provider={provider} />
                </Field>
                {/* The halves are shown only when they SAY something the full
                    name does not. Where an account was provisioned under a
                    login id, both halves carry that id - ZITADEL requires two
                    and there is only one - and three rows then repeat one
                    word. Two names that are identical is that case and no
                    other. */}
                {claims.givenName !== claims.familyName && (
                  <>
                    <Field label="Given name">
                      <Asserted value={claims.givenName} provider={provider} />
                    </Field>
                    <Field label="Family name">
                      <Asserted value={claims.familyName} provider={provider} />
                    </Field>
                  </>
                )}
                <Field label="Language">
                  <Asserted value={claims.locale} provider={provider} />
                </Field>
                <Field label="Details updated">
                  <Asserted
                    value={claims.updatedAt
                      ? formatAbsolute(new Date(claims.updatedAt * 1000).toISOString()) ?? undefined
                      : undefined}
                    provider={provider}
                  />
                </Field>
                {provider ? (
                  <Note>
                    Read from {provider} at each sign-in. A field {provider} does not hold is
                    not held here. The first sign-in links the account only; details recorded
                    at provisioning are replaced from the second.
                  </Note>
                ) : (
                  <Note>
                    These details are held on the account in this system, and are changed
                    where accounts are provisioned.
                  </Note>
                )}
              </Section>
            )}

            <Section label="Access">
              <Field label="Tenant roles">
                {/* The two tiers are shown apart because they mean different
                    things: a tenant role covers products that do not exist yet,
                    a product role names one. */}
                <Chips values={tenantRoles} />
              </Field>
              <Field label="Product roles">
                {productRoles.length ? (
                  <Space direction="vertical" size={4}>
                    {productRoles.map(([product, roles]) => (
                      <span key={product}>
                        <Typography.Text strong style={{ fontSize: 13 }}>{product}</Typography.Text>
                        <span style={{ marginLeft: 8 }}>
                          <Chips values={roles} />
                        </span>
                      </span>
                    ))}
                  </Space>
                ) : (
                  <Muted>None</Muted>
                )}
              </Field>
              <Field label="Permissions">
                <Chips
                  values={(who?.permissions ?? []).map((p) => (p === '*' ? 'everything' : p))}
                />
              </Field>
              <Field label="Visible products">
                {who?.products?.length ? who.products.join(', ') : 'All products'}
              </Field>
              <Note>
                Roles are granted in the identity provider and arrive in the sign-in token.
                Changing one takes effect at the next sign-in.
              </Note>
            </Section>
          </>
        )}

        <Section label="Appearance">
          <AppearanceSettings />
        </Section>
      </div>
    </SectionCard>
  )
}

/**
 * A titled run of fields, separated from the one above by a rule rather than
 * by another card. The label is small and quiet: it groups, it is not a
 * heading somebody reads. The FIRST one draws no rule - the identity band
 * above it already ends in one, and two hairlines a few pixels apart is the
 * sort of detail that makes a page look assembled rather than designed.
 */
function Section({ label, first, children }: { label: string; first?: boolean; children: ReactNode }) {
  return (
    <div
      style={
        first
          ? { marginTop: 20 }
          : { marginTop: 22, paddingTop: 18, borderTop: `1px solid ${c.border}` }
      }
    >
      <div
        style={{
          fontSize: 11,
          fontWeight: 600,
          letterSpacing: '0.06em',
          textTransform: 'uppercase',
          color: c.text3,
          marginBottom: 10,
        }}
      >
        {label}
      </div>
      {children}
    </div>
  )
}

/**
 * One label and its value, on a grid so every value in the page starts at the
 * same x. A definition list with each row sized to its own label is what makes
 * a settings page look assembled rather than designed.
 */
function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'minmax(120px, 156px) 1fr',
        gap: 12,
        alignItems: 'baseline',
        padding: '6px 0',
        fontSize: 13.5,
      }}
    >
      <div style={{ color: c.text2 }}>{label}</div>
      <div style={{ color: c.text, minWidth: 0 }}>{children}</div>
    </div>
  )
}

/**
 * The login name, when it is not already being shown under another heading.
 *
 * Case-insensitive because an address is, and a capitalisation difference
 * between two spellings of one identifier is not a second fact about a person.
 */
function extraIdentifier(
  username: string | undefined, name: string | undefined, email: string | undefined,
): string | null {
  if (!username) return null
  const same = (other: string | undefined) =>
    Boolean(other) && other!.toLowerCase() === username.toLowerCase()
  return same(name) || same(email) ? null : username
}

/** A sentence about the section, not a field in it - so it is not given an
 *  empty label and hung in the value column. */
function Note({ children }: { children: ReactNode }) {
  return (
    <div style={{ marginTop: 10, fontSize: 12.5, lineHeight: 1.55, color: c.text3 }}>
      {children}
    </div>
  )
}

function Chips({ values }: { values: string[] }) {
  if (!values.length) return <Muted>None</Muted>
  return (
    <Space size={4} wrap>
      {values.map((v) => <Tag key={v} style={{ marginInlineEnd: 0 }}>{v}</Tag>)}
    </Space>
  )
}

function Muted({ children }: { children: ReactNode }) {
  return <span style={{ color: c.text3 }}>{children}</span>
}

/**
 * A field the directory either asserted or did not.
 *
 * The absent case NAMES the directory rather than reading "None", because the
 * two are different facts and only one of them is actionable: this product is
 * not withholding the value, the directory did not send it. That sentence is
 * what turns a sparse profile from a defect into an answer.
 */
function Asserted({ value, provider }: { value?: string; provider: string }) {
  if (value) return <>{value}</>
  return <Muted>{provider ? `Not provided by ${provider}` : 'Not set'}</Muted>
}

/**
 * What this deployment's directory is called, once per mount.
 *
 * Read from the runtime document rather than guessed: the same string the
 * sign-in screen puts on its button, so the two surfaces cannot disagree about
 * what the organization's identity provider is called.
 */
function useIdentityProviderName(): string {
  const [name, setName] = useState('')
  useEffect(() => {
    let live = true
    void identityProviderName().then((n) => {
      if (live) setName(n)
    })
    return () => {
      live = false
    }
  }, [])
  return name
}

/**
 * The monogram. A profile that opens with a name and nothing else reads as a
 * row from a table; one shape at the top is what makes it a person's page.
 * Derived rather than stored - this product has no avatars to serve.
 */
function Initials({ of }: { of: string | undefined }) {
  return (
    <div
      aria-hidden
      style={{
        width: 56,
        height: 56,
        flex: '0 0 56px',
        borderRadius: '50%',
        display: 'grid',
        placeItems: 'center',
        background: c.brandSoft,
        color: c.brandStrong,
        border: `1px solid ${c.brandBorder}`,
        fontSize: 18,
        fontWeight: 600,
        letterSpacing: '0.02em',
      }}
    >
      {initialsOf(of)}
    </div>
  )
}

/**
 * What "oidc" means to somebody who did not configure it.
 *
 * The raw value is the trust path and belongs in an audit record; on this page
 * it was rendered verbatim, so a person who had just clicked a Microsoft button
 * was told their method was `oidc`.
 */
function methodLabel(method: string | undefined): string {
  switch (method) {
    case 'oidc':
      return 'Single sign-on'
    case 'none':
      return 'Not authenticated'
    case '':
    case undefined:
      return 'Unknown'
    default:
      return method
  }
}
