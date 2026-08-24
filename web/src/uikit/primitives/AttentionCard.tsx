import type { ReactNode } from "react";
import { Button } from "antd";
import { CheckCircleIcon, ErrorCircleIcon, InfoIcon, WarningIcon } from "../icons";

// AttentionCard is one "needs your attention" row: a severity icon, a title
// with a supporting line, and a right-aligned action.
//
// It sits INSIDE a SectionCard, so it reads as a flat hairline-bordered row
// rather than another floating surface. A list of these stacked inside one
// card is the shape of every "what is waiting on me" panel in both tools.
export type AttentionSeverity = "warn" | "danger" | "ok" | "info";

const ICON: Record<AttentionSeverity, ReactNode> = {
  warn: <WarningIcon />,
  danger: <ErrorCircleIcon />,
  ok: <CheckCircleIcon />,
  info: <InfoIcon />,
};

export default function AttentionCard({
  severity,
  title,
  sub,
  actionLabel,
  onAction,
  primary = false,
  extra,
}: {
  severity: AttentionSeverity;
  title: ReactNode;
  sub?: ReactNode;
  actionLabel?: string;
  onAction?: () => void;
  /** render the action as the primary (brand) button */
  primary?: boolean;
  extra?: ReactNode;
}) {
  return (
    <div className={`ui-attention sev-${severity}`}>
      <span className="ui-attention-icon">{ICON[severity]}</span>
      <div className="ui-attention-main">
        <div className="ui-attention-title">{title}</div>
        {sub && <div className="ui-attention-sub">{sub}</div>}
      </div>
      {extra}
      {actionLabel && onAction && (
        <Button size="small" type={primary ? "primary" : "default"} onClick={onAction}>
          {actionLabel}
        </Button>
      )}
    </div>
  );
}
