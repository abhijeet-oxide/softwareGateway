import { Button, Segmented, Tooltip } from "antd";
import { MoonIcon, SunIcon, SystemIcon } from "../icons";
import { useTheme } from "../ThemeProvider";
import type { Density, FontScale, ThemePref } from "../prefs";

// The appearance control, shared so both tools offer the same three choices in
// the same order and a person who learned one already knows the other.
//
// "System" is a first-class option rather than a checkbox beside a toggle: it
// is what most people actually want, and it is the only one of the three that
// keeps being right after sunset.

const THEME_OPTIONS = [
  { value: "light" as ThemePref, icon: <SunIcon />, label: "Light" },
  { value: "dark" as ThemePref, icon: <MoonIcon />, label: "Dark" },
  { value: "system" as ThemePref, icon: <SystemIcon />, label: "System" },
];

export function ThemeSwitch({ showLabels = true }: { showLabels?: boolean }) {
  const { theme, setTheme } = useTheme();
  return (
    <Segmented
      size="small"
      value={theme}
      onChange={(v) => setTheme(v as ThemePref)}
      options={THEME_OPTIONS.map((o) => ({
        value: o.value,
        label: showLabels ? (
          <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
            {o.icon}
            {o.label}
          </span>
        ) : (
          <Tooltip title={o.label}>{o.icon}</Tooltip>
        ),
      }))}
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
        onClick={toggleMode}
        aria-label={mode === "dark" ? "Switch to light theme" : "Switch to dark theme"}
        icon={mode === "dark" ? <SunIcon /> : <MoonIcon />}
      />
    </Tooltip>
  );
}

export function DensitySwitch() {
  const { density, setDensity } = useTheme();
  return (
    <Segmented
      size="small"
      value={density}
      onChange={(v) => setDensity(v as Density)}
      options={[
        { value: "comfortable", label: "Comfortable" },
        { value: "compact", label: "Compact" },
      ]}
    />
  );
}

export function FontScaleSwitch() {
  const { fontScale, setFontScale } = useTheme();
  return (
    <Segmented
      size="small"
      value={fontScale}
      onChange={(v) => setFontScale(v as FontScale)}
      options={[
        { value: "small", label: "Small" },
        { value: "normal", label: "Default" },
        { value: "large", label: "Large" },
      ]}
    />
  );
}

/** All three, for a settings page. */
export default function AppearanceControls() {
  return (
    <div className="ui-appearance">
      <ThemeSwitch />
      <DensitySwitch />
      <FontScaleSwitch />
    </div>
  );
}
