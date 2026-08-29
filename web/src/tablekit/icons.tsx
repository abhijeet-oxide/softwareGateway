// The glyphs this kit draws for itself.
//
// Inline SVG rather than an icon package, for the same reason `uikit/icons.tsx`
// gives: the tools that share this folder do not share an icon library. One
// keeps a Phosphor-backed registry, the other compiles Iconify sets at build
// time, and a component that imports from either one stops being copyable the
// moment it lands in the other repository. Eleven paths are a cheaper
// dependency than an agreement about icon packages.
//
// They are drawn on a 16 unit grid at a 1.5 stroke with round caps and joins,
// which is the same optical weight as the shared kit's own glyphs, so a table's
// controls and the page around them read as one hand.
//
// They render as `span.anticon > svg`, so Ant Design's own spacing rules (the
// gap it puts between an icon and a button's label, the alignment inside a menu
// item) apply to them unchanged and a caller can pass them straight to `icon=`.

import type { CSSProperties, ReactNode } from "react";

export type TableIconProps = {
  style?: CSSProperties;
  className?: string;
  /** degrees, clockwise. The pin leans to say which way a column is pinned. */
  rotate?: number;
  label?: string;
};

function Glyph({
  children,
  style,
  className,
  rotate,
  label,
  fill = false,
}: TableIconProps & { children: ReactNode; fill?: boolean }) {
  return (
    <span
      className={["anticon", "tk-icon", className].filter(Boolean).join(" ")}
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      style={rotate ? { ...style, transform: `rotate(${rotate}deg)` } : style}
    >
      <svg
        viewBox="0 0 16 16"
        width="1em"
        height="1em"
        fill={fill ? "currentColor" : "none"}
        stroke={fill ? "none" : "currentColor"}
        strokeWidth={1.5}
        strokeLinecap="round"
        strokeLinejoin="round"
        focusable="false"
      >
        {children}
      </svg>
    </span>
  );
}

/** The grab handle on a column header. Two files of dots: the one shape every
 *  platform agrees means "this can be picked up and moved". */
export function HolderIcon(p: TableIconProps) {
  return (
    <Glyph {...p} fill>
      <circle cx="6" cy="3.5" r="1.15" />
      <circle cx="6" cy="8" r="1.15" />
      <circle cx="6" cy="12.5" r="1.15" />
      <circle cx="10" cy="3.5" r="1.15" />
      <circle cx="10" cy="8" r="1.15" />
      <circle cx="10" cy="12.5" r="1.15" />
    </Glyph>
  );
}

/** Pinned. The filled body says the state is ON, so the mark reads at a glance
 *  in a header that is otherwise all text. */
export function PinFilledIcon(p: TableIconProps) {
  return (
    <Glyph {...p} fill>
      <path d="M9.47 1.6a1 1 0 0 1 1.42 0l3.51 3.51a1 1 0 0 1 0 1.42l-.35.35a2.4 2.4 0 0 1-2.4.6l-.9-.28-2.9 2.9.16 1.2a1.6 1.6 0 0 1-.46 1.35l-.5.5a.8.8 0 0 1-1.14 0L4.1 11.05l-2.5 2.5a.7.7 0 0 1-1-.98l2.52-2.52-1.77-1.77a.8.8 0 0 1 0-1.13l.5-.5a1.6 1.6 0 0 1 1.35-.46l1.2.16 2.9-2.9-.28-.9a2.4 2.4 0 0 1 .6-2.4z" />
    </Glyph>
  );
}

/** Not pinned: the same silhouette, hollow. State is said by fill rather than
 *  by two different drawings, so nothing shifts as it is toggled. */
export function PinIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M9.47 1.6a1 1 0 0 1 1.42 0l3.51 3.51a1 1 0 0 1 0 1.42l-.35.35a2.4 2.4 0 0 1-2.4.6l-.9-.28-2.9 2.9.16 1.2a1.6 1.6 0 0 1-.46 1.35l-.5.5a.8.8 0 0 1-1.14 0L1.83 7.15a.8.8 0 0 1 0-1.13l.5-.5a1.6 1.6 0 0 1 1.35-.46l1.2.16 2.9-2.9-.28-.9a2.4 2.4 0 0 1 .6-2.4z" />
      <path d="M4.6 11.4 1.6 14.4" />
    </Glyph>
  );
}

/** Width: a span between two stops. Used for every "fit this column" action,
 *  so autofit, reset width and fit-the-table all wear one mark. */
export function ColumnWidthIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M2 2.5v11M14 2.5v11" />
      <path d="M4.75 8h6.5" />
      <path d="M6.5 5.75 4.25 8l2.25 2.25M9.5 5.75 11.75 8 9.5 10.25" />
    </Glyph>
  );
}

/** Move: four ways out of one centre. The mark for reordering, and for the
 *  badge that says a reorder is in progress. */
export function MoveIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M8 1.75v12.5M1.75 8h12.5" />
      <path d="M5.75 4 8 1.75 10.25 4M5.75 12 8 14.25 10.25 12M4 5.75 1.75 8 4 10.25M12 5.75 14.25 8 12 10.25" />
    </Glyph>
  );
}

/** Back to how it was: the counter-clockwise arrow every platform uses for
 *  revert. Reset width, reset order, reset the whole layout. */
export function ResetIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M2.5 8a5.5 5.5 0 1 0 1.7-3.97" />
      <path d="M2.1 2.4v3.2h3.2" />
    </Glyph>
  );
}

/** Export: something leaves the page and lands on the reader's disk. */
export function ExportIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M8 1.9v8.2" />
      <path d="M5.1 7.3 8 10.2l2.9-2.9" />
      <path d="M2.4 11.4v1.3a1.4 1.4 0 0 0 1.4 1.4h8.4a1.4 1.4 0 0 0 1.4-1.4v-1.3" />
    </Glyph>
  );
}

/** A document: the text formats, CSV and JSON. */
export function DocumentIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M9 1.75H4.6a1.6 1.6 0 0 0-1.6 1.6v9.3a1.6 1.6 0 0 0 1.6 1.6h6.8a1.6 1.6 0 0 0 1.6-1.6V5.5z" />
      <path d="M9 1.75V5.5h4" />
      <path d="M5.75 8.75h4.5M5.75 11.25h3" />
    </Glyph>
  );
}

/** A grid of cells: the spreadsheet export, said as what it opens as rather
 *  than as one vendor's logo. */
export function SheetIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <rect x="2" y="2.75" width="12" height="10.5" rx="1.6" />
      <path d="M2 6.25h12M6.5 6.25v7" />
    </Glyph>
  );
}

/** Filtering a long list of column names down to the one being looked for. */
export function SearchIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <circle cx="7.1" cy="7.1" r="4.35" />
      <path d="M10.4 10.4 14 14" />
    </Glyph>
  );
}

/** Which columns are on screen. An eye rather than a gear: a gear says
 *  "configuration, somewhere else", and this menu is about what is in front of
 *  the reader right now. */
export function VisibilityIcon(p: TableIconProps) {
  return (
    <Glyph {...p}>
      <path d="M1.5 8s2.4-4.25 6.5-4.25S14.5 8 14.5 8s-2.4 4.25-6.5 4.25S1.5 8 1.5 8z" />
      <circle cx="8" cy="8" r="1.85" />
    </Glyph>
  );
}
