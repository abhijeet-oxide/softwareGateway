import type { CSSProperties, ReactNode } from "react";

// StatusPill is THE status chip of the design system: a pastel tinted pill
// with a dot and a short label. Tones map to the semantic palette.
//
// Use it for OPERATIONAL STATE (Synced, Pending review, Failing, Active).
// Never for environment identity: an environment has its own colours, and a
// production instance that borrowed the danger tone would say something about
// its health that nobody meant.

// `accent` is the odd one out and earns its place: it is for a state that is
// NOTABLE without being good, bad, waiting or nothing - a release sitting at
// the vendor, a thing that has been singled out. Given the neutral tone those
// states read as disabled beside the coloured ones, and given any of the four
// semantic tones they claim a health they do not have.
export type PillTone = "ok" | "pending" | "review" | "danger" | "neutral" | "accent";

export function StatusPill({
  tone,
  children,
  dot = true,
  icon,
  size = "md",
  style,
  title,
}: {
  tone: PillTone;
  children: ReactNode;
  /** show the leading status dot (skipped automatically when an icon is given) */
  dot?: boolean;
  icon?: ReactNode;
  size?: "sm" | "md";
  style?: CSSProperties;
  title?: string;
}) {
  return (
    <span
      title={title}
      style={style}
      className={`ui-pill is-${size} tone-${tone}`}
    >
      {icon ?? (dot && <span className="ui-pill-dot" />)}
      {children}
    </span>
  );
}

// ChangeChip labels a diff row: Modified (blue), Added (green), Removed (red),
// Unchanged (neutral). The words are always written out, so the row still
// reads correctly in greyscale.
export type ChangeKind = "modified" | "added" | "removed" | "unchanged";

const CHANGE_TONE: Record<ChangeKind, PillTone> = {
  modified: "review",
  added: "ok",
  removed: "danger",
  unchanged: "neutral",
};
const CHANGE_LABEL: Record<ChangeKind, string> = {
  modified: "Modified",
  added: "Added",
  removed: "Removed",
  unchanged: "Unchanged",
};

export function ChangeChip({ kind, size = "sm" }: { kind: ChangeKind; size?: "sm" | "md" }) {
  return (
    <StatusPill tone={CHANGE_TONE[kind]} dot={false} size={size}>
      {CHANGE_LABEL[kind]}
    </StatusPill>
  );
}
