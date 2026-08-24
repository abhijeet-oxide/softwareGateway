import type { ReactNode } from "react";
import { CheckCircleIcon, ErrorCircleIcon, InfoIcon, WarningIcon } from "../icons";

// InlineNotice is the design system's one-line message: an icon, a sentence,
// and (optionally) one action, on a tinted strip the height of a control.
//
// It exists because most of what an app has to say is one sentence. A full
// alert block - title line, description paragraph, generous padding - spends a
// whole band of the screen on it and reads as an interruption, which is wrong
// for something like "detected layout: kustomize". Reserve the full alert for
// the rare message that genuinely needs a paragraph; everything else is this.

export type NoticeTone = "info" | "ok" | "warn" | "danger" | "neutral";

const DEFAULT_ICON: Record<NoticeTone, ReactNode> = {
  info: <InfoIcon />,
  ok: <CheckCircleIcon />,
  warn: <WarningIcon />,
  danger: <ErrorCircleIcon />,
  neutral: <InfoIcon />,
};

export default function InlineNotice({
  tone = "info",
  icon,
  children,
  action,
  className = "",
}: {
  tone?: NoticeTone;
  /** override the tone's icon; pass null for no icon at all */
  icon?: ReactNode | null;
  children: ReactNode;
  /** one trailing control (a link button, "Try again"), kept on the same line */
  action?: ReactNode;
  className?: string;
}) {
  const glyph = icon === null ? null : (icon ?? DEFAULT_ICON[tone]);
  return (
    <div
      className={`ui-notice tone-${tone}${className ? ` ${className}` : ""}`}
      // A warning or a failure is announced; an informational line is not, or
      // every page that says which layout it detected would interrupt a screen
      // reader to say it.
      role={tone === "danger" || tone === "warn" ? "alert" : undefined}
    >
      {glyph && <span className="ui-notice-glyph">{glyph}</span>}
      <span className="ui-notice-body">{children}</span>
      {action && <span className="ui-notice-action">{action}</span>}
    </div>
  );
}
