import type { CSSProperties, ReactNode } from "react";

// Identifiers - versions, digests, paths, URLs, parameter names - are monospace
// EVERYWHERE they appear, with tabular figures so a column of them lines up.
// One component rather than a class each page remembers to apply, because the
// page that forgets is the one where a digest wraps mid-hash.
export default function Mono({
  children,
  style,
  title,
  className,
}: {
  children: ReactNode;
  style?: CSSProperties;
  title?: string;
  className?: string;
}) {
  return (
    <span className={`ui-mono${className ? ` ${className}` : ""}`} style={style} title={title}>
      {children}
    </span>
  );
}
