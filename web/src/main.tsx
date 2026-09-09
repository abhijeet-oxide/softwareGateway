import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import enGB from 'antd/locale/en_GB'
import { App as AntApp, ConfigProvider } from 'antd'
import { App } from './App'
import { IdentityProvider } from './auth/permissions'
import { SessionGate } from './auth/SessionGate'
import { BootGate } from './BootGate'
import { FeedbackBridge, reportFailure } from './components/feedback'
import { AppErrorBoundary } from './routing'
import { ThemeProvider } from './uikit'
import './uikit/styles.css'
import './index.css'

/**
 * What a query or a mutation is FOR, when a failure has to be reported.
 *
 * `action` is the sentence fragment a toast puts in front of "failed" - "Run
 * health check", "Start discovery". Set it on anything the reader presses; it
 * is also what tells the cache below that a read was ASKED for rather than
 * arriving on its own. See components/feedback.
 */
declare module '@tanstack/react-query' {
  interface Register {
    queryMeta: { action?: string }
    mutationMeta: { action?: string }
  }
}

/*
  FAILURES ARE REPORTED IN ONE PLACE, not in forty catch blocks.

  Before this, every page decided for itself whether to render a failure, and
  the ones that forgot were silent: a button that answered 403 looked like a
  button that did nothing. See components/feedback for the whole account of
  which failures surface here and which belong in a page's own body.
*/
const queryClient = new QueryClient({
  queryCache: new QueryCache({
    onError: (error, query) => {
      // A read somebody pressed a button for, or a background refresh of data
      // already on screen. A FIRST load that fails renders in the page's own
      // body, where there is room for it, so it is not also thrown at them.
      const asked = Boolean(query.meta?.action)
      if (!asked && query.state.data === undefined) return
      reportFailure(error, query.meta?.action)
    },
  }),
  mutationCache: new MutationCache({
    // Every mutation is somebody pressing something, so every failed one is
    // reported whether or not the page that fired it thought to.
    onError: (error, _vars, _ctx, mutation) => {
      reportFailure(error, mutation.meta?.action)
    },
  }),
  defaultOptions: {
    queries: {
      // A failed read is shown, not retried into a spinner that never ends.
      // The one exception is a genuinely unreachable Coordinator, which the
      // error state names and offers a retry for.
      retry: 1,
      refetchOnWindowFocus: false,
      staleTime: 30_000,
    },
  },
})

// ThemeProvider is the shared design system's one entry point: it builds the
// Ant Design theme from the same tokens the CSS variables come from, stamps the
// painted mode on <html> so plain CSS can see it, and follows the operating
// system's light/dark setting LIVE rather than reading it once at boot.
//
// It is uncontrolled here, so it keeps and persists the appearance itself.
// That is the right shape for this application: it has no settings model of
// its own to be the truth, and the alternative would have been inventing one
// just to hold three fields.
//
// The locale stays on an outer ConfigProvider: it is a property of this
// deployment rather than of the design system, and the shared provider must
// not start carrying opinions that only one tool has.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ConfigProvider locale={enGB}>
      <ThemeProvider>
        {/*
          Ant Design's App, and it is not decoration. `App.useApp()` is what
          gives a toast the current theme, the current locale and the current
          root - without this provider it falls back to a static instance that
          renders outside all three and warns in the console. Every message and
          notification in the application goes through it, so it wraps
          everything.
        */}
        <AntApp>
          {/*
            The last boundary. Inside the theme so the page it draws is themed,
            and outside everything else so a throw anywhere - a provider, the
            shell, the router itself - lands on the shared kit's error page
            rather than on a white document with an exception in a console
            nobody has open.
          */}
          <AppErrorBoundary>
            <QueryClientProvider client={queryClient}>
              {/* Binds the query caches' failure reporting to that App. */}
              <FeedbackBridge />
              {/*
                Above IdentityProvider, and that order is load-bearing.
                Returning from the identity provider lands on an address
                carrying a single-use code; any read fired while it is being
                exchanged comes back 401 and starts a SECOND sign-in, which
                navigates away before the first one finished. Nothing may read
                until the session is settled, and the first reader in this tree
                is /whoami.
              */}
              <SessionGate>
                <IdentityProvider>
                  <BrowserRouter>
                    {/* Nothing renders until we know the Coordinator is there. */}
                    <BootGate>
                      <App />
                    </BootGate>
                  </BrowserRouter>
                </IdentityProvider>
              </SessionGate>
            </QueryClientProvider>
          </AppErrorBoundary>
        </AntApp>
      </ThemeProvider>
    </ConfigProvider>
  </StrictMode>,
)
