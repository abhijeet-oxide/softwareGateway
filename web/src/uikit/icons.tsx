// The handful of glyphs this kit draws for itself.
//
// Inline SVG rather than an icon package, because the two tools that share this
// folder do not share an icon library - one compiles Iconify sets at build
// time, the other keeps its own registry - and a shared component that imports
// from either one stops being copyable. Six paths are a cheaper dependency
// than an agreement about icon packages.
//
// They render as `span.anticon > svg` so Ant Design's own spacing rules (the
// gap it puts between an icon and a button label) apply to them unchanged.
//
// "Six paths" was true when it was six. The rule it stands for is the one that
// matters: a glyph is added here, drawn on this 16px grid at this weight, and
// never imported from a package one of the two tools does not have.

import type { CSSProperties, ReactNode } from "react";

function Glyph({
  children,
  style,
  label,
}: {
  children: ReactNode;
  style?: CSSProperties;
  label?: string;
}) {
  return (
    <span
      className="anticon ui-icon"
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      style={style}
    >
      <svg viewBox="0 0 16 16" width="1em" height="1em" fill="none" focusable="false">
        {children}
      </svg>
    </span>
  );
}

export type IconProps = { style?: CSSProperties; label?: string };

const stroke = {
  stroke: "currentColor",
  strokeWidth: 1.5,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
};

export function InfoIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <circle cx="8" cy="8" r="6.25" {...stroke} />
      <path d="M8 7.2v4M8 4.9v.01" {...stroke} />
    </Glyph>
  );
}

export function CheckCircleIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <circle cx="8" cy="8" r="6.25" {...stroke} />
      <path d="m5.3 8.2 1.9 1.9 3.5-4" {...stroke} />
    </Glyph>
  );
}

export function WarningIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M8 2.4 14.3 13H1.7L8 2.4Z" {...stroke} />
      <path d="M8 6.6v3M8 11.4v.01" {...stroke} />
    </Glyph>
  );
}

export function ErrorCircleIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <circle cx="8" cy="8" r="6.25" {...stroke} />
      <path d="m5.9 5.9 4.2 4.2M10.1 5.9l-4.2 4.2" {...stroke} />
    </Glyph>
  );
}

export function CheckIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="m3.2 8.6 3.1 3.1 6.5-7.4" {...stroke} />
    </Glyph>
  );
}

export function SunIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <circle cx="8" cy="8" r="3" {...stroke} />
      <path
        d="M8 1.4v1.4M8 13.2v1.4M1.4 8h1.4M13.2 8h1.4M3.3 3.3l1 1M11.7 11.7l1 1M12.7 3.3l-1 1M4.3 11.7l-1 1"
        {...stroke}
      />
    </Glyph>
  );
}

export function MoonIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8Z" {...stroke} />
    </Glyph>
  );
}

export function SystemIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <rect x="1.9" y="3" width="12.2" height="8" rx="1.4" {...stroke} />
      <path d="M5.6 13.4h4.8" {...stroke} />
    </Glyph>
  );
}

export function SpinnerIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <g className="ui-spin">
        <circle cx="8" cy="8" r="6" stroke="currentColor" strokeWidth="1.5" opacity="0.2" />
        <path d="M14 8a6 6 0 0 0-6-6" {...stroke} />
      </g>
    </Glyph>
  );
}

/**
 * The connection, and the connection cut.
 *
 * Two arcs and a source, which is the shape every operating system draws for
 * "signal" - so the crossed version needs no label to be read as its absence.
 * The slash is drawn in the same weight as the arcs rather than heavier: a
 * thick bar over a thin glyph reads as an error badge stuck onto an icon.
 */
export function SignalIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M3.1 6.4a7 7 0 0 1 9.8 0M5.4 8.9a3.7 3.7 0 0 1 5.2 0" {...stroke} />
      <path d="M8 12.1v.01" {...stroke} />
    </Glyph>
  );
}

