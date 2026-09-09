import { createContext, useContext, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import type { WhoAmIResponse } from '../api/types'

/**
 * What the caller may do, asked in ONE place.
 *
 * # Why this exists before authentication does
 *
 * Today `/whoami` answers `anonymous` with permissions `["*"]`, so every check
 * here returns true and nothing is hidden. The point is not what it returns -
 * it is that every mutating control in the application already asks.
 *
 * Without this, switching on roles means opening all ten pages and every
 * button on them. With it, the resolver changes and the pages do not. See the
 * forward-design section of the UI plan, and docs/design/09 §10.
 *
 * The other half of the rule lives on the server: the UI never removes a ROW
 * for authorization reasons, because that would break pagination and leak the
 * shape of what it hid. It gates ACTIONS. Filtering is the server's job.
 */

/** Matches middleware.Action in internal/api/middleware/scope.go. */
export type Action = 'read' | 'operate' | 'apply' | 'admin'

/** What an action is being attempted on. Empty means estate-wide. */
export interface Scope {
  tenant?: string
  product?: string
}

interface Identity {
  who: WhoAmIResponse | undefined
  loading: boolean
  can: (action: Action, scope?: Scope) => boolean
}

const IdentityContext = createContext<Identity>({
  who: undefined,
  loading: true,
  // Before the answer arrives, permit nothing. A control that flickers from
  // enabled to disabled is worse than one that arrives disabled and enables.
  can: () => false,
})

export function IdentityProvider({ children }: { children: ReactNode }) {
  const { data, isLoading } = useQuery({
    queryKey: ['whoami'],
    queryFn: () => api.get<WhoAmIResponse>('/whoami'),
    // Identity does not change while a page is open, and re-asking on every
    // window focus would be a request per tab switch for an answer that is
    // constant.
    staleTime: Infinity,
    retry: false,
  })

  /*
    THE SAME RULE THE SERVER APPLIES, in the same order: Scope.covers.

    A tenant-wide permission covers every product, including products that do
    not exist yet - that is what the org tier is for. A product permission
    covers the product it names and nothing else, so it can only answer a
    question that NAMES a product: "may I operate", asked with no product, is
    the estate-wide question and a caller scoped to one product cannot answer
    it. The server refuses exactly there.

    The two lists are read separately on purpose. They used to be one flat list
    of verbs beside one flat list of products, and a caller who read product A
    and owned product B held four verbs and two products - which paired up as
    four verbs on BOTH, lighting up "approve download" on a product they may
    only read. The server refused it, so the screen offered what the API then
    denied.
  */
  const can = (action: Action, scope?: Scope): boolean => {
    if (!data) return false
    const tenantWide = data.permissions ?? []
    if (tenantWide.includes('*')) return true
    if (tenantWide.includes(action)) return true
    if (!scope?.product) return false
    return (data.productPermissions?.[scope.product] ?? []).includes(action)
  }

  return (
    <IdentityContext.Provider value={{ who: data, loading: isLoading, can }}>
      {children}
    </IdentityContext.Provider>
  )
}

/** The whole identity, for the Settings page. */
export function useIdentity(): Identity {
  return useContext(IdentityContext)
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

/**
 * Whether this caller may do this thing, here.
 *
 * Every Download, Run Discovery, Retry, Pause, Stop, Apply and Promote control
 * is wrapped in this. Pass the product whenever there is one - a check that
 * names no scope is the strictest question, not the loosest.
 */
export function useCan(action: Action, scope?: Scope): boolean {
  return useContext(IdentityContext).can(action, scope)
}
