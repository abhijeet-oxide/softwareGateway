import type { Plugin } from "vite";
import { ACTIVE_PRESET, renderRootCss } from "./tokens";
import { resolvePalette } from "./antd";
import { documentTitle, faviconHref, type BrandIdentity } from "./brand";

// Build-time theming.
//
// The colour variables are inlined into <head> rather than imported as a
// stylesheet, and that is the whole point: an @import resolves after the first
// paint, so the page renders once in the browser's defaults and then again in
// the theme - the flash of the wrong theme that no amount of CSS fixes. The
// favicon and the <title> come from the same call for the same reason: one
// source for what the product is called.
//
// It takes the brand as an ARGUMENT rather than importing it, so this file
// stays identical between tools:
//
//   import brand from "./src/brand";
//   plugins: [react(), brandPlugin(brand)]
//
// Runs for `vite dev` and `vite build` alike.
export default function brandPlugin(brand: BrandIdentity): Plugin {
  return {
    name: "uikit-brand",
    transformIndexHtml(html) {
      // Resolved through Ant Design, so a variable and the component library can
      // never name two different blues (see resolvePalette).
      const style = `<style id="brand-tokens">${renderRootCss((_, dark) =>
        resolvePalette(dark ? "dark" : "light"),
      )}</style>`;
      const icon = `<link rel="icon" href="${faviconHref(brand.favicon)}" />`;
      const title = documentTitle(brand);

      let out = html;
      // Stamp the active preset on <html> at PARSE time, so the palette it
      // scopes is in force before anything paints.
      if (ACTIVE_PRESET && ACTIVE_PRESET !== "default") {
        out = out.replace(/<html(\s|>)/, `<html data-preset="${ACTIVE_PRESET}"$1`);
      }
      out = out.replace(/<title>[\s\S]*?<\/title>/, `<title>${title}</title>`);
      // Drop any existing favicon <link> so ours is authoritative.
      out = out.replace(/[ \t]*<link[^>]*rel=["']icon["'][^>]*>\s*\n?/gi, "");
      out = out.replace(/<\/head>/, `    ${style}\n    ${icon}\n  </head>`);
      return out;
    },
  };
}
