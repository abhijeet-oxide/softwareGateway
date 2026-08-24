// The shared design system, in one import.
//
//   import { ThemeProvider, SectionCard, StatusPill, c } from "./uikit";
//
// Everything below is IDENTICAL in every tool that uses this folder. See
// README.md for what may and may not be changed here.

// --- the theme -------------------------------------------------------------
export { ThemeProvider, useTheme } from "./ThemeProvider";
export type { ThemeContextValue } from "./ThemeProvider";
export { buildTheme } from "./antd";
export {
  tokens,
  defaultTokens,
  presets,
  ACTIVE_PRESET,
  tokenOverrides,
  VAR_MAP,
  renderRootCss,
  deepMerge,
} from "./tokens";
export type { Palette, ThemeTokens, DeepPartial } from "./tokens";
export { faviconHref, documentTitle } from "./brand";
export type { BrandIdentity, BrandLogo } from "./brand";
export {
  defaultAppearance,
  loadAppearance,
  saveAppearance,
  resolveMode,
  watchSystemMode,
} from "./prefs";
export type { Appearance, Density, FontScale, Mode, ThemePref } from "./prefs";

// --- reading a colour ------------------------------------------------------
export {
  c,
  cssVar,
  withAlpha,
  mono,
  fontUI,
  severity,
  severitySurface,
  verdict,
  envColors,
  envHex,
  isProductionEnv,
} from "./color";
export type { Severity, Verdict } from "./color";

// --- glyphs ----------------------------------------------------------------
export {
  CheckCircleIcon,
  CheckIcon,
  ErrorCircleIcon,
  InfoIcon,
  MoonIcon,
  SpinnerIcon,
  SunIcon,
  SystemIcon,
  WarningIcon,
} from "./icons";
export type { IconProps } from "./icons";

// --- primitives ------------------------------------------------------------
export { StatusPill, ChangeChip } from "./primitives/StatusPill";
export type { PillTone, ChangeKind } from "./primitives/StatusPill";
export { SeverityTag, SeverityDot, VerdictTag } from "./primitives/SeverityTag";
export { default as StatTile } from "./primitives/StatTile";
export { default as PageHeader } from "./primitives/PageHeader";
export { default as SectionCard } from "./primitives/SectionCard";
export { default as AttentionCard } from "./primitives/AttentionCard";
export type { AttentionSeverity } from "./primitives/AttentionCard";
export { default as Toolbar } from "./primitives/Toolbar";
export { default as EmptyState } from "./primitives/EmptyState";
export { default as InlineNotice } from "./primitives/InlineNotice";
export type { NoticeTone } from "./primitives/InlineNotice";
export { default as LoadingStage } from "./primitives/LoadingStage";
export { default as Stepper } from "./primitives/Stepper";
export type { StepDef } from "./primitives/Stepper";
export { default as Kbd } from "./primitives/Kbd";
export { default as Mono } from "./primitives/Mono";
export { FadeIn, Stagger, StaggerItem } from "./primitives/motion";
export { BrandMark, BrandLockup } from "./primitives/BrandLockup";
export {
  default as AppearanceControls,
  ThemeSwitch,
  ThemeToggleButton,
  DensitySwitch,
  FontScaleSwitch,
} from "./primitives/AppearanceControls";
