// Appearance preferences: what a person tunes about their own copy of the tool.
//
// Deliberately tiny and dependency-free. An app with its own settings model
// (a store, a server-side profile) drives the provider in CONTROLLED mode and
// never touches this file; an app without one gets persistence for free.
//
// The storage key is shared on purpose. Two tools hosted on the same origin
// then agree about dark mode, which is the one preference a person expects to
// set once - and on different origins the key simply does not collide.

export type Mode = "light" | "dark";
/** What the user ASKED for; "system" follows the OS and updates live. */
export type ThemePref = Mode | "system";
export type FontScale = "small" | "normal" | "large";
export type Density = "comfortable" | "compact";

export interface Appearance {
  /** requested theme; resolve with resolveMode() before painting */
  theme: ThemePref;
  fontScale: FontScale;
  density: Density;
}

export const defaultAppearance: Appearance = {
  theme: "system",
  fontScale: "normal",
  density: "comfortable",
};

const KEY = "uikit.appearance.v1";

const THEMES: ThemePref[] = ["light", "dark", "system"];
const SCALES: FontScale[] = ["small", "normal", "large"];
const DENSITIES: Density[] = ["comfortable", "compact"];

/** Resolve a theme preference to the mode to actually paint. */
export function resolveMode(pref: ThemePref): Mode {
  if (pref === "system") {
    return typeof window !== "undefined" &&
      window.matchMedia?.("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  }
  return pref;
}

function sanitize(a: Appearance): Appearance {
  return {
    theme: THEMES.includes(a.theme) ? a.theme : defaultAppearance.theme,
    fontScale: SCALES.includes(a.fontScale) ? a.fontScale : defaultAppearance.fontScale,
    density: DENSITIES.includes(a.density) ? a.density : defaultAppearance.density,
  };
}

export function loadAppearance(): Appearance {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) return sanitize({ ...defaultAppearance, ...(JSON.parse(raw) as Partial<Appearance>) });
  } catch {
    // Unreadable or corrupt: the defaults are a perfectly good answer. A
    // preference failing to load must never be the reason a page does not
    // paint.
  }
  return defaultAppearance;
}

export function saveAppearance(a: Appearance): void {
  try {
    localStorage.setItem(KEY, JSON.stringify(a));
  } catch {
    // Private browsing, a full quota, storage disabled by policy: the session
    // keeps the choice in memory and simply forgets it next time.
  }
}

/** Watch the OS setting, for as long as "system" is the preference. Returns an
 *  unsubscribe, so a caller can hand it straight back from an effect. */
export function watchSystemMode(onChange: (mode: Mode) => void): () => void {
  if (typeof window === "undefined" || !window.matchMedia) return () => {};
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  const apply = () => onChange(mq.matches ? "dark" : "light");
  apply();
  mq.addEventListener("change", apply);
  return () => mq.removeEventListener("change", apply);
}
