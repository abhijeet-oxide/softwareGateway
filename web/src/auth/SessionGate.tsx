import { useEffect, useState, type ReactNode } from 'react'
import { completeSignIn, beginSignIn, isCallback } from './session'
import brand from '../brand'
import { BootSplash, SignedOutArt, StatusScreen } from '../uikit'

/**
 * The landing place after the identity provider, and nothing else.
 *
 * It sits ABOVE every read in the application on purpose. The code the issuer
 * sends back is single use, and anything that fires a request while the
 * exchange is in flight gets a 401, which starts a second sign-in, which
 * navigates away from the callback before the first one finished. So no read
 * may run until this has settled.
 *
 * On every other address it renders its children and costs one comparison.
 */
export function SessionGate({ children }: { children: ReactNode }) {
  const [failure, setFailure] = useState<string>()
  const [settled, setSettled] = useState(!isCallback())

  useEffect(() => {
    if (!isCallback()) return
    let live = true
    exchangeOnce()
      .then((returnTo) => {
        if (!live) return
        // replaceState rather than a route change: the callback address
        // carries a spent authorization code, and leaving it in the history
        // means Back returns to a sign-in that can no longer be completed.
        window.history.replaceState(null, '', returnTo)
        setSettled(true)
      })
      .catch((err: unknown) => {
        if (live) setFailure(err instanceof Error ? err.message : String(err))
      })
    return () => {
      live = false
    }
  }, [])

  if (failure) {
    return (
      <StatusScreen
        brand={brand}
        art={<SignedOutArt size={140} />}
        title="Sign-in did not complete"
        actions={[{ label: 'Try again', primary: true, onClick: () => void beginSignIn() }]}
      >
        {failure}
      </StatusScreen>
    )
  }
  if (!settled) return <BootSplash brand={brand} label="Signing in" />
  return <>{children}</>
}

/**
 * The exchange, at most once per page load.
 *
 * React runs an effect twice in development, and the second run would spend an
 * authorization code the first one had already redeemed - so the screen would
 * report a failed sign-in that had in fact succeeded.
 */
let exchange: Promise<string> | undefined
function exchangeOnce(): Promise<string> {
  exchange ??= completeSignIn()
  return exchange
}
