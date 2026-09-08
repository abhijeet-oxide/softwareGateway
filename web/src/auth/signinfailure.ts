/**
 * What a failed sign-in is SAID to be.
 *
 * The identity provider answers in its own vocabulary, and that vocabulary is
 * not English. A person who picked their account and was turned away was shown
 *
 *	Errors.User.NotFound
 *
 * which is a key out of ZITADEL's translation table. It tells the reader
 * nothing, looks like the product has broken, and hides the one fact that
 * would have helped: this system is closed, and the account has not been added
 * to it.
 *
 * So the codes that have a MEANING here are given one. Everything else keeps
 * the provider's own words, quietly, as a reference - because an unexpected
 * failure still has to be diagnosable by whoever is called about it, and
 * inventing a friendly sentence for a fault nobody has seen would be worse
 * than saying so.
 */
export interface SignInFailure {
  title: string
  /** One sentence, in the register of a door rather than a stack trace. */
  body: string
  /** The provider's own text, shown as a reference when it means nothing. */
  reference?: string
  /** Whether the contact line is worth showing: only when a person can act. */
  offerContact: boolean
}

/**
 * Matched on a SUBSTRING rather than equality, because a provider is free to
 * decorate its own code - ZITADEL appends an internal id in some paths, so the
 * body arrives as `Errors.User.NotFound (AUTH-3n8fs)` on one route and bare on
 * another, and an equality test quietly stops matching on an upgrade.
 */
const KNOWN: { match: RegExp; failure: Omit<SignInFailure, 'reference'> }[] = [
  {
    // Nobody has added this person. It is the expected answer for a system
    // where accounts are provisioned rather than created by signing in, and it
    // is the one every new joiner will meet.
    match: /Errors\.User\.NotFound|user not found/i,
    failure: {
      title: 'This account is not recognised',
      body: 'Access is granted by an administrator, and this account has not been added.',
      offerContact: true,
    },
  },
  {
    // Added once, and switched off since. A different fact, and the reader
    // should not be told to ask for an account they already have.
    match: /Errors\.User\.NotActive|Errors\.User\.Inactive|user is not active/i,
    failure: {
      title: 'This account is disabled',
      body: 'The account exists and has been switched off.',
      offerContact: true,
    },
  },
  {
    match: /Errors\.User\.Locked|user is locked/i,
    failure: {
      title: 'This account is locked',
      body: 'The account has been locked at the identity provider.',
      offerContact: true,
    },
  },
  {
    // The person changed their mind at the provider's consent screen. Not a
    // fault, and nothing for an administrator to do about it.
    match: /access_denied|Errors\.User\.ExternalIDP\.LoginUserMismatch/i,
    failure: {
      title: 'Sign-in was cancelled',
      body: 'The sign-in was not completed at the identity provider.',
      offerContact: false,
    },
  },
]

export function signInFailure(raw: string): SignInFailure {
  const text = (raw || '').trim()
  for (const { match, failure } of KNOWN) {
    if (match.test(text)) return failure
  }
  return {
    title: 'Sign-in did not complete',
    body: 'The identity provider did not complete the sign-in.',
    // Kept, because this is the branch for a fault nobody anticipated and the
    // provider's own words are the only evidence there is.
    reference: text || undefined,
    offerContact: false,
  }
}