export function SignalOffIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M3.1 6.4a7 7 0 0 1 3.1-1.8M10.4 5.1a7 7 0 0 1 2.5 1.3M6.2 9.3a3.7 3.7 0 0 1 2.6-.5" {...stroke} />
      <path d="M8 12.1v.01" {...stroke} />
      <path d="M2.6 2.6l10.8 10.8" {...stroke} />
    </Glyph>
  );
}

/** Try that again: a circular arrow, for a retry that is offered rather than waited for. */
export function RefreshIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M13.2 8a5.2 5.2 0 1 1-1.6-3.7" {...stroke} />
      <path d="M13.4 2.9v2.9h-2.9" {...stroke} />
    </Glyph>
  );
}

export function ChevronDownIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="m4 6.2 4 4 4-4" {...stroke} />
    </Glyph>
  );
}

export function ChevronLeftIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="m9.6 4 -4 4 4 4M13 4l-4 4 4 4" {...stroke} />
    </Glyph>
  );
}

export function ChevronRightIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="m6.4 4 4 4-4 4M3 4l4 4-4 4" {...stroke} />
    </Glyph>
  );
}

/* WHO SOMEBODY IS, as six glyphs.
 *
 * They badge the avatar at the navigation's foot, where the account's standing
 * was a word and only a word: "Admin", "Operator", "Security", "Reader",
 * "User". Five words in 10.5px grey type, in the same position, differing by a
 * few letters - so the one fact that decides whether every control on screen
 * is available to you was the least glanceable thing in the rail.
 *
 * Chosen so the SHAPES differ rather than the details: a shield with a tick, a
 * spanner, a plain shield, an eye, a person, a padlock. At 9px inside a badge
 * that is all that survives, and it is enough - the word is still underneath,
 * and the tooltip names the role in full.
 */

/** An administrator: everything in the tenant. */
export function ShieldCheckIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M8 1.9 13 3.6v4c0 3-2 5.2-5 6.5-3-1.3-5-3.5-5-6.5v-4Z" {...stroke} />
      <path d="M5.9 7.6 7.4 9.1l2.9-3" {...stroke} />
    </Glyph>
  );
}

/** An operator: may request work, may not change configuration. */
export function WrenchIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path
        d="M9.6 2.5a3.2 3.2 0 0 1 3.9 4.2l-1.7-1.7-1.8 1.8 1.7 1.7A3.2 3.2 0 0 1 7.5 4.6L3.9 8.2"
        {...stroke}
      />
      <path d="M3.2 11.1 6 8.3l1.7 1.7-2.8 2.8a1.2 1.2 0 0 1-1.7-1.7Z" {...stroke} />
    </Glyph>
  );
}

/** Security: reads findings and the audit trail across the estate. */
export function ShieldIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M8 1.9 13 3.6v4c0 3-2 5.2-5 6.5-3-1.3-5-3.5-5-6.5v-4Z" {...stroke} />
    </Glyph>
  );
}

/** A reader: everything visible, nothing changeable. */
export function EyeIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <path d="M1.6 8S3.9 3.9 8 3.9 14.4 8 14.4 8 12.1 12.1 8 12.1 1.6 8 1.6 8Z" {...stroke} />
      <circle cx="8" cy="8" r="1.9" {...stroke} />
    </Glyph>
  );
}

/** A person whose access is a set of products rather than a tenant role. */
export function UserIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <circle cx="8" cy="5.6" r="2.7" {...stroke} />
      <path d="M2.9 13.6a5.1 5.1 0 0 1 10.2 0" {...stroke} />
    </Glyph>
  );
}

/** Provisioned and granted nothing: the reason every control is disabled. */
export function LockIcon(p: IconProps) {
  return (
    <Glyph {...p}>
      <rect x="3.1" y="6.9" width="9.8" height="6.6" rx="1.6" {...stroke} />
      <path d="M5.6 6.9V5.1a2.4 2.4 0 0 1 4.8 0v1.8" {...stroke} />
    </Glyph>
  );
}
