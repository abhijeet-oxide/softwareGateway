import type { CSSProperties, ReactNode } from "react";
import type { Severity, Verdict } from "../color";
import { verdict as verdictColour } from "../color";

// Severity, said in three channels at once.
//
// COLOUR IS NEVER THE ONLY SIGNAL. Every severity here is a hue AND a dot
// whose fill differs (critical and high solid, medium and low ringed, unknown
// dashed) AND a word spelled out beside it. That is what lets a deployment
// retheme the scale freely without making a page unreadable, and it is the
// whole test of whether the reinforcement is real: read the page in greyscale
// and nothing is lost.

const LABEL: Record<Severity, string> = {
  critical: "Critical",
  high: "High",
  medium: "Medium",
  low: "Low",
  unknown: "Unknown",
};

export function SeverityTag({
  value,
  count,
  label,
  style,
}: {
  value: Severity;
  /** a number to read after the word; omitted when there is nothing to count */
  count?: number;
  /** override the written word (the scale's own name is the default) */
  label?: ReactNode;
  style?: CSSProperties;
}) {
  return (
    <span className={`ui-sev sev-${value}`} style={style}>
      <span className="ui-sev-dot" />
      <span className="ui-sev-label">
        {label ?? LABEL[value]}
        {count === undefined ? null : ` ${count}`}
      </span>
    </span>
  );
}

/** Just the dot, for a table cell that already carries the word. */
export function SeverityDot({ value, style }: { value: Severity; style?: CSSProperties }) {
  return (
    <span className={`ui-sev sev-${value}`} style={style} title={LABEL[value]}>
      <span className="ui-sev-dot" />
    </span>
  );
}

const VERDICT_LABEL: Record<Verdict, string> = {
  better: "Better",
  worse: "Worse",
  unchanged: "Unchanged",
  inconclusive: "Inconclusive",
};

/** A comparison's verdict, in the same three channels. */
export function VerdictTag({ value, label }: { value: Verdict; label?: ReactNode }) {
  return (
    <span
      className="ui-sev"
      style={{ color: verdictColour[value] }}
      title={VERDICT_LABEL[value]}
    >
      <span
        className="ui-sev-dot"
        style={{
          background: value === "unchanged" ? "transparent" : "currentColor",
          borderStyle: value === "inconclusive" ? "dashed" : "solid",
        }}
      />
      <span className="ui-sev-label">{label ?? VERDICT_LABEL[value]}</span>
    </span>
  );
}
