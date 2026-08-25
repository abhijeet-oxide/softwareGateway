import { flushSync } from "react-dom";

// THE THEME REVEAL: the new theme spreads as a growing circle from the point
// that was clicked, instead of the page blinking from one palette to the other.
//
// It lives in the kit rather than in one app because it is not a feature, it is
// how this design system CHANGES THEME - and a tool where the lights come on
// smoothly next to a tool where they snap does not read as one product. Every
// consumer of `useTheme()` gets it without asking: the control passes the click
// point, and that is the whole integration.
//
// Three ways it degrades, all to an instant switch and none to a broken one:
// a browser without the View Transitions API, a reader who has asked for
// reduced motion, and any call with no point to grow from (a keyboard, a
// settings page, a system change).

// Typed structurally rather than by augmenting Document: the DOM lib already
// declares this in newer TypeScript versions and disagrees with older ones
// about the signature, so a `declare global` here fails to compile in whichever
// repository is on the other version. The kit has to build under both.
type ViewTransitionCapable = {
  startViewTransition?: (callback: () => void) => { ready: Promise<void> };
};

export interface RevealPoint {
  x: number;
  y: number;
}

/** The point a mouse or keyboard event happened at, or undefined for a keyboard
 *  activation (which has no coordinates and should not pretend to). */
export function pointOf(e: { clientX?: number; clientY?: number } | undefined): RevealPoint | undefined {
  if (!e || !e.clientX || !e.clientY) return undefined;
  return { x: e.clientX, y: e.clientY };
}

/**
 * Apply a theme change, revealed from `point` when that is possible.
 *
 * `apply` must do the whole switch, and it is called INSIDE the transition so
 * the before and after snapshots are exact. It is wrapped in flushSync because
 * the View Transitions API snapshots the DOM the moment the callback returns:
 * without it React has not re-rendered yet, the "after" snapshot is identical
 * to the "before" one, and the circle grows over nothing.
 */
export function revealThemeChange(apply: () => void, point?: RevealPoint): void {
  const reduce =
    typeof window !== "undefined" &&
    window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

  const run = () => flushSync(apply);

  const doc = typeof document === "undefined" ? undefined : (document as unknown as ViewTransitionCapable);
  if (!doc || typeof doc.startViewTransition !== "function" || reduce || !point) {
    apply();
    return;
  }

  const { x, y } = point;
  // Grow to whichever corner is furthest away, so the circle always finishes by
  // covering the window rather than leaving a wedge of the old theme behind.
  const radius = Math.hypot(
    Math.max(x, window.innerWidth - x),
    Math.max(y, window.innerHeight - y),
  );

  const transition = doc.startViewTransition(run);
  transition.ready
    .then(() => {
      document.documentElement.animate(
        { clipPath: [`circle(0px at ${x}px ${y}px)`, `circle(${radius}px at ${x}px ${y}px)`] },
        {
          duration: 550,
          easing: "cubic-bezier(0.2, 0.8, 0.4, 1)",
          pseudoElement: "::view-transition-new(root)",
        },
      );
    })
    .catch(() => {
      // The transition could not start - another one is already running, most
      // likely a double click. The theme is applied either way; only the reveal
      // is skipped, which is the right failure.
    });
}
