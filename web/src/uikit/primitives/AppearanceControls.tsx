import type { ReactNode } from "react";
import { Button, Segmented, Tooltip } from "antd";
import { MoonIcon, SunIcon } from "../icons";
import { useTheme } from "../ThemeProvider";
import type { Density, FontScale, ThemePref } from "../prefs";
import { pointOf } from "../themeTransition";

// The appearance controls, shared so both tools offer the same three choices,
// in the same order, with the same words. A person who learned one already
// knows the other, and neither can quietly grow a fourth option the other
// lacks.
//
// They read the ThemeProvider rather than any app's own store, which is what
// lets one implementation serve an app that owns its settings model and one
// that does not: the provider is controlled in the first case and
// self-persisting in the second, and the control cannot tell the difference.

/**
 * ThemeTile is a MINIATURE OF THE APPLICATION ITSELF: navy navigation, canvas,
 * two content lines, drawn in the palette it would apply. The "system" tile is
 * split diagonally between both.
 *
 * A picture answers "what will this look like?" faster than any label, which
 * is why this is three tiles rather than three words. The miniature reads its
 * colours from the tokens, so it keeps telling the truth after a reskin - a
 * hardcoded navy would go on advertising a palette nobody ships any more.
 */
function ThemeTile({
  value,
  label,
  selected,
  onSelect,
}: {
  value: ThemePref;
  label: string;
  selected: boolean;
  onSelect: (from?: { x: number; y: number }) => void;
}) {
  const mini = (dark: boolean) => (
    <span
      className={`ui-theme-mini${dark ? " is-dark" : ""}`}
      style={
        // The system tile layers this twice with a diagonal clip.
        value === "system" && dark
          ? { clipPath: "polygon(100% 0, 100% 100%, 0 100%)" }
          : undefined
      }
    >
      <span className="ui-theme-mini-nav" />
      <span className="ui-theme-mini-page">
        <span className="ui-theme-mini-line is-long" />
        <span className="ui-theme-mini-line is-short" />
        <span className="ui-theme-mini-accent" />
      </span>
    </span>
  );
  return (
    <button
      type="button"
      className={`ui-theme-tile${selected ? " is-selected" : ""}`}
      // The reveal grows from the tile that was clicked, so the change starts
      // where the reader's attention already is.
      onClick={(e) => onSelect(pointOf(e))}
      aria-pressed={selected}
      aria-label={`${label} theme`}
    >
      <span className="ui-theme-tile-art">
        {mini(value === "dark")}
        {value === "system" && mini(true)}
      </span>
      <span className="ui-theme-tile-label">{label}</span>
    </button>
  );
}

const THEME_OPTIONS: Array<{ value: ThemePref; label: string }> = [
  { value: "light", label: "Light" },
  { value: "dark", label: "Dark" },
  { value: "system", label: "System" },
];

export function ThemeControl() {
  const { theme, setTheme } = useTheme();
  return (
    <div className="ui-theme-tiles">
      {THEME_OPTIONS.map((o) => (
        <ThemeTile
          key={o.value}
          value={o.value}
          label={o.label}
          selected={theme === o.value}
          onSelect={(from) => setTheme(o.value, from)}
        />
      ))}
    </div>
  );
}

/** The compact form, for a toolbar with no room for three tiles. */
export function ThemeSwitch() {
  const { theme, setTheme } = useTheme();
  return (
    <Segmented
      size="small"
      value={theme}
      onChange={(v) => setTheme(v as ThemePref)}
      options={THEME_OPTIONS}
    />
  );
}

/** A single button that flips light and dark, for a top bar with no room for
 *  three. It settles the PREFERENCE on the result rather than leaving it on
 *  "system", because a toggle that the OS overrides an hour later is a toggle
 *  that appears not to work. */
export function ThemeToggleButton() {
  const { mode, toggleMode } = useTheme();
  return (
    <Tooltip title={mode === "dark" ? "Switch to light" : "Switch to dark"}>
      <Button
        type="text"
        // The point is what turns a theme switch into a reveal: the new theme
        // grows out of the button the reader just pressed. A keyboard
        // activation has no coordinates and correctly gets an instant switch.
        onClick={(e) => toggleMode(pointOf(e))}
        aria-label={mode === "dark" ? "Switch to light theme" : "Switch to dark theme"}
        icon={mode === "dark" ? <SunIcon /> : <MoonIcon />}
      />
    </Tooltip>
  );
}

export function FontScaleControl() {
  const { fontScale, setFontScale } = useTheme();
  // The label is set in the size it selects, so the control demonstrates its
  // own effect rather than describing it.
  const opt = (value: FontScale, label: string, px: number) => ({
    value,
    label: (
      <span style={{ display: "inline-flex", alignItems: "baseline", gap: 6 }}>
        <span style={{ fontSize: px, fontWeight: 600, lineHeight: 1 }}>Aa</span>
        {label}
      </span>
    ),
  });
  return (
    <Segmented
      value={fontScale}
      onChange={(v) => setFontScale(v as FontScale)}
      options={[opt("small", "Small", 12), opt("normal", "Default", 14), opt("large", "Large", 16)]}
    />
  );
}

export function DensityControl() {
  const { density, setDensity } = useTheme();
  return (
    <Segmented
      value={density}
      onChange={(v) => setDensity(v as Density)}
      options={[
        { value: "comfortable", label: "Comfortable" },
        { value: "compact", label: "Compact" },
      ]}
    />
  );
}

/** One labelled row of a settings surface. */
export function SettingRow({
  title,
  description,
  control,
  stacked,
}: {
  title: ReactNode;
  description?: ReactNode;
  control: ReactNode;
  /** put the control under the text rather than beside it, for a wide one */
  stacked?: boolean;
}) {
  return (
    <div className={`ui-setting-row${stacked ? " is-stacked" : ""}`}>
      <div className="ui-setting-row-text">
        <div className="ui-setting-row-title">{title}</div>
        {description && <div className="ui-setting-row-desc">{description}</div>}
      </div>
      <div className="ui-setting-row-control">{control}</div>
    </div>
  );
}

/**
 * The whole Appearance section, copy included.
 *
 * The words are here rather than at each call site on purpose: "System follows
 * your device" is the same sentence in every tool, and two products that
 * explain the same control differently are two products.
 */
export default function AppearanceSettings() {
  return (
    <div className="ui-settings-group">
      <SettingRow
        stacked
        title="Theme"
        description="System follows your device and switches automatically. The toggle in the top bar keeps working for quick switches."
        control={<ThemeControl />}
      />
      <SettingRow
        title="Text size"
        description="Applies everywhere, from tables to dialogs."
        control={<FontScaleControl />}
      />
      <SettingRow
        title="Density"
        description="Compact tightens controls and tables to fit more on screen."
        control={<DensityControl />}
      />
    </div>
  );
}
