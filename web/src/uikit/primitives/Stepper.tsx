import type { ReactNode } from "react";
import { Tooltip } from "antd";
import { CheckIcon } from "../icons";

// Stepper is the one progress indicator for every multi-step flow (import,
// onboarding, a new application) AND for a review lifecycle - one component so
// the steppers can never diverge.
//
// A single clean row: a numbered node per step with a one-line label, joined by
// a connector that fills with the brand colour as the reader advances.
// Completed steps collapse to a check; the current step carries a soft brand
// ring. Labels stay single-line so they never wrap into the cramped two-row
// look of a raw component stepper.

export interface StepDef {
  /** short, single-word-ish label */
  label: string;
  /** optional glyph shown on upcoming/active nodes instead of the number */
  icon?: ReactNode;
  /** optional tooltip explaining the step */
  explain?: ReactNode;
  /** render this step in the danger tone (e.g. a rejected change) */
  error?: boolean;
}

export default function Stepper({
  steps,
  current,
  className,
  compact,
  ariaLabel = "Progress",
}: {
  steps: StepDef[];
  /** zero-based index of the active step */
  current: number;
  className?: string;
  /** A one-line strip instead of the full row: small nodes, small labels, left
   *  aligned and no wider than it needs. For places where the lifecycle is
   *  CONTEXT beside the real content (an opened change, a running transfer)
   *  rather than the SUBJECT of the screen (a wizard), where the full-size row
   *  was just a band of empty space above the thing the reader came for. */
  compact?: boolean;
  ariaLabel?: string;
}) {
  if (compact) {
    return (
      <div className="ui-steps-mini" role="list" aria-label={ariaLabel}>
        {steps.map((s, i) => {
          const done = i < current;
          const active = i === current;
          const state = s.error ? "is-err" : done ? "is-done" : active ? "is-now" : "is-todo";
          return (
            <span key={s.label} className="ui-steps-mini-group" role="listitem">
              {i > 0 && (
                <i className={`ui-steps-mini-bar${done || active ? " is-filled" : ""}`} aria-hidden />
              )}
              <Tooltip title={s.explain}>
                <span className={`ui-steps-mini-step ${state}`}>
                  <i className="ui-steps-mini-dot" aria-hidden>
                    {done && !s.error ? <CheckIcon /> : null}
                  </i>
                  {s.label}
                </span>
              </Tooltip>
            </span>
          );
        })}
      </div>
    );
  }
  return (
    <div className={`ui-steps${className ? ` ${className}` : ""}`} role="list" aria-label={ariaLabel}>
      {steps.map((s, i) => {
        const done = i < current;
        const active = i === current;
        const last = i === steps.length - 1;
        const state =
          `${done ? " is-done" : ""}${active ? " is-now" : ""}${s.error ? " is-err" : ""}` +
          `${last ? " is-last" : ""}`;
        const node = (
          <div className="ui-steps-node">
            {done && !s.error ? <CheckIcon /> : (s.icon ?? i + 1)}
          </div>
        );
        return (
          <div key={s.label} className={`ui-steps-step${state}`} role="listitem">
            <div className="ui-steps-col">
              {s.explain ? <Tooltip title={s.explain}>{node}</Tooltip> : node}
              <span className="ui-steps-label" title={s.label}>
                {s.label}
              </span>
            </div>
            {!last && (
              <div className="ui-steps-bar" aria-hidden>
                <div className="ui-steps-fill" />
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
