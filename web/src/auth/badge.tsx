import type { ReactNode } from 'react'
import { EyeIcon, LockIcon, ShieldCheckIcon, ShieldIcon, UserIcon, WrenchIcon } from '../uikit'
import { accountKind, type AccountKind } from './roles'
import type { WhoAmIResponse } from '../api/types'

/**
 * WHAT KIND of account this is, as one glyph.
 *
 * # Why the picture lives here and the kind lives in roles.ts
 *
 * Because the tenant ladder is a rule about access and this is a rendering
 * decision about it, and the two change for different reasons: a tier added to
 * the ladder is a policy change, a glyph swapped for a clearer one is not. What
 * must not happen is a second walk of the ladder - a switch on roles for the
 * word and another for the icon - because then an account can read "Operator"
 * beside an administrator's shield. `accountKind` is the single answer both
 * read.
 *
 * # Why a shared module rather than one per surface
 *
 * Two surfaces name an account's standing: the avatar at the navigation's foot
 * and the chip at the top of the profile page. A reader moving between them
 * should be looking at ONE thing. Written twice, they would agree until
 * somebody improved one of them.
 *
 * The shapes are chosen to differ at 9px, where only the outline survives: a
 * shield with a tick, a spanner, a plain shield, an eye, a person, a padlock.
 * The word is always beside it - this is what makes the word findable, not a
 * replacement for it.
 */
const BADGES: Record<AccountKind, ReactNode> = {
  admin: <ShieldCheckIcon />,
  operator: <WrenchIcon />,
  security: <ShieldIcon />,
  reader: <EyeIcon />,
  user: <UserIcon />,
  // Provisioned and granted nothing. The padlock is the reason every control
  // on screen is disabled, said in a picture.
  none: <LockIcon />,
}

/**
 * The glyph for this account, or nothing until we know who it is.
 *
 * UNDEFINED while the answer has not arrived, for the reason the navigation
 * shows every entry until then: not knowing is not a refusal. A padlock drawn
 * over an identity that has simply not loaded yet tells somebody their account
 * has no access, a moment before it turns out to be an administrator's.
 */
export function accountBadge(who: WhoAmIResponse | undefined): ReactNode {
  return who ? BADGES[accountKind(who)] : undefined
}
