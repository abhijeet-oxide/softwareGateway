/**
 * The browser's half of signing in: OpenID Connect authorization code with
 * PKCE, against whichever issuer this deployment was configured with.
 *
 * # Why this is hand-rolled rather than a library
 *
 * The whole flow is four HTTP interactions and one hash, and a library would
 * bring its own storage model, its own silent-renew iframe and its own opinion
 * about routing - three things this application already has answers for. What
 * is here is the code path this product actually uses and nothing else.
 *
 * # The shape of it
 *
 * The SERVER decides whether authentication is required. Nothing here guesses:
 * the application loads, makes its first read, and a 401 is what starts a
 * sign-in. That is why a Coordinator running with SWGW_AUTH_ENABLED=false
 * needs no flag in the browser to keep working.
 *
 *   1. a read comes back 401           -> requireSignIn()
 *   2. the browser goes to the issuer  -> the person signs in there
 *   3. the issuer returns a code       -> completeSignIn() exchanges it
 *   4. every request carries the token -> authorization()
 *   5. the token expires               -> renewSession(), once, before failing
 *
 * # Where the tokens live
 *
 * sessionStorage, not localStorage: a token is scoped to the tab that obtained
 * it and does not outlive the browser being closed. A second tab starts its
 * own sign-in, which is silent when the issuer's own session is still valid.
 *
 * The refresh token is held there too. That is a deliberate trade and worth
 * stating: this is a PUBLIC client, so it has no secret to protect and a
 * refresh token is the only thing that keeps a long shift from being
 * interrupted by a redirect every hour. Anything script-readable is readable
 * by injected script, which is what the strict same-origin API surface and the
 * absence of third-party script on this origin are there to prevent.
 */

/** What the deployment published at /runtime-config.json. */
export interface AuthConfig {
  issuer: string
  clientId: string
  /** the redirect the issuer will accept, as registered. */
  redirectUri: string
}

/** The endpoints the issuer advertises. */
interface Endpoints {
  authorization: string
  token: string
  endSession: string | undefined
}

interface Tokens {
  accessToken: string
  refreshToken: string | undefined
  /** epoch milliseconds */
  expiresAt: number
}

const TOKENS_KEY = 'swgw.auth.tokens'
const PENDING_KEY = 'swgw.auth.pending'
const OBTAINED_KEY = 'swgw.auth.obtained'

/**
 * How close to expiry a token may be and still be used.
 *
 * Sixty seconds because the alternative is a request that leaves here valid
 * and arrives expired, which fails as a 401 the reader sees rather than as a
 * renewal nobody notices.
 */
const EXPIRY_SKEW_MS = 60_000

/**
 * How long after a completed sign-in a 401 stops meaning "sign in".
 *
 * A token that the issuer minted and the Coordinator rejects is a
 * CONFIGURATION fault - a mismatched issuer, audience or clock - and sending
 * the browser back to the issuer cannot fix it. Without this the two of them
 * bounce the reader between them forever and the screen never says why.
 */
const REJECT_WINDOW_MS = 60_000

// --- the state the screens read ---------------------------------------------

/**
 * Why the application cannot proceed, when it cannot.
 *
 *   redirecting  - the browser is on its way to the issuer
 *   unconfigured - the Coordinator wants a token and this deployment never
 *                  published an issuer to get one from
 *   rejected     - a token was obtained and the Coordinator refused it anyway
 */
export type SignInStatus = 'idle' | 'redirecting' | 'unconfigured' | 'rejected'

let status: SignInStatus = 'idle'
const listeners = new Set<() => void>()

function setStatus(next: SignInStatus): void {
  if (status === next) return
  status = next
  for (const l of listeners) l()
}

