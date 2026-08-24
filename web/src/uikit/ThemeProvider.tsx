// The one place a tool becomes themed.
//
// Mount this at the root and everything below it - Ant Design components, the
// primitives in this folder, and any hand-written surface that names a
// variable - is on the same design system.
//
// It runs in either of two modes, which is what lets two very different apps
// share one file:
//
//   UNCONTROLLED (pass nothing): it keeps the appearance itself and persists
//     it. Right for an app with no settings model of its own.
//   CONTROLLED (pass `value` and `onChange`): the app's own store is the truth
//     and this just paints it. Right for an app that already persists a user
//     profile, and the reason adopting the kit does not mean adopting a second
//     copy of everybody's preferences.
//
// Either way the same three things happen, and all three matter:
//   - the mode is written to <html data-theme> so PLAIN CSS can see it (that
//     is how every variable in tokens.ts flips at once),
//   - density and font scale are written to <html> as well, so hand-rolled
//     surfaces answer to the same controls as the component library,
//   - "follow system" is watched LIVE, not read once at boot.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { App as AntApp, ConfigProvider } from "antd";
import { buildTheme } from "./antd";
import {
  defaultAppearance,
  loadAppearance,
  resolveMode,
  saveAppearance,
  watchSystemMode,
  type Appearance,
  type Density,
  type FontScale,
  type Mode,
  type ThemePref,
} from "./prefs";

export interface ThemeContextValue extends Appearance {
  /** the mode actually painted (a "system" preference is already resolved) */
  mode: Mode;
  setTheme: (pref: ThemePref) => void;
  setFontScale: (scale: FontScale) => void;
  setDensity: (density: Density) => void;
  /** flip between light and dark, settling the preference on the result */
  toggleMode: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

/** Read the current appearance. Throws outside the provider, deliberately: a
 *  control that can change the theme has no meaningful behaviour without one. */
export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used inside <ThemeProvider>");
  return ctx;
}

export function ThemeProvider({
  children,
  value,
  onChange,
  /** wrap in Ant's App so message/notification/modal have a themed context.
   *  Turn off only if the app mounts AntApp itself. */
  withAntApp = true,
}: {
  children: ReactNode;
  /** controlled appearance; omit to let the provider own and persist it */
  value?: Appearance;
  onChange?: (next: Appearance) => void;
  withAntApp?: boolean;
}) {
  const controlled = value !== undefined;
  // Uncontrolled state is seeded lazily so localStorage is read once, not on
  // every render, and never during a server render.
  const [own, setOwn] = useState<Appearance>(() =>
    typeof window === "undefined" ? defaultAppearance : loadAppearance(),
  );
  const appearance = controlled ? value : own;

  const update = useCallback(
    (patch: Partial<Appearance>) => {
      const next = { ...appearance, ...patch };
      if (controlled) onChange?.(next);
      else {
        setOwn(next);
        saveAppearance(next);
      }
    },
    [appearance, controlled, onChange],
  );

  // "system" is resolved on every render rather than stored, so the painted
  // mode cannot go stale; systemMode below is what makes the OS flip arrive.
  const [systemMode, setSystemMode] = useState<Mode>(() => resolveMode("system"));
  useEffect(() => {
    if (appearance.theme !== "system") return;
    return watchSystemMode(setSystemMode);
  }, [appearance.theme]);

  const mode: Mode = appearance.theme === "system" ? systemMode : appearance.theme;

  useEffect(() => {
    const root = document.documentElement;
    root.dataset.theme = mode;
    // colorScheme is what gives form controls, scrollbars and the space around
    // the page the right base colours. Without it a dark page keeps a white
    // scrollbar and a white overscroll band.
    root.style.colorScheme = mode;
  }, [mode]);

  useEffect(() => {
    const root = document.documentElement;
    root.dataset.density = appearance.density;
    root.dataset.fontscale = appearance.fontScale;
  }, [appearance.density, appearance.fontScale]);

  const ctx = useMemo<ThemeContextValue>(
    () => ({
      ...appearance,
      mode,
      setTheme: (theme) => update({ theme }),
      setFontScale: (fontScale) => update({ fontScale }),
      setDensity: (density) => update({ density }),
      toggleMode: () => update({ theme: mode === "dark" ? "light" : "dark" }),
    }),
    [appearance, mode, update],
  );

  const themed = (
    <ConfigProvider
      theme={buildTheme(mode, appearance.fontScale, appearance.density)}
      componentSize={appearance.density === "compact" ? "small" : "middle"}
    >
      {withAntApp ? <AntApp>{children}</AntApp> : children}
    </ConfigProvider>
  );

  return <ThemeContext.Provider value={ctx}>{themed}</ThemeContext.Provider>;
}
