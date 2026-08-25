// The Ant Design half of the theme.
//
// Everything a component library draws - a table header, a drawer, a modal, a
// focus ring - comes from these tokens, and they are derived from the SAME
// tokens.ts the CSS variables are generated from. That is what stops the two
// halves drifting: change a colour in one file and the primitives, the custom
// surfaces AND the component library all move together.
//
// These must be REAL colour values, not var() references: Ant derives whole
// families (a hover, a border, a disabled fill) from each one, and it cannot
// derive anything from a string it has to ask the browser to resolve. So this
// file reads tokens.ts directly, and everything the app writes by hand reads
// color.ts instead.

import { theme as antdTheme, type ThemeConfig } from "antd";
import { tokens, type Palette } from "./tokens";
import type { Density, FontScale, Mode } from "./prefs";

/**
 * THE ONE PLACE THE TWO HALVES ARE RECONCILED.
 *
 * Ant Design does not use `colorPrimary` as given: it treats it as a SEED and
 * derives the shade it actually paints, which under the dark algorithm is a
 * different colour entirely (#4d94e8 went in, #4481c8 came out). So a primary
 * button and anything written with `var(--brand)` were two different blues on
 * the same screen - precisely the drift this folder exists to prevent, hiding
 * in the one place nobody looks because both halves came from one file.
 *
 * The same is true of success, warning and error: a status pill and a Tag that
 * both mean "healthy" were different greens.
 *
 * So the CSS variables are not written by hand any more. They are RESOLVED
 * FROM ANT'S OWN TOKENS - ask the component library what it is going to paint,
 * then hand that to the stylesheet - which makes disagreement impossible
 * rather than merely unlikely.
 */
export function resolvePalette(mode: Mode): Palette {
  const p = mode === "dark" ? tokens.dark : tokens.light;
  const t = antdTheme.getDesignToken(baseConfig(mode));
  return {
    ...p,
    brand: t.colorPrimary,
    brandStrong: t.colorPrimaryHover,
    ok: t.colorSuccess,
    pending: t.colorWarning,
    review: t.colorInfo,
    danger: t.colorError,
    // The selected navigation item IS the brand: one accent with one meaning.
    // It used to be a separate blue, and the same value in both modes, so the
    // selected item and every primary button on the page disagreed - in dark
    // mode obviously so.
    navBgActive: t.colorPrimary,
  };
}

/** The seed half of the theme, shared by resolvePalette and buildTheme so the
 *  resolution above cannot be asked about a config the app never builds. */
function baseConfig(mode: Mode): ThemeConfig {
  const dark = mode === "dark";
  const p = dark ? tokens.dark : tokens.light;
  return {
    algorithm: dark ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
    token: {
      colorPrimary: p.brand,
      colorInfo: p.review,
      colorSuccess: p.ok,
      colorWarning: p.pending,
      colorError: p.danger,
      // A seed of its own since Ant 5.17. Left unset it derives from blue and
      // a link came out a different colour from every primary button on the
      // page; given the PAINTED value it derives a second time and lands
      // somewhere else again. It takes the same seed as the primary, and then
      // the two agree exactly.
      colorLink: p.brand,
    },
  };
}

