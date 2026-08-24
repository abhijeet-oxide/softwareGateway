// The IDENTITY contract: the small set of things that legitimately differ
// between two tools that are meant to look the same.
//
// A shared look is not a shared name. Configer and Software Gateway wear the
// same palette, the same controls and the same spacing, and they must still
// say which one you are looking at - so identity is the one thing this folder
// declares a TYPE for rather than a VALUE. Each app supplies its own
// `src/brand.ts` (outside uikit/), and that file is exactly what survives when
// this folder is copied over the top of it.
//
// Anything else you are tempted to put here - a colour, a radius, a font - is
// a token, and belongs in tokens.ts where both tools get it.

/** A mark to sit before the product name in the navigation. */
export interface BrandLogo {
  /** a short text glyph, used when neither svg nor src is given */
  text?: string;
  /** inline <svg> markup, so the mark takes its colour from around it */
  svg?: string;
  /** a path or URL to an image; use last - it cannot follow the theme */
  src?: string;
}

export interface BrandIdentity {
  /** Product name shown in the navigation and the browser title. */
  appName: string;
  /** Long descriptor used in the browser <title>, after the app name. */
  tagline: string;
  /** Short caption shown under the name in the navigation. */
  navCaption: string;
  logo: BrandLogo;
  /** An emoji, an inline <svg ...> string, or a path/URL to a file. */
  favicon: string;
}

/** Resolve a favicon field (emoji / inline svg / path) to an href. */
export function faviconHref(favicon: string): string {
  const s = favicon.trim();
  if (s.startsWith("<svg")) return "data:image/svg+xml," + encodeURIComponent(s);
  if (/^(\/|https?:|data:)/.test(s) || /\.(svg|png|ico|jpe?g|gif|webp)$/i.test(s)) return s;
  // Treat anything else (e.g. an emoji) as a glyph centered on a square.
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'>` +
    `<text x='16' y='25' font-size='26' text-anchor='middle'>${s}</text></svg>`;
  return "data:image/svg+xml," + encodeURIComponent(svg);
}

/** The <title> a brand asks for. One place, so both tools title the same way. */
export function documentTitle(brand: BrandIdentity): string {
  return brand.tagline ? `${brand.appName} - ${brand.tagline}` : brand.appName;
}
