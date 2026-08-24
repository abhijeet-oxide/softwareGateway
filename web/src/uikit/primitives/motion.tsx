import type { CSSProperties, ReactNode } from "react";

// The motion vocabulary, kept deliberately small: content fades in with a
// gentle rise (FadeIn), and collections cascade with a short stagger
// (Stagger + StaggerItem). A virtualized data grid stays out of this on
// purpose - rows that animate as they scroll into view are noise, not motion.
//
// These are CSS animations (see base.css), not a JavaScript animation runtime.
// Three fades and a stagger did not justify an animation library in the ENTRY
// bundle, downloaded by every visitor before anything renders, to express what
// @keyframes expresses for nothing. Timing comes from the same motion tokens
// the rest of the system uses, so the vocabulary cannot drift, and
// reduced-motion is handled by the stylesheet rather than by every caller.

export function FadeIn({
  children,
  delay = 0,
  y = 8,
  className,
  style,
}: {
  children: ReactNode;
  delay?: number;
  /** initial rise distance in px */
  y?: number;
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <div
      className={`ui-fade-in${className ? ` ${className}` : ""}`}
      style={{ "--rise": `${y}px`, "--delay": `${delay}s`, ...style } as CSSProperties}
    >
      {children}
    </div>
  );
}

export function Stagger({
  children,
  className,
  style,
}: {
  children: ReactNode;
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <div className={`ui-stagger${className ? ` ${className}` : ""}`} style={style}>
      {children}
    </div>
  );
}

export function StaggerItem({
  children,
  className,
  style,
}: {
  children: ReactNode;
  className?: string;
  style?: CSSProperties;
}) {
  // The cascade delay comes from this element's position under .ui-stagger, so
  // items stay ordinary children and callers pass nothing extra.
  return (
    <div className={`ui-stagger-item${className ? ` ${className}` : ""}`} style={style}>
      {children}
    </div>
  );
}
