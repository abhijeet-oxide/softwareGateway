import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import Icons from 'unplugin-icons/vite'
import brandPlugin from './src/uikit/vitePluginBrand'
import brand from './src/brand'

// The Coordinator installs no CORS middleware, so the browser must reach the
// API on its own origin. In development that means proxying rather than
// pointing the app at http://localhost:8080 - and same-origin is the right
// production posture anyway, so dev and prod agree.
export default defineConfig({
  plugins: [
    react(),
    // Iconify, compiled at BUILD time rather than fetched at runtime.
    //
    // The @iconify/react runtime resolves unknown icons over the network from
    // api.iconify.design, which in an air-gapped deployment means every icon
    // silently fails to render. This plugin turns `~icons/simple-icons/nokia`
    // into an inline SVG component instead, so only the icons actually used
    // are bundled and nothing is fetched.
    Icons({ compiler: 'jsx', jsx: 'react', scale: 1 }),
    // The shared design system's colour variables, inlined into <head> at
    // build time along with the favicon and the title.
    //
    // Inlined rather than imported as a stylesheet, and that is the whole
    // point: an @import resolves after the first paint, so the page renders
    // once in the browser's defaults and then again in the theme. It takes the
    // brand as an argument because nothing inside uikit/ may name a product -
    // that rule is what lets the folder be copied between tools unchanged.
    brandPlugin(brand),
  ],
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
