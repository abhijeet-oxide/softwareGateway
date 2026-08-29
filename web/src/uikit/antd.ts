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
      // The whole radius ramp, stated. Left to itself Ant derives these from
      // `borderRadius` and lands on 4 for a tag and 12 for a modal, which is
      // not the same ramp the stylesheet uses - and a card with a 12px corner
      // holding a button with a 4px one is the tell that two systems are
      // drawing the same screen.
      borderRadiusXS: 4,
      borderRadiusSM: 6,
      borderRadiusLG: 12,
      controlHeight: compact ? tokens.shape.controlHeight - 4 : tokens.shape.controlHeight,
      fontSize: base,
      fontFamily: tokens.type.fontFamily,
      fontFamilyCode: tokens.type.monoFamily,
      // A hairline, not a rule. The platform separates with the thinnest line
      // the display can draw and lets shadow and value do the rest.
      lineWidth: 1,
      colorSplit: p.border,

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

      // The same three-ingredient elevation the stylesheet uses (see
      // tokens.css): a hairline for the edge, a tight contact shadow, a wide
      // ambient cast. Ant's own default is a black halo with no offset, which
      // reads as a glow rather than as a thing sitting on a page.
      boxShadow: dark
        ? "0 0 0 0.5px rgba(255,255,255,0.06), 0 1px 1px rgba(0,0,0,0.4), 0 3px 8px -2px rgba(0,0,0,0.45)"
        : "0 0 0 0.5px rgba(16,24,40,0.05), 0 1px 1px rgba(16,24,40,0.04), 0 3px 8px -2px rgba(16,24,40,0.06)",
      boxShadowSecondary: dark
        ? "0 0 0 0.5px rgba(255,255,255,0.09), 0 8px 20px -6px rgba(0,0,0,0.55), 0 32px 64px -24px rgba(0,0,0,0.7)"
        : "0 0 0 0.5px rgba(16,24,40,0.08), 0 8px 20px -6px rgba(16,24,40,0.14), 0 32px 64px -24px rgba(16,24,40,0.24)",
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
        itemBorderRadius: 8,
        itemHeight: 34,
      },
      Button: {
        fontWeight: 500,
        primaryShadow: "none",
        defaultShadow: "none",
        dangerShadow: "none",
        // A button is the control people touch most, so it gets the platform's
        // proportions rather than the library's: a little wider than tall, and
        // the same corner as the field beside it.
        paddingInline: 14,
        paddingInlineSM: 10,
        contentFontSize: base,
      },
      Card: {
        boxShadowTertiary: dark
          ? "0 0 0 0.5px rgba(255,255,255,0.06), 0 1px 1px rgba(0,0,0,0.4), 0 3px 8px -2px rgba(0,0,0,0.45)"
          : "0 0 0 0.5px rgba(16,24,40,0.05), 0 1px 1px rgba(16,24,40,0.04), 0 3px 8px -2px rgba(16,24,40,0.06)",
        headerFontSize: base,
        borderRadiusLG: 12,
      },
      Table: {
        headerBg: p.surface2,
        headerColor: p.text2,
        headerSplitColor: "transparent",
        borderColor: p.border,
        cellPaddingBlock: compact ? 6 : 10,
        cellPaddingBlockSM: 4,
        cellPaddingInlineSM: 8,
        // A row lights up on approach without changing colour: the brand tint
        // used to be the hover, which meant every row looked selected as the
        // pointer crossed the table. Selection is the accent's job.
        rowHoverBg: p.surface2,
        rowSelectedBg: p.brandSoft,
        rowSelectedHoverBg: p.brandSoft,
      },
      Tabs: {
        titleFontSize: base,
        horizontalItemPadding: "10px 4px",
        horizontalMargin: "0",
      },
      Tag: {
        defaultBg: p.surface2,
        defaultColor: p.text2,
        borderRadiusSM: 6,
      },
      Tree: {
        nodeSelectedBg: p.brandSoft,
      },
      // The platform's segmented control: a capsule track with a raised
      // capsule riding inside it.
      Segmented: {
        itemSelectedBg: p.surface,
        trackBg: p.surface2,
        borderRadius: 999,
        borderRadiusSM: 999,
        borderRadiusLG: 999,
        trackPadding: 2,
      },
      Statistic: {
        contentFontSize: scale === "large" ? 24 : 20,
      },
      Modal: {
        borderRadiusLG: 18,
        // A sheet dims the page rather than blacking it out, so what it is
        // over stays readable underneath it.
        contentBg: p.surface,
      },
      Drawer: {
        borderRadiusLG: 18,
      },
      Tooltip: {
        borderRadius: 8,
        colorBgSpotlight: dark ? "rgba(58,58,60,0.92)" : "rgba(28,28,30,0.88)",
      },
      Popover: {
        borderRadiusLG: 12,
      },
      Dropdown: {
        borderRadiusLG: 12,
        // 8 rather than Ant's 4: an item inside a 12px menu wants a corner
        // that belongs to the same family.
        borderRadiusSM: 8,
        controlItemBgHover: p.brandSoft,
      },
      Input: {
        paddingInline: 11,
      },
      Select: {
        borderRadiusSM: 6,
        optionSelectedBg: p.brandSoft,
      },
      Switch: {
        // The platform's switch is a full capsule and noticeably wider than
        // the library's default, which is what makes it read as a physical
        // toggle rather than as a rounded checkbox.
        trackHeight: 22,
        trackMinWidth: 38,
        handleSize: 18,
      },
      Notification: {
        borderRadiusLG: 14,
      },
      Message: {
        borderRadiusLG: 12,
      },
    },
  };
}
