import type { ReactNode } from "react";

/**
 * The name of a zone, a figure or a group of fields.
 *
 * ONE component, because there were five hand-rolled copies of the same four
 * declarations across two screens, and they had already started to disagree
 * about the letter spacing.
 *
 * It is SENTENCE CASE, and that is the point of extracting it. Every copy set
 * `textTransform: uppercase` with tracking opened up to compensate, which is
 * the single most dated pattern a dashboard can wear: it shouts, it costs
 * roughly a third more width than the words need, and at 11px it is measurably
 * slower to read than the same words in the case they are written in. What
 * makes a label recede is WEIGHT and COLOUR, which is what this uses.
 */
export default function FieldLabel({
  children,
  /** a figure that belongs to the label rather than under it */
  count,
  /** the mark that leads the label - a direction arrow, a status glyph */
  icon,
  as: Tag = "div",
}: {
  children: ReactNode;
  count?: ReactNode;
  icon?: ReactNode;
  as?: "div" | "span";
}) {
  return (
    <Tag className="ui-field-label">
      {icon && <span className="ui-field-label-icon">{icon}</span>}
      <span className="ui-field-label-text">{children}</span>
      {count !== undefined && <span className="ui-field-label-count ui-num">{count}</span>}
    </Tag>
  );
}
