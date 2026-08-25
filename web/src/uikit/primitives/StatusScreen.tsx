import type { ReactNode } from "react";
import { Button } from "antd";
import type { BrandIdentity } from "../brand";
import { BrandLockup, BrandMark } from "./BrandLockup";

// THE FULL-PAGE STATES, shared whole.
//
// Every tool has the same three moments before it can show anything: it is
// checking whether its service is there, the service did not answer, or nobody
// is signed in. They are the FIRST thing a person ever sees of a product and
// the thing they see on its worst day, so they are exactly the wrong place for
// each tool to improvise its own layout.
//
// A missing precondition is a STATE, not an error: it says what happened in
// plain words, keeps trying where trying helps, and offers the one action that
// fixes it. No stack traces, no dead ends, and nothing that looks broken.

/**
 * The in-between, while a probe is in flight: the product's own mark on its own
 * canvas, so the first paint already belongs to the app rather than being a
 * blank page.
 */
export function BootSplash({
  brand,
  label = "Starting",
}: {
  brand: BrandIdentity;
  label?: string;
}) {
  return (
    <div className="ui-status-screen">
      <div className="ui-boot-splash">
        <BrandMark brand={brand} size={44} />
        <span className="ui-boot-name">{brand.appName}</span>
        <span className="ui-boot-progress" aria-label={label} role="progressbar" />
      </div>
    </div>
  );
}

export interface StatusAction {
  label: string;
  onClick?: () => void;
  href?: string;
  primary?: boolean;
  icon?: ReactNode;
  loading?: boolean;
}

/**
 * One full-page state: the lockup, an illustration, a sentence, and the way
 * out.
 *
 * The lockup sits at the card's top-left rather than centred with everything
 * else. It is the one part of the card that is not the message - it says which
 * product is speaking - and centring it made the card read as a dialog from
 * nowhere in particular.
 */
export function StatusScreen({
  brand,
  art,
  title,
  children,
  actions,
  note,
  captionText,
}: {
  brand: BrandIdentity;
  /** the illustration; omit for a plain card */
  art?: ReactNode;
  title: ReactNode;
  /** the explanation, in plain words */
  children?: ReactNode;
  actions?: StatusAction[];
  /** a quiet line under the actions: a retry countdown, an address */
  note?: ReactNode;
  captionText?: string;
}) {
  return (
    <div className="ui-status-screen">
      <div className="ui-status-card">
        <BrandLockup brand={brand} size={30} captionText={captionText} />
        {art && <div className="ui-status-art">{art}</div>}
        <h1 className="ui-status-title">{title}</h1>
        {children && <div className="ui-status-body">{children}</div>}
        {actions && actions.length > 0 && (
          <div className="ui-status-actions">
            {actions.map((a) => (
              <Button
                key={a.label}
                type={a.primary ? "primary" : "default"}
                icon={a.icon}
                loading={a.loading}
                href={a.href}
                onClick={a.onClick}
              >
                {a.label}
              </Button>
            ))}
          </div>
        )}
        {note && <div className="ui-status-note">{note}</div>}
      </div>
    </div>
  );
}
