import type { ThemeConfig } from 'antd'

/**
 * The AT&T enterprise visual language, as Ant Design tokens.
 *
 * Tokens rather than forked CSS: every component picks these up, so a table, a
 * drawer and a modal agree without any of them being restyled by hand.
 *
 * Light theme only. The brief makes it the default and the one to design
 * first, and a dark theme is explicitly not part of this exercise.
 */

export const attBlue = '#0057B8'
export const attNavy = '#0B1F3A'

/** Semantic colours. Each is REINFORCEMENT - every status is also stated in words. */
export const semantic = {
  success: '#1F7A3D',
  error: '#C4262E',
  warning: '#B26B00',
  /** Used sparingly, for lifecycle-state differentiation only. */
  lifecycle: '#5B3FA8',
  neutral: '#5A6675',
}

export const theme: ThemeConfig = {
  token: {
    colorPrimary: attBlue,
    colorSuccess: semantic.success,
    colorError: semantic.error,
    colorWarning: semantic.warning,
    colorInfo: attBlue,

    // The semantic colours above are chosen for TEXT and icons, so they are
    // dark enough to read. Ant Design also derives every status SURFACE from
    // them - an Alert's background, a `color="success"` Tag's fill - and a dark
    // green derives a dark green surface. The result was a filled slab beside
    // the light preset tags, which is what made a production target look like a
    // different design system from an enabled rule.
    //
    // So the surfaces are stated rather than derived. Foreground stays AT&T;
    // background stays light, and matches the preset palettes everything else
    // uses.
    colorSuccessBg: '#F1F7F3',
    colorSuccessBorder: '#C6E0D0',
    colorErrorBg: '#FDF1F1',
    colorErrorBorder: '#F2C7C9',
    colorWarningBg: '#FDF7EA',
    colorWarningBorder: '#EEDCAF',
    colorInfoBg: '#EFF5FC',
    colorInfoBorder: '#C6DCF4',

    // Compact and restrained: a mature internal operations product, not a
    // generic SaaS dashboard.
    borderRadius: 4,
    controlHeight: 32,
    fontSize: 14,

    colorBgLayout: '#F4F6F9',
    colorBorderSecondary: '#E4E8EE',
    colorTextHeading: '#111C2B',

    // No webfont is loaded - an air-gapped bundle ships no CDN request, and a
    // system stack renders identically without one (docs/design/19 §6).
    fontFamily:
      "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif",
  },
  components: {
    Layout: {
      siderBg: attNavy,
      headerBg: '#FFFFFF',
      bodyBg: '#F4F6F9',
    },
    Menu: {
      darkItemBg: attNavy,
      darkSubMenuItemBg: attNavy,
      darkItemSelectedBg: attBlue,
      darkItemColor: 'rgba(255,255,255,0.72)',
    },
    Table: {
      headerBg: '#FAFBFC',
      headerColor: '#5A6675',
      cellPaddingBlock: 10,
    },
    Card: { paddingLG: 16 },
    Statistic: { contentFontSize: 28 },
  },
}

/** Identifiers - versions, digests, paths, URLs - are monospace everywhere. */
export const mono =
  "ui-monospace, SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace"
