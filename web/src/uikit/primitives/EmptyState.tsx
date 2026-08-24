import type { ReactNode } from "react";
import { Button } from "antd";

// EmptyState is what a screen shows when there is nothing to show yet.
//
// A missing precondition is a STATE, not an error: nothing recorded yet, no
// instances found, no application connected. It gets an icon in a soft pressed
// well, one line saying what this is, one line saying what to do next, and at
// most one action - never a red banner, which would say something went wrong
// when nothing did.
export default function EmptyState({
  icon,
  art,
  title,
  hint,
  actionLabel,
  onAction,
  children,
}: {
  icon?: ReactNode;
  /** a full illustration; replaces the icon well */
  art?: ReactNode;
  title: ReactNode;
  hint?: ReactNode;
  actionLabel?: string;
  onAction?: () => void;
  children?: ReactNode;
}) {
  return (
    <div className="ui-empty">
      {art}
      {!art && icon && <div className="ui-empty-icon">{icon}</div>}
      <div className="ui-empty-title">{title}</div>
      {hint && <div className="ui-empty-hint">{hint}</div>}
      {actionLabel && onAction && (
        <Button type="primary" size="small" onClick={onAction} style={{ marginTop: 8 }}>
          {actionLabel}
        </Button>
      )}
      {children}
    </div>
  );
}
