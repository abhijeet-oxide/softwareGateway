import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The Coordinator installs no CORS middleware, so the browser must reach the
// API on its own origin. In development that means proxying rather than
// pointing the app at http://localhost:8080 — and same-origin is the right
// production posture anyway, so dev and prod agree.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.COORDINATOR_URL ?? 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    // Air-gapped by construction: everything the page needs ships in the
    // bundle. Nothing is fetched from a CDN at runtime (docs/design/19 §6).
    assetsInlineLimit: 4096,
    rollupOptions: {
      output: {
        // React is pinned to its own chunk because it is what changes least
        // and therefore stays cached across deploys.
        //
        // The component library is deliberately NOT pinned. Forcing it into one
        // chunk was measured and was worse: it put every component on the
        // critical path, including the date picker and its date library, which
        // only the Activity page uses. Letting rollup split it moves that
        // ~40 kB behind the route that needs it and leaves the first paint
        // smaller.
        manualChunks: {
          react: ['react', 'react-dom', 'react-router-dom'],
        },
      },
    },
  },
})
