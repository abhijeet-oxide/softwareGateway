import { useEffect, useState, type ReactNode } from 'react'
import { completeSignIn, beginSignIn, isCallback } from './session'
import { AccessRoute, useSupportContact } from './contact'
import { signInFailure } from './signinfailure'
import brand from '../brand'
import { AccessDeniedArt, BootSplash, c, SignedOutArt, StatusScreen } from '../uikit'

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

  if (failure) return <SignInRefused raw={failure} />
  if (!settled) return <BootSplash brand={brand} label="Signing in" />
  return <>{children}</>
}

/**
 * The identity provider would not sign this person in.
 *
 * Said in the terms the reader is in - turned away at a door - rather than in
 * the provider's own vocabulary. See signInFailure for which codes have a
 * meaning here and why the rest keep their own words.
 *
 * `Try again` stays the primary action even for an account that does not
 * exist: it is exactly what somebody does once an administrator has added
 * them, and it is the only thing on this screen that can ever succeed.
 */
function SignInRefused({ raw }: { raw: string }) {
  const contact = useSupportContact()
  const failure = signInFailure(raw)
  return (
    <StatusScreen
      brand={brand}
      /* A refusal must not be drawn with a tick. SignedOutArt carries a green
         check - correct for "you have signed out", wrong on a door somebody
         was turned away from, where it reads as success. `offerContact` is
         exactly the set of refusals: an account that was not recognised,
         disabled, or locked. */
      art={failure.offerContact ? <AccessDeniedArt size={140} /> : <SignedOutArt size={140} />}
      title={failure.title}
      actions={[{ label: 'Try again', primary: true, onClick: () => void beginSignIn() }]}
      note={failure.offerContact ? <AccessRoute contact={contact} /> : undefined}
    >
      <>
        <div>{failure.body}</div>
        {failure.reference && (
          /* The provider's own words, kept for whoever is called about it and
             set apart from the sentence so it does not read as one. */
          <div style={{ marginTop: 12, fontSize: 12, color: c.text3, wordBreak: 'break-word' }}>
            {failure.reference}
          </div>
        )}
      </>
    </StatusScreen>
  )
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
