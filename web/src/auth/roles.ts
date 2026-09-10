import type { WhoAmIResponse } from '../api/types'

/**
 * How a person's access is NAMED on screen.
 *
 * # The problem this solves
 *
 * Somebody holding org-admin, org-member and a role on ten products holds
 * twelve role strings. The navigation card printed all twelve, comma
 * separated, under their name in a rail two hundred pixels wide: three lines
 * of `software-01: product-owner, software-02: product-owner, ...` wrapped
 * into a paragraph, in the one place on screen whose job is to say who you
 * are. The profile page printed the same twelve as a wall of identical grey
 * chips.
 *
 * Twelve strings is not twelve facts. It is ONE fact - what this person is
 * here - plus a list of products, and the two belong in different places: the
 * standing they hold goes under their name, and the products go on the page
 * about their access, where there is room to arrange them.
 *
 * # Why a ladder and not a set
 *
 * The tiers are ordered by design (docs/design/24 §5): an org admin can do
 * everything an org operator can. So the roles somebody holds have a MAXIMUM,
 * and that maximum is the answer to "what are you here". Listing the rest
 * describes the grants rather than the person.
 */

/**
 * WHAT KIND of account this is, as one value.
 *
 * The discriminant exists so that the WORD and the GLYPH come out of one
 * ladder. They were about to be two: a switch on roles here for the label and
 * another in the shell for the icon, walking the same ladder in the same order
 * for the same answer - which is two places to add a tier to, and the failure
 * mode is a card reading "Operator" beside an administrator's shield.
 */
export type AccountKind =
  | 'admin'
  | 'operator'
  | 'security'
  | 'reader'
  /** Access is a set of products rather than a tenant role. */
  | 'user'
  /** Provisioned and granted nothing, or not signed in at all. */
  | 'none'

/** The tenant-wide roles, strongest first. The order is the ladder. */
const TENANT_LADDER: { role: string; kind: AccountKind; label: string }[] = [
  { role: 'org-admin', kind: 'admin', label: 'Admin' },
  { role: 'org-operator', kind: 'operator', label: 'Operator' },
  { role: 'org-security', kind: 'security', label: 'Security' },
  { role: 'org-reader', kind: 'reader', label: 'Reader' },
]

/**
 * Which kind of account this is.
 *
 * A tenant role names it outright, by the ladder above. An account whose
 * access is entirely per-product is a `user`: which products, and what on
 * each, is the profile page's subject and does not belong in a rail.
 * `org-member` names no permission at all - it is the marker that somebody was
 * provisioned - so it never decides this on its own.
 */
export function accountKind(who: WhoAmIResponse | undefined): AccountKind {
  if (!who?.authenticated) return 'none'
  const roles = who.roles ?? []
  const top = TENANT_LADDER.find((entry) => roles.includes(entry.role))
  if (top) return top.kind
  if (Object.keys(who.productRoles ?? {}).length > 0) return 'user'
  return 'none'
}

/**
 * ONE WORD for what this person is, for the navigation card.
 *
 * Read from the same ladder accountKind uses, so the word and the glyph beside
 * it cannot disagree.
 */
export function accountLabel(who: WhoAmIResponse | undefined): string {
  if (!who) return ''
  if (!who.authenticated) return 'Not signed in'
  const roles = who.roles ?? []
  const top = TENANT_LADDER.find((entry) => roles.includes(entry.role))
  if (top) return top.label
  if (Object.keys(who.productRoles ?? {}).length > 0) return 'User'
  // Provisioned and granted nothing yet. Said plainly, because it is the
  // reason every control on screen is disabled and the person needs to know it
  // is their account rather than the system.
  return who.member ? 'No access granted' : 'No access'
}

/**
 * A role identifier as a person would say it.
 *
 * `product-owner` is the wire's word and it is fine in a policy file; on a
 * page about somebody's access it reads as a slug. The tier prefix is dropped
 * where the surrounding context already supplies it - a chip inside a product's
 * row does not need to repeat that it is a product role.
 */
export function roleLabel(role: string): string {
  switch (role) {
    case 'org-admin':
      return 'Organisation admin'
    case 'org-operator':
      return 'Organisation operator'
    case 'org-security':
      return 'Security'
    case 'org-reader':
      return 'Organisation reader'
    case 'org-member':
      return 'Member'
    case 'product-owner':
      return 'Owner'
    case 'product-operator':
      return 'Operator'
    case 'product-reader':
      return 'Reader'
    default:
      return role
  }
}

/**
 * What a role is FOR, on hover.
 *
 * A chip that says "Owner" and nothing else makes a reader guess at the
 * boundary between it and "Operator", and the boundary is exactly the thing
 * that decides whether a button is there. These sentences are the policies'
 * own division, in words.
 */
export function roleMeaning(role: string): string {
  switch (role) {
    case 'org-admin':
      return 'Everything in this tenant, including products added later.'
    case 'org-operator':
      return 'Request work across every product: downloads, discovery, syncs. Not registry configuration.'
    case 'org-security':
      return 'Read security findings and the audit trail across every product.'
    case 'org-reader':
      return 'Read every product, release and download. No changes.'
    case 'org-member':
      return 'Provisioned in this tenant. Grants nothing on its own.'
    case 'product-owner':
      return 'Everything on this product, including promotions and registry configuration.'
    case 'product-operator':
      return 'Request work on this product: downloads, discovery, syncs.'
    case 'product-reader':
      return 'Read this product. No changes.'
    default:
      return ''
  }
}

/** Strongest first, so a product's chips lead with what it grants most. */
const PRODUCT_LADDER = ['product-owner', 'product-operator', 'product-reader']

/**
 * One product and what is held on it, ordered so the list is stable and reads
 * strongest first.
 *
 * Sorted by product name rather than by role: a person looking for one product
 * in a list of ten scans for the name, and an order that moves as roles change
 * makes that scan fail.
 */
export function productAccess(
  who: WhoAmIResponse | undefined,
): { product: string; roles: string[]; label: string }[] {
  return Object.entries(who?.productRoles ?? {})
    .map(([product, roles]) => {
      const ordered = [...roles].sort(
        (a, b) => indexIn(PRODUCT_LADDER, a) - indexIn(PRODUCT_LADDER, b),
      )
      return { product, roles: ordered, label: roleLabel(ordered[0] ?? '') }
    })
    .sort((a, b) => a.product.localeCompare(b.product))
}

function indexIn(ladder: string[], value: string): number {
  const i = ladder.indexOf(value)
  return i === -1 ? ladder.length : i
}
