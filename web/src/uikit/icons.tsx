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
