import type { ThemeConfig } from 'antd'
import brand from './brand'
import {
  buildTheme,
  c,
  cssVar,
  envHex,
  isProductionEnv,
  mono as monoFamily,
  tokens,
  severity as severityColours,
  severitySurface as severitySurfaces,
  verdict as verdictColours,
  withAlpha,
} from './uikit'

/**
 * THE VISUAL VOCABULARY OF THIS APPLICATION, IN ONE FILE - and none of it
 * decided here any more.
 *
 * # What changed, and why
 *
 * Every value below now comes from `uikit/`, the design system this tool
 * shares - byte for byte - with the other tools on this platform. They are
 * meant to look and behave like one product, and the only way two codebases
 * stay identical is for the shared part to be the SAME FILES rather than two
 * careful copies of the same intentions. Two palettes that agree today are two
 * palettes; a copied folder is one design system.
 *
 * So:
 *   - to change how this tool looks, edit `uikit/tokens.ts` - and BOTH tools
 *     change, which is the point;
 *   - to change what it is CALLED, edit `brand.ts`, which is the one file that
 *     says "Software Gateway" and the one thing copying `uikit/` never touches.
 *
 * # Why these are var() strings
 *
 * Each colour here is a CSS custom property reference, not a hex. An inline
 * style written with one follows the light/dark switch and the active preset
 * without the component knowing either exists - which is how this application
 * gained a dark theme without a single page being edited. The one place real
 * values are still needed is the Ant Design config below, because Ant derives
 * whole families (a hover, a border, a disabled fill) from each colour and
 * cannot derive anything from a string only the browser can resolve. `uikit`
 * builds that half from the same tokens, so the two cannot drift.
 *
 * # This file is a COMPATIBILITY LAYER
 *
 * The names below are the ones this application already used in about twenty
 * files. They are kept so adopting the shared system was not also a rename of
 * every page. New code should import from `./uikit` directly; `palette.primary`
 * and `c.brand` are the same colour, and only one of them is a name the other
 * tools also know.
 */

// ---------------------------------------------------------------------------
// Branding - now one import, so it cannot be set in two places
// ---------------------------------------------------------------------------

export const branding = {
  /** The name in the navigation, and nowhere else. */
  name: brand.appName,
  /** How large the mark is drawn, in pixels. */
  markSize: 20,
}

export { brand }

// ---------------------------------------------------------------------------
// The palette, mapped onto the shared tokens
// ---------------------------------------------------------------------------

export const palette = {
  /** The brand colour. Everything interactive derives from it. */
  primary: c.brand,

  /** The side navigation, and the type on it. */
  sidebar: c.navBg,
  sidebarText: c.navFg,
  sidebarSelected: c.navBgActive,

  /** The top bar, and the rule under it. */
  topBar: c.surface,
  topBarBorder: c.border,

  /** Behind the cards. */
  pageBackground: c.canvas,
  headingText: c.text,
  /** Separates a CARD from the page. */
  border: c.borderStrong,
  /**
   * A rule INSIDE a card - between table rows, under a panel heading.
   *
   * Lighter than `border`, which separates a card from the page. One value for
   * both made every table read as a grid of equal boxes, because the line
   * around a card and the line between two of its rows carried the same weight.
   */
  hairline: c.border,
  /** A recessed well: a panel nested inside a card, a code block, a diff. */
  sunken: c.surface2,

  /** Corners, control height and type scale. */
  borderRadius: tokens.shape.borderRadius,
  controlHeight: tokens.shape.controlHeight,
  fontSize: tokens.type.fontSizeBase,

  fontFamily: cssVar('font-ui'),
  /** Identifiers - versions, digests, paths, URLs - are monospace everywhere. */
  monoFamily,
}

/**
 * Semantic colours. Each is REINFORCEMENT - every status is also stated in
 * words, so a deployment may retheme these freely without making anything
 * unreadable.
 */
export const semantic = {
  success: c.ok,
  error: c.danger,
  warning: c.pending,
  /** Used sparingly, for lifecycle-state differentiation only. */
  lifecycle: c.base,
  neutral: c.text2,
}

/**
 * Severity, and the verdict of a comparison.
 *
 * COLOUR IS NEVER THE ONLY SIGNAL. Every severity is also a word and a shape -
 * the dot beside it differs in fill, and the label is always written out - so
 * these read correctly in greyscale and to anybody who does not distinguish
 * red from green. A deployment may retheme them freely without making the page
 * unreadable, which is the whole test of whether the reinforcement is real.
 */
export const severity = severityColours

/** The severity dots' fills, lighter than the text they sit beside. */
export const severitySurface = severitySurfaces

/**
 * A comparison's verdict.
 *
 * `inconclusive` is deliberately not a shade of grey that reads as "nothing
 * happened". It is a state with an action in it - go and scan the rest - and
 * looking like an empty result is how it gets ignored.
 */
export const verdict = verdictColours

/** The SURFACES those colours sit on: an alert's fill, a status tag's ground. */
export const surfaces = {
  successBg: c.okBg,
  successBorder: c.okBd,
  errorBg: c.dangerBg,
  errorBorder: c.dangerBd,
  warningBg: c.pendingBg,
  warningBorder: c.pendingBd,
  infoBg: c.reviewBg,
  infoBorder: c.reviewBd,
}

/**
 * Depth.
 *
 * Every card in this application was a white rectangle with the same 1px
 * border on the same grey, so a page of six of them had no order in it: the
 * summary that should be read first and the table it summarises sat at exactly
 * the same distance from the reader. These give the page three planes, and
 * they now change with the theme - a shadow tuned for a pastel canvas is a
 * grey smear on a dark one.
 */
export const elevation = {
  /** A card at rest. Enough to lift it off the page background, no more. */
  card: cssVar('el-1'),
  /** A card the pointer is over, or one carrying the page's answer. */
  raised: cssVar('el-2'),
  /** Something genuinely floating: a popover, a drawer, a dropdown. */
  overlay: cssVar('el-overlay'),
}

/**
 * Motion.
 *
 * One authored moment, not an effect on every element. Panels settle in on
 * arrival and severity bars grow from nothing; everything else is a state
 * change fast enough to feel like a response rather than an animation.
 */
export const motion = {
  fast: `var(--dur-hover) var(--ease-out)`,
  base: `var(--dur-panel) var(--ease-out)`,
  slow: `var(--dur-view) var(--ease-out)`,
}

/** The selected navigation item. */
export const sidebarSelected = c.navBgActive

/** Identifiers - versions, digests, paths, URLs - are monospace everywhere. */
export const mono = monoFamily

/** Re-exported so a page needing a tint reaches for the helper rather than
 *  appending two hex digits to a value that is not a hex. */
export { withAlpha, envHex, isProductionEnv }

/**
 * The Ant Design theme for the LIGHT mode, kept for anything still mounting a
 * bare `<ConfigProvider theme={theme}>`. The application itself no longer
 * does: `<ThemeProvider>` in main.tsx builds this from the same tokens for
 * whichever mode is being painted, which is what makes dark mode work at all.
 */
export const theme: ThemeConfig = buildTheme('light')
