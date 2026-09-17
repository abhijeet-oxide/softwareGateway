import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import Icons from 'unplugin-icons/vite'
// The extensions are load-bearing, here and in everything this file reaches.
// Vite's `configLoader: 'native'` hands the config to the runtime's own TypeScript
// support instead of bundling it first, and that resolver does no extension
// guessing and no directory indexes: `./src/brand` is simply not found. It is
// the planned default, and the current loader already warns about every
// specifier that would break under it, so the config's own module graph -
// vitePluginBrand, brand, vitePluginRuntimeConfig, and the uikit files those
// reach - spells its imports out. The app's other 60-odd imports are resolved
// by Vite and need nothing.
import brandPlugin from './src/uikit/vitePluginBrand.ts'
import brand from './src/brand.ts'
import runtimeConfigPlugin from './vitePluginRuntimeConfig.ts'

// The Coordinator installs no CORS middleware, so the browser must reach the
// API on its own origin. In development that means proxying rather than
// pointing the app at http://localhost:8080 - and same-origin is the right
// production posture anyway, so dev and prod agree.
const apiProxy = {
  '/api': {
    target: process.env.COORDINATOR_URL ?? 'http://localhost:8080',
    changeOrigin: true,
  },
}

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
    // The deployment's own runtime document, which in a deployment is written
    // by the web container's entrypoint and here has no author at all.
    runtimeConfigPlugin(),
  ],
  server: {
    port: 5173,
    proxy: apiProxy,
  },
  // THE SAME PROXY for `vite preview`, which is the only way to look at a
  // production build against a running Coordinator without building an image.
  //
  // Without it `preview` served the bundle and 404ed every read, so the only
  // local view of this interface was the development server - and a
  // development React is several times slower to render than the one that
  // ships. A page that takes two seconds to appear under `pnpm dev` and 300ms
  // in a deployment is a performance report nobody can act on. See
  // docs/design/32-performance.md.
  preview: {
    proxy: apiProxy,
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
        // A FUNCTION, not a map. Vite 8 builds with rolldown, which dropped the
        // object form and reports it at build time as
        // `TypeError: manualChunks is not a function`.
        manualChunks(id) {
          if (/node_modules\/(react|react-dom|react-router|react-router-dom)\//.test(id)) {
            return 'react'
          }
          return undefined
        },
      },
    },
  },
})
