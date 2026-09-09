import { useCallback, useState, type ReactNode } from 'react'
import { Button, Tooltip, type ButtonProps } from 'antd'
import { LockOutlined } from '../icons'
import {
  useCan, useCanAny, useIdentity, whyDisabled, type Permission, type Scope,
} from '../auth/permissions'
import { reportFailure } from './feedback'
import { AccessDeniedArt, c, StatePanel } from '../uikit'

/**
 * THE THREE THINGS AN INTERFACE DOES WITH A PERMISSION, in one file.
 *
 * Hide a control, disable a control, or refuse a whole page. Every one of them
 * was being decided ad hoc: some controls were disabled with a tooltip, some
 * were rendered and answered 403, some were left visible with no tooltip at
 * all, and no page refused itself - a person with no access to the audit trail
 * got the Activity page, its filters, its table furniture and an empty body.
 *
 * # Which of the three, and when
 *
 * HIDDEN is the default for an ACTION, and it is what somebody reading the
 * screen actually wants: a Run Discovery button they can never press is not
 * information, it is furniture that makes the page look like it is refusing
 * them personally. A control nobody in that role can use does not exist for
 * them.
 *
 * DISABLED is for a control whose ABSENCE would be confusing - one in a row of
 * controls, or one whose slot is part of how a panel reads. It always carries
 * the reason, and the reason always names the permission, because "you do not
 * have permission" without saying which is a sentence an administrator cannot
 * act on.
 *
 * REFUSED is for a whole page, and it is a real screen rather than an empty
 * one: what was refused, which account, and what to do about it.
 *
 * # Loading is not optional here
 *
 * `ActionButton` takes a handler that returns a promise and owns the pending
 * state itself, so a control CANNOT be shipped without one. That is the point:
 * every button that was missing a spinner was missing it because remembering
 * was left to whoever wrote it, and the failure - a button that looks dead for
 * the four seconds it is working - is invisible in review and obvious in use.
 * It reports its own failures too, through the same path everything else does.
 */

export interface ActionButtonProps extends Omit<ButtonProps, 'onClick' | 'loading'> {
  /**
   * The permission this control needs. Omit for a control that needs none -
   * it then behaves as a plain button that still owns its pending state.
   */
  permission?: Permission
  /**
   * The product it acts on. Omit and the question is asked estate-wide, which
   * is the STRICTEST form and correct for a control that acts on everything.
   */
  scope?: Scope
  /**
   * Ask whether the permission is held ANYWHERE rather than over the estate.
   *
   * For a fleet-wide control whose request the server narrows to what the
   * caller may touch - a scan across every product they own. Without it, an
   * owner of two products is refused a button the API would have accepted.
   */
  anyScope?: boolean
  /** What to do when the permission is missing. Hidden unless said otherwise. */
  whenDenied?: 'hide' | 'disable'
  /**
   * The work. Returning a promise is what drives the spinner, so an async
   * handler needs nothing else; a synchronous one simply never spins.
   */
  onClick?: () => void | Promise<unknown>
  /**
   * What this button is called in a failure report - "Run health check". The
   * toast then reads "Run health check failed" over the Coordinator's own
   * sentence, rather than a bare error with no idea what was being attempted.
   */
  action?: string
  /** What the enabled control's tooltip says. The disabled one says why not. */
  title?: string
  /**
   * Pending because of work this button did not start - a mutation shared with
   * another control, or one whose promise is awaited elsewhere.
   *
   * Merged with the spinner this button owns rather than replacing it, so a
   * caller that passes it does not accidentally turn off the automatic one.
   */
  busy?: boolean
}

/**
 * A button that knows what it needs, what it is doing, and what went wrong.
 */
export function ActionButton({
  permission, scope, anyScope, whenDenied = 'hide', onClick, action, title, busy, children, ...rest
}: ActionButtonProps) {
  const scoped = useCan(permission ?? 'product.view', scope)
  const anywhere = useCanAny(permission ?? 'product.view')
  const allowed = !permission || (anyScope ? anywhere : scoped)

  const [running, setRunning] = useState(false)

  const press = useCallback(() => {
    const result = onClick?.()
    if (!(result instanceof Promise)) return
    setRunning(true)
    void result
      // Reported centrally, so a handler that forgets to catch still tells the
      // reader what happened rather than logging to a console nobody has open.
      .catch((error: unknown) => reportFailure(error, action))
      .finally(() => setRunning(false))
  }, [onClick, action])

  if (!allowed && whenDenied === 'hide') return null

  const button = (
    <Button {...rest} disabled={rest.disabled || !allowed} loading={running || busy} onClick={press}>
      {children}
    </Button>
  )

  const tip = allowed ? title : whyDisabled(permission!, scope)
  if (!tip) return button
  // A disabled Ant button swallows pointer events, so the tooltip has to sit on
  // a wrapper - otherwise the one control that most needs to explain itself is
  // the one control with no hover.
  return (
    <Tooltip title={tip}>
      {allowed ? button : <span style={{ display: 'inline-block' }}>{button}</span>}
    </Tooltip>
  )
}