export function buildTheme(
  mode: Mode,
  scale: FontScale = "normal",
  density: Density = "comfortable",
): ThemeConfig {
  const dark = mode === "dark";
  // TWO palettes, and the distinction is the whole point.
  //
  // `seed` is what Ant is GIVEN. `p` is what Ant will PAINT once its algorithm
  // has had its way with the seed, and it is what the stylesheet was handed.
  // Feeding `p` back in as the seed would derive a second time and land
  // somewhere else again - which is exactly the bug this pair exists to close.
  const seed = dark ? tokens.dark : tokens.light;
  const p = resolvePalette(mode);
  const base =
    scale === "large"
      ? tokens.type.fontSizeBase + 2
      : scale === "small"
        ? tokens.type.fontSizeBase - 1
        : tokens.type.fontSizeBase;
  const compact = density === "compact";
  return {
    algorithm: dark ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
    token: {
      // Seeds. Ant re-derives each of these; resolvePalette reports where they
      // land, and the stylesheet is given that.
      colorPrimary: seed.brand,
      colorInfo: seed.review,
      colorSuccess: seed.ok,
      colorWarning: seed.pending,
      colorError: seed.danger,
      // Also a seed (see baseConfig), so it takes the seed, not the painted
      // value. Handed the painted one it derives again and a link stops
      // matching the button beside it.
      colorLink: seed.brand,

      // The status SURFACES, stated rather than derived. The semantic colours
      // above are chosen for text and icons, so they are dark enough to read;
      // left to itself Ant derives an Alert's background and a
      // `color="success"` Tag's fill from them, and a dark green derives a dark
      // green surface. The result was a filled slab beside the light pastel
      // pills, which is what made one page look like a different design system
      // from the next.
      colorSuccessBg: p.okBg,
      colorSuccessBorder: p.okBd,
      colorWarningBg: p.pendingBg,
      colorWarningBorder: p.pendingBd,
      colorErrorBg: p.dangerBg,
      colorErrorBorder: p.dangerBd,
      colorInfoBg: p.reviewBg,
      colorInfoBorder: p.reviewBd,

      borderRadius: tokens.shape.borderRadius,
      controlHeight: compact ? tokens.shape.controlHeight - 4 : tokens.shape.controlHeight,
      fontSize: base,
      fontFamily: tokens.type.fontFamily,
      fontFamilyCode: tokens.type.monoFamily,

      // three planes: pastel page canvas < content surface < floating surface
      colorBgLayout: p.canvas,
      colorBgContainer: p.surface,
      colorBgElevated: dark ? p.surface2 : p.surface,
      colorBorder: p.borderStrong,
      colorBorderSecondary: p.border,
      colorText: p.text,
      colorTextHeading: p.text,
      colorTextSecondary: p.text2,
      colorTextTertiary: p.text3,

      // Ant's default is a black halo with no offset. Depth should read as
      // light falling on something, so it gets an offset and the palette's own
      // shade rather than an outline pretending to be a shadow.
      boxShadow: dark
        ? "0 1px 2px rgba(0,0,0,0.5)"
        : "0 1px 2px rgba(16,24,40,0.05), 0 1px 3px rgba(16,24,40,0.04)",
      boxShadowSecondary: dark
        ? "0 18px 44px -14px rgba(0,0,0,0.72), 0 6px 16px -8px rgba(0,0,0,0.6)"
        : "0 12px 32px -10px rgba(16,24,40,0.18), 0 4px 10px -4px rgba(16,24,40,0.1)",
    },
    components: {
      Layout: {
        headerHeight: 48,
        headerPadding: "0 16px",
        siderBg: p.navBg,
        headerBg: p.surface,
        bodyBg: p.canvas,
      },
      Menu: {
        darkItemBg: p.navBg,
        darkSubMenuItemBg: p.navBg,
        darkItemSelectedBg: p.navBgActive,
        darkItemColor: p.navFg,
        darkItemHoverBg: p.navBgHover,
      },
      Button: {
        fontWeight: 500,
        primaryShadow: "none",
        defaultShadow: "none",
        dangerShadow: "none",
      },
      Card: {
        boxShadowTertiary: dark
          ? "5px 5px 12px rgba(0,0,0,0.45), -4px -4px 10px rgba(255,255,255,0.035)"
          : "5px 5px 12px rgba(163,177,198,0.28), -4px -4px 10px rgba(255,255,255,0.85)",
        headerFontSize: base,
      },
      Table: {
        headerBg: p.surface2,
        headerColor: p.text2,
        headerSplitColor: "transparent",
        borderColor: p.border,
        cellPaddingBlock: compact ? 6 : 10,
        cellPaddingBlockSM: 4,
        cellPaddingInlineSM: 8,
        rowHoverBg: dark ? p.surface2 : p.brandSoft,
      },
      Tabs: {
        titleFontSize: base,
        horizontalItemPadding: "10px 4px",
        horizontalMargin: "0",
      },
      Tag: {
        defaultBg: p.surface2,
        defaultColor: p.text2,
      },
      Tree: {
        nodeSelectedBg: p.brandSoft,
      },
      Segmented: {
        itemSelectedBg: p.surface,
        trackBg: p.surface2,
      },
      Statistic: {
        contentFontSize: scale === "large" ? 24 : 20,
      },
    },
  };
}
