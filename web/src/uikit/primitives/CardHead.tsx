import type { ReactNode } from "react";

/** The tint behind a card's mark. Status tones are for panels ABOUT state. */
export type CardIconTone = "brand" | "ok" | "review" | "pending" | "danger" | "neutral";

/**
 * A card's title, as a mark and a name.
 *
 * # Why a card needs more than a bold line
 *
 * Because a panel that reports a live subject has to say three things before
 * the reader reaches a number: what it is, what state it is in, and over what
 * period. A plain title can carry one of them. Crammed into one line they read
 * as a sentence nobody parses at a glance - and the period, which is a
 * control, ends up looking like part of the name.
 *
 * So the grammar is fixed: a tinted mark, the name (with an optional second
 * line under it), whatever states the subject's condition beside it, and the
 * controls at the far right, where a reader's eye goes for them. Three panels
 * built this way read as one system rather than three cards that happen to be
 * adjacent.
 *
 * Pass the result as `SectionCard`'s `title`, with `extra` for the controls.
 */
export default function CardHead({
  icon,
  tone = "brand",
  title,
  sub,
  status,
}: {
  icon: ReactNode;
  tone?: CardIconTone;
  title: ReactNode;
  /** A second line under the name: what the panel is counting, or over what. */
  sub?: ReactNode;
  /** The subject's state, stated beside its name - a pill, usually. */
  status?: ReactNode;
}) {
  return (
    <>
      <span className={`ui-card-icon tone-${tone}`} aria-hidden>
        {icon}
      </span>
      <span className="ui-card-head-name">
        <span className="ui-card-head-line">
          <span className="ui-card-head-text">{title}</span>
          {status}
        </span>
        {sub && <span className="ui-card-head-sub">{sub}</span>}
      </span>
    </>
  );
}

/**
 * One figure in a row of them.
 *
 * The label is above the value rather than beside it, because these are read
 * as a row: eyes travel across the values and drop to a label only for the one
 * that surprised them.
 */
export function Figure({ label, value }: { label: ReactNode; value: ReactNode }) {
  return (
    <div className="ui-figure">
      <div className="ui-figure-label">{label}</div>
      <div className="ui-figure-value">{value}</div>
    </div>
  );
}