/** For useSyncExternalStore. */
export function subscribeSignIn(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function signInStatus(): SignInStatus {
  return status
}

// --- storage ----------------------------------------------------------------

/**
 * Reads and writes never throw.
 *
 * Storage is unavailable in a browser configured to block it, and a sign-in
 * that dies on a DOMException while STORING a token it already holds is worse
 * than one that simply cannot be remembered.
 */
function read(key: string): string | null {
  try {
    return sessionStorage.getItem(key)
  } catch {
    return null
  }
}

function write(key: string, value: string): void {
  try {
    sessionStorage.setItem(key, value)
  } catch {
    // The session survives in memory; it will not survive a reload.
  }
}

function forget(key: string): void {
  try {
    sessionStorage.removeItem(key)
  } catch {
    // Nothing to do, and nothing depends on it having worked.
  }
}

/** Held in memory as well as in storage, so a blocked store still works. */
let tokens: Tokens | undefined = loadTokens()

function loadTokens(): Tokens | undefined {
  const raw = read(TOKENS_KEY)
  if (!raw) return undefined
  try {
    const parsed = JSON.parse(raw) as Tokens
    return parsed.accessToken ? parsed : undefined
  } catch {
    return undefined
  }
}

function keepTokens(next: Tokens | undefined): void {
  tokens = next
  if (next) write(TOKENS_KEY, JSON.stringify(next))
  else forget(TOKENS_KEY)
}

// --- configuration and discovery --------------------------------------------

let configPromise: Promise<AuthConfig | null> | undefined

/**
 * The deployment's OIDC settings, fetched once.
 *
 * Written by the web container at start (deploy/web/docker-entrypoint.sh) from
 * a value ZITADEL generates, which is why it cannot be a build-time constant.
 * Absent or empty means this deployment did not configure a sign-in, which is
 * the normal state of a development server.
 */
export function authConfig(): Promise<AuthConfig | null> {
  configPromise ??= (async () => {
    try {
      const response = await fetch('/runtime-config.json', {
        headers: { Accept: 'application/json' },
        cache: 'no-store',
      })
      if (!response.ok) return null
      const doc = (await response.json()) as { oidc?: Partial<AuthConfig> }
      const oidc = doc.oidc
      if (!oidc?.issuer || !oidc.clientId) return null
      return {
        issuer: oidc.issuer.replace(/\/$/, ''),
        clientId: oidc.clientId,
        // The registered redirect wins over one derived here: it is what the
        // issuer will actually accept, and a derived one that differs by a
        // port fails at the issuer with a message about a URI nobody typed.
        redirectUri: oidc.redirectUri || `${window.location.origin}/auth/callback`,
      }
    } catch {
      return null
    }
  })()
  return configPromise
}

let endpointsPromise: Promise<Endpoints | null> | undefined

/**
 * The issuer's own endpoints, from its discovery document.
 *
 * Read rather than assembled: the paths differ between identity providers, and
 * a deployment is free to put a different one behind the same variable.
 */
function endpoints(config: AuthConfig): Promise<Endpoints | null> {
  endpointsPromise ??= (async () => {
    try {
      const response = await fetch(`${config.issuer}/.well-known/openid-configuration`, {
        headers: { Accept: 'application/json' },
      })
      if (!response.ok) return null
      const doc = (await response.json()) as Record<string, string>
      if (!doc.authorization_endpoint || !doc.token_endpoint) return null
      return {
        authorization: doc.authorization_endpoint,
        token: doc.token_endpoint,
        endSession: doc.end_session_endpoint,
      }
    } catch {
      return null
    }
  })()
  return endpointsPromise
}

// --- PKCE -------------------------------------------------------------------

function base64url(bytes: ArrayBuffer | Uint8Array): string {
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes)
  let binary = ''
  for (const byte of view) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function randomString(): string {
  return base64url(crypto.getRandomValues(new Uint8Array(32)))
}

async function challengeFor(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier))
  return base64url(digest)
}

// --- starting a sign-in -----------------------------------------------------

interface Pending {
  verifier: string
  state: string
  /** where to put the reader back once the exchange succeeds */
  returnTo: string
}

/**
 * Sends the browser to the issuer.
 *
 * Returns only if it could not: everything after a successful call is a page
 * that is being replaced.
 */
export async function beginSignIn(): Promise<void> {
  const config = await authConfig()
  if (!config) {
    setStatus('unconfigured')
    return
  }
  const ends = await endpoints(config)
  if (!ends) {
    setStatus('unconfigured')
    return
  }

  const verifier = randomString()
  const pending: Pending = {
    verifier,
    state: randomString(),
    // The whole address, so a link into a release comes back to that release
    // rather than to the overview.
    returnTo: window.location.pathname + window.location.search + window.location.hash,
  }
  write(PENDING_KEY, JSON.stringify(pending))

  const params = new URLSearchParams({
    client_id: config.clientId,
    redirect_uri: config.redirectUri,
    response_type: 'code',
    // offline_access is what earns a refresh token, and a refresh token is
    // what keeps an eight-hour shift from being interrupted by a redirect.
    scope: 'openid profile email offline_access',
    state: pending.state,
    code_challenge: await challengeFor(verifier),
    code_challenge_method: 'S256',
  })
  setStatus('redirecting')
  window.location.assign(`${ends.authorization}?${params.toString()}`)
}

/**
 * Starts a sign-in because the Coordinator asked for one.
 *
 * Called from the API client on any 401. It is deliberately fire-and-forget
 * and deliberately idempotent: a page that fans out six reads gets six 401s,
 * and six redirects would each cancel the last.
 */
export function requireSignIn(): void {
  if (status === 'redirecting' || status === 'unconfigured' || status === 'rejected') return
  if (isCallback()) return

  // A token this session obtained a moment ago, refused by the Coordinator.
  // Redirecting would only obtain the same token again.
  const obtained = Number(read(OBTAINED_KEY) ?? 0)
  if (obtained && Date.now() - obtained < REJECT_WINDOW_MS) {
    setStatus('rejected')
    return
  }

  keepTokens(undefined)
  void beginSignIn()
}

// --- coming back ------------------------------------------------------------

/** The address the issuer was told to return to. */
export function isCallback(): boolean {
  return window.location.pathname === '/auth/callback'
}

/**
 * Exchanges the code the issuer sent back for tokens.
 *
 * Returns where the reader was before the sign-in started, so the caller can
 * put them back there. Throws with a sentence fit to show on a screen: this is
 * the one moment in the flow with nothing else to fall back on.
 */
