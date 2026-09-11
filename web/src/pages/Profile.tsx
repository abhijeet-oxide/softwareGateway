import type { ReactNode } from 'react'
import { useEffect, useState } from 'react'
import { Alert, Popover, Space, Tag, Tooltip, Typography } from 'antd'
import { initialsOf, useIdentity } from '../auth/permissions'
import { accountBadge } from '../auth/badge'
import { accountLabel, productAccess, roleLabel, roleMeaning } from '../auth/roles'
import { identityClaims, identityProviderName, isSignedIn, signOut } from '../auth/session'
import { formatAbsolute } from '../domain/format'
import { ActionButton } from '../components/access'
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
  const products = productAccess(who)
  const tenantRoles = who?.roles ?? []
  const standing = accountLabel(who)
  /* The login name is shown only when it ADDS something.
   *
   * An identity provider is free to make the preferred username the address,
   * and Microsoft does: the page then introduced somebody as
   * `email@example.com  ·  email@example.com`, which reads as a rendering fault and
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
    <SectionCard padded={false} style={{ width: '100%' }}>
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
          {/*
            WHAT THIS PERSON IS, then WHERE.

            This line used to print every role identifier the account holds,
            comma separated: `org-admin`, `org-member` and a
            `product: role` pair per product, which for an administrator of ten
            products is twelve slugs wrapping onto three lines under their own
            name. It read as a debug dump, and the one thing somebody actually
            wants from it - am I an administrator here, and which products are
            mine - was the hardest part of it to find.

            So the standing is one chip, in the brand colour because it is the
            answer, and the products are named after it with the rest counted.
            The full list is a click away and laid out below, where a list
            belongs.
          */}
          {!anonymous && (
            <div style={{ marginTop: 10 }}>
              <Space size={[7, 6]} wrap>
                {/*
                  THE SAME GLYPH the navigation's avatar wears, on the same
                  word. Two surfaces name this account's standing and a reader
                  moving between them should be looking at one thing, not
                  learning a picture in one place and a word in the other.
                */}
                <Tag
                  color="processing"
                  icon={accountBadge(who)}
                  style={{ marginInlineEnd: 0, fontWeight: 600 }}
                >
                  {standing}
                </Tag>
                <ProductChips products={products} />
              </Space>
            </div>
          )}
        </div>
        {isSignedIn() && (
          // The one control here that ENDS something, so it is the one thing
          // said in the danger colour and the only button on the surface.
          <ActionButton danger action="Sign out" onClick={signOut}>
            Sign out
          </ActionButton>
        )}
      </div>

      <div style={{ padding: '4px 28px 24px' }}>
        {anonymous ? (
          <div style={{ maxWidth: 960 }}>
            <Section label="Authentication" first>
              <Alert
                type="error"
                showIcon
                message={
                  <Space direction="vertical" size={4}>
                    <Typography.Text>
                      Authentication is not enabled.
                    </Typography.Text>
                  </Space>
                }
              />
            </Section>
          </div>
        ) : (
          <div style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 420px), 1fr))',
            gap: '0 36px',
            alignItems: 'start',
          }}>
            <div>
              <Section label="Sign-in" first>
                <Field label="Method">{methodLabel(who?.method)}</Field>
                <Field label="Tenant">{who?.tenant || 'All tenants'}</Field>
                <Field label="Account id">
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
                  <Field label="Source">{provider || 'This system'}</Field>
                  <Field label="Full name">
                    <Asserted value={claims.name} provider={provider} />
                  </Field>
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
                      Details fetched from {provider}
                    </Note>
                  ) : (
                    <Note>
                      These details are held on the account in this system, and are changed
                      where accounts are provisioned.
                    </Note>
                  )}
                </Section>
              )}
            </div>

            <div>
              <Section label="Access" first>
                {/* The two tiers are shown apart because they mean different
                    things: a tenant role covers products that do not exist yet,
                    a product role names one. */}
                <Field label="Across the tenant">
                  {tenantRoles.length ? (
                    <Space size={[7, 6]} wrap>
                      {tenantRoles.map((r) => <RoleTag key={r} role={r} />)}
                    </Space>
                  ) : (
                    <Muted>None - this account's access is per product</Muted>
                  )}
                </Field>
              </Section>

              {/*
                ONE ROW PER PRODUCT, not one chip per grant.

                This was a vertical stack of `product` in bold with its roles
                jammed against it, and with ten products it was ten ragged lines
                whose names started at ten different places. A person scanning
                for one product name was scanning a shape that moved.

                Two columns on a grid: the name on the left, aligned, and what
                is held on it on the right. It reads as a table because it is
                one, and it stays readable at ten products and at one.
              */}
              <Section label={`Products${products.length ? ` (${products.length})` : ''}`}>
                <ProductAccessList products={products} />
              </Section>

              <Section label="What that allows">
                <PermissionSummary />
              </Section>

            </div>
          </div>
        )}

        {/*
          APPEARANCE IS NOT PART OF THE ACCOUNT, so it does not sit in the
          account's columns.

          It was the last section of the right-hand column, which said two
          wrong things at once: that changing the theme is the same KIND of
          fact as which roles somebody holds, and - because it is the tallest
          block on the page - it made that column run far past the left one,
          leaving a tall empty run under Directory.

          Full width, below everything, after a rule. This is a preference this
          browser remembers; everything above it is what the tenant says about
          this person.
        */}
        <div style={{ maxWidth: 960 }}>
          <Section label="Appearance">
            <AppearanceSettings />
          </Section>
        </div>
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

