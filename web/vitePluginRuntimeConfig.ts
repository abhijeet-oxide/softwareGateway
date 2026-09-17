import { readFileSync } from 'node:fs'
import type { Connect, Plugin, PreviewServer, ViteDevServer } from 'vite'

// /runtime-config.json, for the development server.
//
// In a deployment this document is WRITTEN AT CONTAINER START by
// deploy/web/docker-entrypoint.sh, because neither the issuer nor the client id
// is knowable when the image is built - ZITADEL generates the client id when
// the seeder registers the application - and nginx then serves it `no-store`.
//
// `task run` has neither nginx nor a seeder, so the path did not exist at all
// and every load of the app opened on a red
// `GET /runtime-config.json 404 (Not Found)`. The app coped: session.ts reads
// an absent document as "no sign-in configured", which is the correct answer
// for local development. But an error the browser reports and the code expects
// is a false alarm, and a console that cries wolf is one nobody reads.
//
// So development serves the same document, from the same environment variables
// the entrypoint reads, with the same precedence: a variable wins over the file
// the seeder writes. All of them unset yields a document of empty strings -
// what `task run` wants, and what session.ts already treats as no sign-in.
export default function runtimeConfigPlugin(): Plugin {
  const path = '/runtime-config.json'

  // One field of the flat JSON object the seeder writes, or empty. Pointed at
  // by OIDC_CONFIG_FILE, which is how a developer runs `pnpm dev` against a
  // stack that `docker compose up` has already seeded.
  const field = (name: string): string => {
    const file = process.env.OIDC_CONFIG_FILE
    if (!file) return ''
    try {
      const doc = JSON.parse(readFileSync(file, 'utf8')) as Record<string, unknown>
      const value = doc[name]
      return typeof value === 'string' ? value : ''
    } catch {
      // Absent or unreadable is the ordinary case: nobody has run the seeder.
      return ''
    }
  }

  const document = (): string =>
    JSON.stringify(
      {
        oidc: {
          issuer: process.env.OIDC_ISSUER || field('issuer'),
          clientId: process.env.OIDC_CLIENT_ID || field('clientId'),
          redirectUri: process.env.OIDC_REDIRECT_URI || field('redirectUri'),
        },
        support: { contact: process.env.SUPPORT_CONTACT ?? '' },
        identityProvider: { name: process.env.SSO_DISPLAY_NAME ?? '' },
      },
      null,
      2,
    )

  const middleware: Connect.NextHandleFunction = (req, res, next) => {
    if ((req.url ?? '').split('?')[0] !== path) {
      next()
      return
    }
    res.setHeader('Content-Type', 'application/json')
    // As nginx does, and for the same reason: a cached copy names the identity
    // provider of the deployment before this one.
    res.setHeader('Cache-Control', 'no-store')
    res.end(document())
  }

  // Both servers. `vite preview` serves the built bundle with no nginx in front
  // of it, so it would 404 in exactly the same way.
  const serve = (server: ViteDevServer | PreviewServer): void => {
    server.middlewares.use(middleware)
  }

  return {
    name: 'runtime-config',
    configureServer: serve,
    configurePreviewServer: serve,
  }
}
