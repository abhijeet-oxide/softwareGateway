import type { ThemeConfig } from 'antd'
import type { ComponentType, CSSProperties } from 'react'
import AtandtIcon from '@iconify-react/thesvg-color/atandt';

/**
 * EVERY VISUAL DECISION IN THIS APPLICATION, IN ONE FILE.
 *
 * # What this file is for
 *
 * A deployment of this tool belongs to whoever runs it, and the first thing
 * anybody wants is for it to look like theirs. That must not mean forking the
 * application, so everything a company would want to change lives here: the
 * name, the mark beside it, the primary colour, the side navigation, the top
 * bar, the corner radius, the type.
 *
 * Nothing outside this file names a colour. A page that reached for its own
 * hex would be one that a rebrand silently misses.
 *
 * # Tokens rather than forked CSS
 *
 * The palette below is turned into Ant Design tokens, and every component picks
 * those up - so a table, a drawer and a modal agree without any of them being
 * restyled by hand. Change `palette.primary` and the buttons, the links, the
 * selected nav item, the progress bars and the focus rings all follow.
 *
 * Light theme only. The brief makes it the default and the one to design
 * first, and a dark theme is explicitly not part of this exercise.
 */

// ---------------------------------------------------------------------------
// Branding - what a deployment changes first
// ---------------------------------------------------------------------------

/**
 * A mark to sit before the product name in the side navigation.
 *
 * Three forms, in the order they are worth reaching for:
 *
 *   1. AN ICONIFY ICON. Add the import at the top of this file and name it
 *      here. The icon is compiled INTO the bundle at build time, which is what
 *      keeps an air-gapped deployment working - the Iconify runtime resolves
 *      unknown icons over the network, and in a closed network that means an
 *      icon that silently never appears.
 *
 *        import BrandMark from '~icons/mdi/shield-check'
 *        mark: BrandMark
 *
 *   2. RAW SVG MARKUP, for a company mark that is not in an icon set. Anything
 *      starting with `<svg` is rendered inline, so it inherits the colour and
 *      size around it.
 *
 *        mark: '<svg viewBox="0 0 24 24">…</svg>'
 *
 *   3. AN IMAGE, by URL or data URI - a PNG logo, or a file under `public/`.
 *      Use this last: it cannot take its colour from the theme.
 *
 *        mark: '/logo.svg'
 *
 * Leave it undefined for no mark at all, which is the default.
 */
export type BrandMark = ComponentType<{ style?: CSSProperties }> | string

export const branding: {
  /** The name in the side navigation, and nowhere else. */
  name: string
  mark?: BrandMark
  /** How large the mark is drawn, in pixels. */
  markSize: number
} = {
  name: 'Software Gateway',
  mark: AtandtIcon,
  markSize: 20,
}

// ---------------------------------------------------------------------------
// Palette - the colours a deployment sets
// ---------------------------------------------------------------------------

/**
 * The colours and metrics everything else is derived from.
 *
 * `primary` is the one that matters: it is the buttons, the links, the selected
 * navigation item, the progress bars and the focus ring. The rest are here so a
 * company with a dark brand and a light one can both look deliberate rather
 * than both looking like this one with a different button.
 */
export const palette = {
  /** The brand colour. Everything interactive derives from it. */
  primary: '#009fdb',

  /** The side navigation, and the type on it. */
  sidebar: '#0B1F3A',
  sidebarText: 'rgba(255,255,255,0.72)',
  /** The selected navigation item. Defaults to the brand colour. */
  sidebarSelected: '',

  /** The top bar, and the rule under it. */
  topBar: '#FFFFFF',
  topBarBorder: '#E4E8EE',

  /** Behind the cards. */
  pageBackground: '#F4F6F9',
  headingText: '#111C2B',
  border: '#E4E8EE',

  /** Corners, control height and type scale. */
  borderRadius: 4,
  controlHeight: 32,
  fontSize: 14,

  /**
   * No webfont is loaded - an air-gapped bundle ships no CDN request, and a
   * system stack renders identically without one (docs/design/19 §6). A
   * deployment that wants its own face should bundle it and name it here.
   */
  fontFamily:
    "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif",

  /** Identifiers - versions, digests, paths, URLs - are monospace everywhere. */
  monoFamily:
    "ui-monospace, SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace",
}

/**
 * Semantic colours. Each is REINFORCEMENT - every status is also stated in
 * words, so a deployment may retheme these freely without making anything
 * unreadable.
 */
export const semantic = {
  success: '#1F7A3D',
  error: '#C4262E',
  warning: '#B26B00',
  /** Used sparingly, for lifecycle-state differentiation only. */
  lifecycle: '#5B3FA8',
  neutral: '#5A6675',
}

/**
 * The SURFACES those colours sit on.
 *
 * Stated rather than derived. The semantic colours above are chosen for TEXT
 * and icons, so they are dark enough to read; Ant Design derives every status
 * surface from them - an Alert's background, a `color="success"` Tag's fill -
 * and a dark green derives a dark green surface. The result was a filled slab
 * beside the light preset tags, which is what made a production target look
 * like a different design system from an enabled rule.
 */
export const surfaces = {
  successBg: '#F1F7F3',
  successBorder: '#C6E0D0',
  errorBg: '#FDF1F1',
  errorBorder: '#F2C7C9',
  warningBg: '#FDF7EA',
  warningBorder: '#EEDCAF',
  infoBg: '#EFF5FC',
  infoBorder: '#C6DCF4',
}

// ---------------------------------------------------------------------------
// Derived - nothing below here is meant to be edited
// ---------------------------------------------------------------------------

/** The selected navigation item, falling back to the brand colour. */
export const sidebarSelected = palette.sidebarSelected || palette.primary

export const theme: ThemeConfig = {
  token: {
    colorPrimary: palette.primary,
    colorInfo: palette.primary,
    colorSuccess: semantic.success,
    colorError: semantic.error,
    colorWarning: semantic.warning,

    colorSuccessBg: surfaces.successBg,
    colorSuccessBorder: surfaces.successBorder,
    colorErrorBg: surfaces.errorBg,
    colorErrorBorder: surfaces.errorBorder,
    colorWarningBg: surfaces.warningBg,
    colorWarningBorder: surfaces.warningBorder,
    colorInfoBg: surfaces.infoBg,
    colorInfoBorder: surfaces.infoBorder,

    // Compact and restrained: a mature internal operations product, not a
    // generic SaaS dashboard.
    borderRadius: palette.borderRadius,
    controlHeight: palette.controlHeight,
    fontSize: palette.fontSize,

    colorBgLayout: palette.pageBackground,
    colorBorderSecondary: palette.border,
    colorTextHeading: palette.headingText,
    fontFamily: palette.fontFamily,
  },
  components: {
    Layout: {
      siderBg: palette.sidebar,
      headerBg: palette.topBar,
      bodyBg: palette.pageBackground,
    },
    Menu: {
      darkItemBg: palette.sidebar,
      darkSubMenuItemBg: palette.sidebar,
      darkItemSelectedBg: sidebarSelected,
      darkItemColor: palette.sidebarText,
    },
    Table: {
      headerBg: '#FAFBFC',
      headerColor: semantic.neutral,
      cellPaddingBlock: 10,
    },
    Card: { paddingLG: 16 },
    Statistic: { contentFontSize: 28 },
  },
}

/** Identifiers - versions, digests, paths, URLs - are monospace everywhere. */
export const mono = palette.monoFamily