/**
 * One role, named as a person would say it, explaining itself on hover.
 *
 * The raw identifier is kept in the tooltip rather than thrown away: it is what
 * an administrator types into `config/users/users.yaml` and greps
 * `config/access/policies` for, so the page that shows somebody their access
 * should not be the one place that identifier is unavailable.
 */
function RoleTag({ role }: { role: string }) {
  const meaning = roleMeaning(role)
  return (
    <Tooltip
      title={
        <span>
          {meaning}
          <span style={{ display: 'block', marginTop: 4, fontFamily: mono, opacity: 0.75 }}>
            {role}
          </span>
        </span>
      }
    >
      {/*
        marginInlineEnd is zeroed because Ant Design's own 8px right margin on
        a Tag is a MARGIN, which the flex gap of the Space around these adds to
        - so the last tag on a line sat 8px further from the edge than the
        others. The consequence is that the parent's gap is then the only gap
        there is, which is why those are [7, 6] and not 4: two bordered tags
        four pixels apart read as one tag with a line through it.
      */}
      <Tag style={{ marginInlineEnd: 0, cursor: 'default' }}>{roleLabel(role)}</Tag>
    </Tooltip>
  )
}

type ProductAccess = ReturnType<typeof productAccess>

/**
 * The products, on the identity band, with the rest COUNTED rather than
 * printed.
 *
 * Three is the cut, and it is not arbitrary: three chips and a count fit on one
 * line beside a standing chip at the narrowest width this page is designed for,
 * and a fourth wraps. The overflow is not hidden - it is one click, and the
 * full list is laid out in full further down the same page.
 */
function ProductChips({ products }: { products: ProductAccess }) {
  if (products.length === 0) return null
  const shown = products.slice(0, 3)
  const rest = products.slice(3)

  return (
    <>
      {shown.map((p) => (
        <Tooltip key={p.product} title={`${roleLabel(p.roles[0] ?? '')} of ${p.product}`}>
          <Tag style={{ marginInlineEnd: 0, cursor: 'default' }}>{p.product}</Tag>
        </Tooltip>
      ))}
      {rest.length > 0 && (
        <Popover
          placement="bottomLeft"
          title={`${rest.length} more product${rest.length === 1 ? '' : 's'}`}
          content={
            <div style={{ maxHeight: 260, overflow: 'auto', minWidth: 200 }}>
              <ProductAccessList products={rest} compact />
            </div>
          }
        >
          <Tag
            style={{ marginInlineEnd: 0, cursor: 'pointer', borderStyle: 'dashed' }}
          >
            +{rest.length} more
          </Tag>
        </Popover>
      )}
    </>
  )
}

/**
 * Every product and what is held on it, as a two-column list.
 *
 * The name column is a fixed minimum so every name starts at the same x: a
 * reader scanning ten products for one of them is scanning a straight edge
 * rather than a ragged one, which is the whole difference between a list that
 * can be read at a glance and one that has to be read line by line.
 */
