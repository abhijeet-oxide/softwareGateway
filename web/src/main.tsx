import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import enGB from 'antd/locale/en_GB'
import { ConfigProvider } from 'antd'
import { App } from './App'
import { IdentityProvider } from './auth/permissions'
import { BootGate } from './BootGate'
import { AppErrorBoundary } from './routing'
import { ThemeProvider } from './uikit'
import './uikit/styles.css'
import './index.css'

const queryClient = new QueryClient({
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
          The last boundary. Inside the theme so the page it draws is themed,
          and outside everything else so a throw anywhere - a provider, the
          shell, the router itself - lands on the shared kit's error page
          rather than on a white document with an exception in a console
          nobody has open.
        */}
        <AppErrorBoundary>
          <QueryClientProvider client={queryClient}>
            <IdentityProvider>
              <BrowserRouter>
                {/* Nothing renders until we know the Coordinator is there. */}
                <BootGate>
                  <App />
                </BootGate>
              </BrowserRouter>
            </IdentityProvider>
          </QueryClientProvider>
        </AppErrorBoundary>
      </ThemeProvider>
    </ConfigProvider>
  </StrictMode>,
)
