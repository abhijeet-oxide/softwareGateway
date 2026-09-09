import { createContext, useContext, useMemo, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import type { AccessSet, WhoAmIResponse } from '../api/types'

/**
 * WHAT THIS CALLER MAY DO, asked in ONE place.
 *
 * # Why the interface is told rather than working it out
 *
 * Because the two alternatives both drift, and both drift silently.
 *
 * Deciding from ROLES - "if you hold product-owner, show Discover" - is the
 * server's authorization model reimplemented in TypeScript, in a file nobody
 * reviews against `config/access/policies`. The first policy change makes it
 * wrong, and wrong in both directions: a control offered and then refused, and
 * a control hidden from somebody perfectly entitled to press it. Both were
 * happening here.
 *
 * Deciding from the four coarse verbs - read, operate, apply, admin - is what
 * this file used to do, and it cannot express the estate boundary. "Run a scan
 * on my product" and "run a scan across the fleet" are both `operate`, so a
 * product owner either got a fleet-wide button that answered 403 or no button
 * at all. They got no button at all.
 *
 * So the server publishes the SAME questions it enforces, answered by the SAME
 * authority - the policy engine, where one is configured - and this renders
 * what it is told. A permission named here exists in the catalogue in
 * `internal/api/middleware/permissions.go` and in a rule in
 * `config/access/policies`, and a test fails the build if a route asks for one
 * that is not in all three.
 *
 * # This is not a security control
 *
 * Nothing here decides anything. Every request is authorized again on arrival
 * by the same catalogue, so a person who edits this in their browser gets a
 * screen full of controls that all answer 403. What it removes is the
 * interface offering work the API will refuse, and hiding work it would allow.
 *
 * # The one rule about hiding
 *
 * ACTIONS are gated; ROWS are not. The interface never removes a row from a
 * listing for authorization reasons - that would break pagination and leak the
 * shape of what it hid. Filtering a listing is the server's job, and the
 * server does it (`middleware.PermittedProducts`, `Identity.VisibleProducts`).
 * What this gates is controls, navigation entries and whole pages.
 */

/**
 * Every permission the server can grant, as it spells them.
 *
 * A union rather than `string`, so a typo is a build failure rather than a
 * control that is hidden from everybody forever. Kept in the order of
 * `middleware.Catalogue`; adding one here without adding it there gates a
 * control on a permission nobody can hold.
 */
export type Permission =
  | 'product.view'
  | 'product.discover'
  | 'product.calibrate'
  | 'product.check_connectivity'
  | 'package.view'
  | 'package.inspect'
  | 'software_download.view'
  | 'software_download.request'
  | 'software_download.retry'
  | 'software_download.cancel'
  | 'software_download.promote'
  | 'software_download.apply'
  | 'download_rule.view'
  | 'replication.view'
  | 'replication.sync'
  | 'replication.cancel_sync'
  | 'replication.apply'
  | 'security_report.view'
  | 'security_report.export'
  | 'compliance_report.view'
  | 'compliance_report.export'
  | 'compliance_report.run'
  | 'compliance_report.cancel'
  | 'audit_event.view'
  | 'report.view'
  | 'worker.view'
  | 'policy_catalogue.view'
  | 'system.view'
  | 'system.write'

/** What a permission is being asked about. No product is the estate-wide question. */
export interface Scope {
  product?: string
}

interface Identity {
  who: WhoAmIResponse | undefined
  loading: boolean
  /**
   * Held over this scope. With a product, the question is about that product;
   * without one it is the ESTATE-WIDE question, which is the strictest there
   * is - a caller scoped to one product cannot answer it, and the server
   * refuses them in exactly the same place.
   */
  can: (permission: Permission, scope?: Scope) => boolean
  /**
   * Held ANYWHERE - tenant-wide, or on at least one product.
   *
   * The question a navigation entry and a listing page ask: "is there anything
   * behind this door for you". The page it opens still asks `can` per product
   * for each control on it, and the server still narrows the contents.
   */
  canAny: (permission: Permission) => boolean
  /**
   * The products this permission is held on, for a chooser that must not offer
   * a product the request would then be refused for.
   *
   * `undefined` means UNRESTRICTED - held tenant-wide, so it covers every
   * product including ones created after this session started. That is a
   * different answer from the empty array, which means none.
   */
  productsWith: (permission: Permission) => string[] | undefined
  /** The policy engine could not be reached, so nothing could be resolved. */
  accessUnavailable: boolean
}

const EMPTY_ACCESS: AccessSet = { global: [] }

const IdentityContext = createContext<Identity>({
  who: undefined,
  loading: true,
  // Before the answer arrives, permit nothing. A control that flickers from
  // enabled to disabled is worse than one that arrives disabled and enables.
  can: () => false,
  canAny: () => false,
  productsWith: () => [],
  accessUnavailable: false,
})

export function IdentityProvider({ children }: { children: ReactNode }) {
  const { data, isLoading } = useQuery({
    queryKey: ['whoami'],
    queryFn: () => api.get<WhoAmIResponse>('/whoami'),
    // Identity does not change while a page is open, and re-asking on every
    // window focus would be a request per tab switch for an answer that is
    // constant. A role granted mid-session takes effect at the next sign-in,
    // which is what the profile page says.
    staleTime: Infinity,
    retry: false,
  })

  const value = useMemo<Identity>(() => {
    const access = data?.access ?? EMPTY_ACCESS
    const global = new Set(access.global ?? [])
    const byProduct = access.byProduct ?? {}

    /*
      THE SAME RULE THE SERVER APPLIES, in the same order.

      A tenant-wide permission covers every product, including products that do
      not exist yet - that is what the org tier is for. A product permission
      covers the product it names and nothing else, so it can only answer a
      question that NAMES a product.

      The two lists are read separately on purpose, and the server publishes
      them separately for the same reason: flattened into one, somebody who
      reads product A and owns product B holds every verb over both, and the
      screen lights up "promote" on a product they may only look at.
    */
    const can = (permission: Permission, scope?: Scope): boolean => {
      if (!data) return false
      if (global.has(permission)) return true
      if (!scope?.product) return false
      return (byProduct[scope.product] ?? []).includes(permission)
    }

    const canAny = (permission: Permission): boolean => {
      if (!data) return false
      if (global.has(permission)) return true
      return Object.values(byProduct).some((held) => held.includes(permission))
    }

    const productsWith = (permission: Permission): string[] | undefined => {
      if (!data) return []
      if (global.has(permission)) return undefined
      return Object.entries(byProduct)
        .filter(([, held]) => held.includes(permission))
        .map(([product]) => product)
        .sort()
    }

    return {
      who: data,
      loading: isLoading,
      can,
      canAny,
      productsWith,
      accessUnavailable: Boolean(data?.access?.unavailable),
    }
  }, [data, isLoading])

  return <IdentityContext.Provider value={value}>{children}</IdentityContext.Provider>
}

/** The whole identity, for the pages that report on it. */
export function useIdentity(): Identity {
  return useContext(IdentityContext)
}

/**
 * Whether this caller holds this permission, here.
 *
 * Every Download, Run Discovery, Retry, Pause, Stop, Apply and Promote control
 * is wrapped in this. Pass the product whenever there is one - a check that
 * names no scope is the strictest question, not the loosest.
 */
export function useCan(permission: Permission, scope?: Scope): boolean {
  return useContext(IdentityContext).can(permission, scope)
}

/** Whether this caller holds this permission anywhere at all. */
export function useCanAny(permission: Permission): boolean {
  return useContext(IdentityContext).canAny(permission)
}

/**
 * The products a permission is held on, or `undefined` for every product.
 *
 * For a chooser: offering a product the request would be refused for is the
 * same defect as offering a button that answers 403, one level up.
 */
export function useProductsWith(permission: Permission): string[] | undefined {
  return useContext(IdentityContext).productsWith(permission)
}

/**
 * What to put on a disabled control's tooltip.
 *
 * ONE sentence, written once. A refusal explained differently on every button
 * reads as several different problems, and the reader's next question - "so
 * what do I need?" - is answered by naming the permission, which is the string
 * an administrator greps `config/access/policies` for.
 */
export function whyDisabled(permission: Permission, scope?: Scope): string {
  const where = scope?.product ? ` on ${scope.product}` : ''
  return `Access denied: your account does not have the ${permission} permission${where}.`
}

/**
 * The two letters that stand in for a person where there is no avatar.
 *
 * ONE derivation, because there are two places that draw them - the navigation
 * card and the profile page - and two hand-written copies disagreed
 * immediately: "Platform Administrator" was PL in the rail and PA on the page,
 * so the same account was two people on one screen. Words first, not
 * characters: an address, a login name and a display name all split on the
 * same separators.
 */
export function initialsOf(of: string | undefined): string {
  const letters = (of ?? '')
    .split(/[\s@._-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]?.toUpperCase() ?? '')
    .join('')
  return letters || '?'
}
