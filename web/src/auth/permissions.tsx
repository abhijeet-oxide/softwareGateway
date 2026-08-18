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
 * here returns true and nothing is hidden. The point is not what it returns —
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

  const can = (action: Action, scope?: Scope): boolean => {
    if (!data) return false
    const permissions = data.permissions ?? []
    if (permissions.includes('*')) return true
    if (!permissions.includes(action)) return false

    // A caller restricted to some products may act within those products, and
    // not estate-wide. An action with no product named is the estate-wide
    // question, which a narrowed caller cannot answer — the same rule the
    // server applies in Scope.covers.
    const products = data.products ?? []
    if (products.length === 0) return true
    return Boolean(scope?.product && products.includes(scope.product))
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
 * Whether this caller may do this thing, here.
 *
 * Every Download, Run Discovery, Retry, Pause, Stop, Apply and Promote control
 * is wrapped in this. Pass the product whenever there is one — a check that
 * names no scope is the strictest question, not the loosest.
 */
export function useCan(action: Action, scope?: Scope): boolean {
  return useContext(IdentityContext).can(action, scope)
}
