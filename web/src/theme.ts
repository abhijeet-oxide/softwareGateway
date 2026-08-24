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
  /**
   * A rule INSIDE a card - between table rows, under a panel heading.
   *
   * Lighter than `border`, which separates a card from the page. One value for
   * both made every table read as a grid of equal boxes, because the line
   * around a card and the line between two of its rows carried the same weight.
   */
  hairline: '#EDF0F4',
  /** A recessed well: a panel nested inside a card, a code block, a diff. */
  sunken: '#F7F9FB',

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
 * Depth.
 *
 * Every card in this application was a white rectangle with the same 1px border
 * on the same grey, so a page of six of them had no order in it: the summary
 * that should be read first and the table it summarises sat at exactly the same
 * distance from the reader. These give the page three planes.
 *
 * Offset and blur, never a zero-offset halo - a shadow without an offset is not
 * light falling on anything, it is a coloured outline pretending to be depth.
 * The tint is the sidebar navy rather than black, so the shade belongs to the
 * palette and follows a rebrand with everything else.
 */
export const elevation = {
  /** A card at rest. Enough to lift it off the page background, no more. */
  card: '0 1px 2px rgba(11,31,58,0.05), 0 1px 3px rgba(11,31,58,0.04)',
  /** A card the pointer is over, or one carrying the page's answer. */
  raised: '0 2px 4px rgba(11,31,58,0.06), 0 6px 16px rgba(11,31,58,0.07)',
  /** Something genuinely floating: a popover, a drawer, a dropdown. */
  overlay: '0 6px 16px rgba(11,31,58,0.08), 0 12px 40px rgba(11,31,58,0.12)',
}

/**
 * Motion.
 *
 * One authored moment, not an effect on every element. Panels settle in on
 * arrival and severity bars grow from nothing; everything else is a state
 * change fast enough to feel like a response rather than an animation.
 *
 * Exponential ease-out from an already-visible default, so a slow connection
 * that renders before the transition runs shows the finished page rather than
 * an empty one.
 */
export const motion = {
  fast: '120ms cubic-bezier(0.16, 1, 0.3, 1)',
  base: '240ms cubic-bezier(0.16, 1, 0.3, 1)',
  slow: '420ms cubic-bezier(0.16, 1, 0.3, 1)',
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
 * Severity, and the verdict of a comparison.
 *
 * Here rather than in the security components for the reason at the top of this
 * file: nothing outside it names a colour, and a page that reached for its own
 * hex is one a rebrand silently misses.
 *
 * COLOUR IS NEVER THE ONLY SIGNAL. Every severity is also a word and a shape -
 * the dot beside it differs in fill, and the label is always written out - so
 * these read correctly in greyscale and to anybody who does not distinguish red
 * from green. A deployment may retheme them freely without making the page
 * unreadable, which is the whole test of whether the reinforcement is real.
 */
export const severity = {
  critical: '#B4232B',
  high: '#D9660B',
  medium: '#B98900',
  low: '#2E7D4F',
  unknown: '#7A8694',
}

/** The severity dots' fills, lighter than the text they sit beside. */
export const severitySurface = {
  critical: '#FBEDED',
  high: '#FDF2E8',
  medium: '#FCF7E6',
  low: '#EFF6F1',
  unknown: '#F1F3F5',
}

/**
 * A comparison's verdict.
 *
 * `inconclusive` is deliberately not a shade of grey that reads as "nothing
 * happened". It is a state with an action in it - go and scan the rest - and
 * looking like an empty result is how it gets ignored.
 */
export const verdict = {
  better: '#1F7A3D',
  worse: '#B4232B',
  unchanged: '#5A6675',
  inconclusive: '#7A4FBF',
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

    // Ant's defaults are a black halo with no offset. Replaced with the
    // palette's own shade so depth reads as light rather than as an outline.
    boxShadow: elevation.card,
    boxShadowSecondary: elevation.overlay,
    boxShadowTertiary: elevation.card,
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
      headerBg: palette.sunken,
      headerColor: semantic.neutral,
      cellPaddingBlock: 12,
      borderColor: palette.hairline,
      rowHoverBg: '#F6FAFD',
    },
    Card: { paddingLG: 16 },
    Statistic: { contentFontSize: 28 },
    Segmented: { itemSelectedBg: '#FFFFFF', trackBg: palette.sunken },
    Tabs: { horizontalItemPadding: '10px 0', horizontalMargin: '0 0 16px 0' },
  },
}

/** Identifiers - versions, digests, paths, URLs - are monospace everywhere. */
export const mono = palette.monoFamily
