import { useEffect, useState } from 'react'
import { supportContact } from './session'

/**
 * Who to ask, when this deployment has said.
 *
 * Shared by every screen that has to refuse somebody, so the sentence and the
 * behaviour are the same wherever they meet it: the one who signed in and holds
 * no roles, and the one the identity provider would not sign in at all.
 */
export function useSupportContact(): string {
  const [contact, setContact] = useState('')
  useEffect(() => {
    let live = true
    void supportContact().then((c) => {
      if (live) setContact(c)
    })
    return () => {
      live = false
    }
  }, [])
  return contact
}

/**
 * A configured contact is rendered as something clickable, because an address
 * somebody has to retype is an address somebody mistypes. With none configured
 * the sentence still completes and simply names no route: inventing one sends
 * people to a mailbox nobody reads.
 */
export function AccessRoute({ contact }: { contact: string }) {
  if (!contact) return <>Request access from an administrator.</>
  const href = /^https?:\/\//i.test(contact) ? contact : `mailto:${contact}`
  return (
    <>
      Request access from <a href={href}>{contact}</a>.
    </>
  )
}