/**
 * Renders its children only for a caller who holds the permission.
 *
 * For a whole panel, a tab, a menu entry or a column - anything that is not one
 * button and should not exist for somebody who cannot use it.
 */
export function Guard({
  permission, scope, anyScope, fallback = null, children,
}: {
  permission: Permission
  scope?: Scope
  anyScope?: boolean
  /** What stands in its place. Nothing, usually: absence is the point. */
  fallback?: ReactNode
  children: ReactNode
}) {
  const { can, canAny } = useIdentity()
  const allowed = anyScope ? canAny(permission) : can(permission, scope)
  return <>{allowed ? children : fallback}</>
}

/**
 * A whole page this account may not open.
 *
 * # Why a page and not an empty table
 *
 * Because an empty table is a lie about the estate. Somebody without the audit
 * permission was shown the Activity page with its filters and a table saying
 * "no events" - which reads as "nothing has happened here", a confident
 * statement about a system they cannot see. Every screen in this application
 * that cannot answer says why it cannot, and this is the authorization one.
 *
 * It names the PERMISSION, because that is the string an administrator greps
 * `config/access/policies` for and the only part of this a reader can act on.
 */
export function AccessDenied({ permission, what }: { permission: Permission; what: string }) {
  const { who } = useIdentity()
  const account = who?.email || who?.name || who?.subject
  return (
    <StatePanel
      art={<AccessDeniedArt size={128} />}
      title={`You do not have access to ${what}`}
      subtitle={
        <span>
          This page needs the{' '}
          <code style={{ fontSize: '0.95em' }}>{permission}</code> permission, which this
          account has not been granted. Access is granted by an administrator and takes
          effect at your next sign-in.
          {account && (
            <span style={{ display: 'block', marginTop: 8, color: c.text3, fontSize: 12.5 }}>
              Signed in as {account}
            </span>
          )}
        </span>
      }
    />
  )
}

/**
 * Guards a page, and refuses it properly when the permission is missing.
 *
 * `anyScope` is the ordinary case for a listing: a caller who holds the
 * permission on one product may open the page, and the SERVER narrows what is
 * in it. Estate pages - the fleet, the rollups, the rulebook - ask the
 * tenant-wide question, because there is nothing to narrow them to.
 */
export function RequirePermission({
  permission, what, anyScope = true, children,
}: {
  permission: Permission
  /** What the page is, for the refusal: "the audit trail". */
  what: string
  anyScope?: boolean
  children: ReactNode
}) {
  const { who, can, canAny, loading } = useIdentity()
  /*
    NOT KNOWING IS NOT A REFUSAL, and the two must not look alike.

    `loading` is the ordinary case - a page that flashed a closed door and then
    opened would read as a system changing its mind. `!who` is the one that
    matters: /whoami itself failed, so we did not ask and were not told no.
    Refusing there would put "you do not have access to the audit trail" in
    front of an administrator whose network blipped, and send them to ask for a
    role they already hold.

    Rendering the page instead is not a hole. The page's own reads answer to
    the server, which authorizes every one of them, and their failures now
    surface properly - which is a far better description of "the Coordinator is
    not answering" than a permission screen is.
  */
  if (loading || !who) return <>{children}</>
  const allowed = anyScope ? canAny(permission) : can(permission)
  if (!allowed) return <AccessDenied permission={permission} what={what} />
  return <>{children}</>
}

/**
 * The mark for a control that is present and refused.
 *
 * Used where hiding is wrong and a bare disabled control would look broken -
 * a padlock says "this is deliberate" in a way greyed-out text cannot.
 */
export function DeniedMark({ title }: { title: string }) {
  return (
    <Tooltip title={title}>
      <LockOutlined style={{ color: c.text3, fontSize: 12 }} />
    </Tooltip>
  )
}
