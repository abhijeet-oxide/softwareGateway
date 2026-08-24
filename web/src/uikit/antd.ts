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
import { tokens } from "./tokens";
import type { Density, FontScale, Mode } from "./prefs";

export function buildTheme(
  mode: Mode,
  scale: FontScale = "normal",
  density: Density = "comfortable",
): ThemeConfig {
  const dark = mode === "dark";
  const p = dark ? tokens.dark : tokens.light;
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
      colorPrimary: p.brand,
      colorInfo: p.brand,
      colorLink: p.brand,
      colorSuccess: p.ok,
      colorWarning: p.pending,
      colorError: p.danger,

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
