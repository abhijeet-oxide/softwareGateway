import type { CSSProperties, ReactNode } from "react";

// SectionCard is the standard content surface: a soft dual shadow, no hard
// border, an optional title row and a right-side action ("View all").
//
// Use grouping and whitespace first and reach for a card only when the
// composition calls for one. A page of six identical white rectangles has no
// order in it: the summary that should be read first and the table it
// summarises sit at exactly the same distance from the reader.
export default function SectionCard({
  title,
  extra,
  children,
  style,
  bodyStyle,
  padded = true,
  className,
}: {
  title?: ReactNode;
  extra?: ReactNode;
  children: ReactNode;
  style?: CSSProperties;
  bodyStyle?: CSSProperties;
  padded?: boolean;
  className?: string;
}) {
  const head = Boolean(title || extra);
  const body = padded ? `is-padded${head ? " has-head" : ""}` : "is-flush";
  return (
    <div className={`ui-card${className ? ` ${className}` : ""}`} style={style}>
      {head && (
        <div className="ui-card-head">
          <span className="ui-card-head-title">{title}</span>
          {extra && <span className="ui-card-head-extra">{extra}</span>}
        </div>
      )}
      <div className={`ui-card-body ${body}`} style={bodyStyle}>
        {children}
      </div>
    </div>
  );
}
