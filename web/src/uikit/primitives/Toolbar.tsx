import type { CSSProperties, ReactNode } from "react";

// Toolbar is the compact control strip above a working surface: one 40px row,
// 8px gaps, a bottom hairline. Left content leads, right content trails.
export default function Toolbar({
  left,
  right,
  style,
  border = true,
}: {
  left?: ReactNode;
  right?: ReactNode;
  style?: CSSProperties;
  border?: boolean;
}) {
  return (
    <div className={`ui-toolbar${border ? " has-border" : ""}`} style={style}>
      <div className="ui-toolbar-left">{left}</div>
      {right && <div className="ui-toolbar-right">{right}</div>}
    </div>
  );
}