export async function completeSignIn(): Promise<string> {
  const params = new URLSearchParams(window.location.search)
  const issuerError = params.get('error')
  if (issuerError) {
    throw new Error(params.get('error_description') || `The identity provider answered ${issuerError}.`)
  }

  const code = params.get('code')
  if (!code) throw new Error('The identity provider returned no authorization code.')

  const raw = read(PENDING_KEY)
  if (!raw) {
    throw new Error('This sign-in did not start in this tab, so it cannot be completed here.')
  }
  forget(PENDING_KEY)
  const pending = JSON.parse(raw) as Pending

  // The state is the only thing tying this response to the request that
  // started it. A mismatch is a cross-site request forgery attempt or a stale
  // tab, and neither may be exchanged.
  if (params.get('state') !== pending.state) {
    throw new Error('This sign-in response does not match the request that started it.')
  }

  const config = await authConfig()
  if (!config) throw new Error('Sign-in is not configured on this deployment.')
  const ends = await endpoints(config)
  if (!ends) throw new Error(`The identity provider at ${config.issuer} did not answer.`)

  const response = await fetch(ends.token, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      grant_type: 'authorization_code',
      code,
      redirect_uri: config.redirectUri,
      client_id: config.clientId,
      code_verifier: pending.verifier,
    }),
  })
  if (!response.ok) {
    // The token endpoint's own error names the real fault - an unregistered
    // redirect URI, most often - and repeating it saves a round trip through
    // a container log.
    const body = (await response.json().catch(() => ({}))) as Record<string, string>
    throw new Error(
      body.error_description || body.error || `The identity provider answered ${response.status}.`,
    )
  }

  store((await response.json()) as TokenResponse)
  setStatus('idle')
  return pending.returnTo || '/'
}

interface TokenResponse {
  access_token: string
  refresh_token?: string
  expires_in?: number
}

function store(body: TokenResponse): void {
  keepTokens({
    accessToken: body.access_token,
    // A response that renews without reissuing keeps the refresh token it was
    // given; dropping it here would end the session at the next expiry.
    refreshToken: body.refresh_token ?? tokens?.refreshToken,
    // A response with no expires_in is treated as five minutes rather than as
    // forever: a token believed valid forever is one the Coordinator rejects
    // and nothing here renews.
    expiresAt: Date.now() + (body.expires_in ?? 300) * 1000,
  })
  write(OBTAINED_KEY, String(Date.now()))
}

// --- using the session ------------------------------------------------------

/** Whether this tab holds a session at all. */
export function isSignedIn(): boolean {
  return Boolean(tokens)
}

/**
 * The Authorization header for one request, renewing first if the token is
 * about to expire. Nothing when this tab has no session, which is the normal
 * case against a Coordinator running without authentication.
 */
export async function authorization(): Promise<Record<string, string>> {
  if (!tokens) return {}
  if (tokens.expiresAt - Date.now() < EXPIRY_SKEW_MS) await renewSession()
  return tokens ? { Authorization: `Bearer ${tokens.accessToken}` } : {}
}

let renewal: Promise<boolean> | undefined

/**
 * Exchanges the refresh token for a new access token. Reports whether it now
 * holds a usable one.
 *
 * SINGLE FLIGHT. Every read on a page whose token has just expired arrives
 * here at the same moment, and an identity provider that rotates refresh
 * tokens invalidates the previous one - so the second concurrent attempt would
 * spend a token the first had already replaced and end the session.
 */
export function renewSession(): Promise<boolean> {
  renewal ??= (async () => {
    try {
      const refreshToken = tokens?.refreshToken
      if (!refreshToken) return false
      const config = await authConfig()
      if (!config) return false
      const ends = await endpoints(config)
      if (!ends) return false

      const response = await fetch(ends.token, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          grant_type: 'refresh_token',
          refresh_token: refreshToken,
          client_id: config.clientId,
        }),
      })
      if (!response.ok) {
        keepTokens(undefined)
        return false
      }
      store((await response.json()) as TokenResponse)
      return true
    } catch {
      return false
    } finally {
      // Cleared on the next tick rather than immediately, so the callers that
      // are already waiting all share this one attempt.
      setTimeout(() => {
        renewal = undefined
      }, 0)
    }
  })()
  return renewal
}

/**
 * Ends the session here and, when the issuer supports it, there as well.
 *
 * Clearing only this tab's tokens would put the reader back on the issuer's
 * still-valid session at the next redirect, which reads as a sign-out button
 * that does nothing.
 */
export async function signOut(): Promise<void> {
  const config = await authConfig()
  const ends = config ? await endpoints(config) : null
  keepTokens(undefined)
  forget(PENDING_KEY)
  forget(OBTAINED_KEY)
  setStatus('idle')

  if (config && ends?.endSession) {
    const params = new URLSearchParams({
      client_id: config.clientId,
      post_logout_redirect_uri: `${window.location.origin}/`,
    })
    window.location.assign(`${ends.endSession}?${params.toString()}`)
    return
  }
  window.location.assign('/')
}