function ProductAccessList({ products, compact }: { products: ProductAccess; compact?: boolean }) {
  if (products.length === 0) {
    return (
      <Muted>
        No product access. Ask an administrator for the products you need; a grant reaches this
        screen within about fifteen minutes, when this session&rsquo;s token is next renewed.
      </Muted>
    )
  }
  return (
    <div style={{ display: 'grid', gap: compact ? 6 : 0 }}>
      {products.map((p, i) => (
        <div
          key={p.product}
          style={{
            display: 'grid',
            gridTemplateColumns: 'minmax(0, 1fr) auto',
            gap: 12,
            alignItems: 'center',
            // Room between a product and the next one. At 2px of gap and 5px
            // of padding the rows sat on top of each other, so two products
            // read as one wrapped line rather than as two grants.
            padding: compact ? 0 : '8px 0',
            // The rule SEPARATES rows, so the last one has nothing to separate
            // from: a trailing border under the final product drew a line
            // across the section that looked like the start of another.
            borderBottom: compact || i === products.length - 1
              ? undefined
              : `1px solid ${c.border}`,
          }}
        >
          <Typography.Text
            style={{ fontSize: 13.5, minWidth: 0 }}
            ellipsis={{ tooltip: p.product }}
          >
            {p.product}
          </Typography.Text>
          <Space size={[7, 6]} wrap>
            {p.roles.map((r) => <RoleTag key={r} role={r} />)}
          </Space>
        </div>
      ))}
    </div>
  )
}

/**
 * WHAT THE ROLES ACTUALLY ALLOW, from the server rather than from a table
 * written here.
 *
 * A person looking at "Owner" cannot tell whether that includes promoting a
 * release, and the answer is not guessable from the word - it is a rule in
 * `config/access/policies`. So the permissions the Coordinator resolved are
 * shown, grouped by the thing they act on, which is how they are named:
 * `software_download.promote` is `promote` under Software download.
 *
 * Grouped rather than listed flat because a flat list is thirty slugs and
 * nobody reads thirty slugs; grouped it is eight subjects with two or three
 * verbs each, which is a shape somebody can scan for the verb they came for.
 */
function PermissionSummary() {
  const { who, accessUnavailable } = useIdentity()

  if (accessUnavailable) {
    return (
      <Alert
        type="warning"
        showIcon
        message="Permissions could not be resolved"
        description={
          'The policy engine did not answer, so this account\u2019s permissions are unknown - not empty. '
          + 'Requests are being refused while that is true. This is a deployment fault rather than '
          + 'anything about this account.'
        }
      />
    )
  }

  const global = who?.access?.global ?? []
  const byProduct = who?.access?.byProduct ?? {}
  // The union of everything held anywhere, because this section answers "what
  // can I do", and the per-product breakdown is the list above it.
  const all = new Set<string>([...global, ...Object.values(byProduct).flat()])
  if (all.size === 0) {
    return <Muted>None yet. Access is granted by an administrator.</Muted>
  }

  const groups = new Map<string, string[]>()
  for (const permission of [...all].sort()) {
    const [subject, verb] = splitPermission(permission)
    groups.set(subject, [...(groups.get(subject) ?? []), verb])
  }

  return (
    <div style={{ display: 'grid', gap: 2 }}>
      {[...groups].map(([subject, verbs]) => (
        <div
          key={subject}
          style={{
            display: 'grid',
            gridTemplateColumns: 'minmax(120px, 156px) 1fr',
            gap: 12,
            alignItems: 'baseline',
            padding: '5px 0',
            fontSize: 13.5,
          }}
        >
          <div style={{ color: c.text2 }}>{subjectLabel(subject)}</div>
          <div style={{ color: c.text }}>{verbs.map(verbLabel).join(', ')}</div>
        </div>
      ))}
    </div>
  )
}

/** `software_download.promote` -> `['software_download', 'promote']`. */
function splitPermission(permission: string): [string, string] {
  const dot = permission.indexOf('.')
  if (dot === -1) return [permission, permission]
  return [permission.slice(0, dot), permission.slice(dot + 1)]
}

/** The resource, as this product's own screens name it. */
function subjectLabel(subject: string): string {
  switch (subject) {
    case 'product':
      return 'Products'
    case 'package':
      return 'Releases'
    case 'software_download':
      return 'Downloads'
    case 'download_rule':
      return 'Download rules'
    case 'replication':
      return 'Mirrors'
    case 'security_report':
      return 'Security'
    case 'compliance_report':
      return 'Compliance'
    case 'audit_event':
      return 'Audit trail'
    case 'report':
      return 'Reports'
    case 'worker':
      return 'Workers'
    case 'policy_catalogue':
      return 'Policy catalogue'
    case 'system':
      return 'Deployment'
    default:
      return subject
  }
}

/** `check_connectivity` -> `check connectivity`. */
function verbLabel(verb: string): string {
  return verb.replace(/_/g, ' ')
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
