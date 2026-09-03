import type { CSSProperties, ReactNode } from "react";
import { Button } from "antd";
import type { BrandIdentity } from "../brand";
import { BrandLockup } from "./BrandLockup";
import { ErrorArt, LoadingArt, MaintenanceArt, NotFoundArt } from "./illustrations";
import type { StatusAction } from "./StatusScreen";

// THE FOUR PAGES A TOOL SHOWS INSTEAD OF ITSELF.
//
// Planned work, a wrong address, a failure nobody planned for, and the wait
// before a screen can be drawn. Every tool on the platform has all four, and
// they are the screens most likely to be improvised one at a time - a `Result`
// here, a bare spinner there, a stack trace on the third - which is exactly how
// two products that share a palette stop looking like one product.
//
// So they are ONE component with four faces. What differs between them is the
// drawing, the label above the title and the sentence; the frame, the spacing,
// the order of the parts and the way the actions sit is identical, because a
// person meeting two of these in one session should not be able to tell that
// they were written separately.
//
// # Where they render
//
// Each takes `full`. Without it the page fills whatever it was given - the
// content area inside an application shell, where the navigation is still on
// screen and still works, which is the right frame for a wrong address or a
// page that failed to load. With it the page takes the viewport and states the
// product's identity itself, for the moments when there is no shell to sit in:
// before the application has booted, or when the whole of it has fallen over.
//
// # What they never do
//
// They do not blame, apologise or narrate. A state screen says what the
// condition is, what it means for the work in hand, and the one action that
// resolves it. Nothing here is a dead end: every face carries a way onward,
// even if that way is only reloading.

export interface StatePageProps {
  /** the identity, drawn only when this page owns the whole viewport */
  brand?: BrandIdentity;
  /** the drawing; each face supplies its own, and callers rarely pass this */
  art?: ReactNode;
  /**
   * The small line above the title: a status code, or the name of the
   * condition. Upper case and tracked out, so it reads as a machine's word for
   * what happened rather than as a heading of its own.
   */
  code?: ReactNode;
  title: ReactNode;
  /** the explanation, in plain words */
  children?: ReactNode;
  /**
   * The one fact the page is ABOUT, set apart: the address that matched
   * nothing, the window that has not ended yet. It sits between the sentence
   * and the actions rather than under a fold, because it is the first thing
   * the reader checks and folding it away costs a click on every visit.
   */
  subject?: ReactNode;
  actions?: StatusAction[];
  /** a quiet line under the actions: a retry countdown, a time */
  note?: ReactNode;
  /**
   * The technical account, folded away: a message, an identifier, a path.
   * Present because these pages get quoted into tickets, and a screen that
   * hides what actually failed turns one report into three.
   */
  details?: ReactNode;
  /** take the viewport rather than filling the container */
  full?: boolean;
  className?: string;
  style?: CSSProperties;
}

/**
 * The frame all four faces share.
 *
 * Exported because a tool occasionally has a fifth state of its own - a region
 * it cannot serve, a licence that has lapsed - and one more hand-built centred
 * column is how the family starts to drift.
 */
export function StatePage({
  brand,
  art,
  code,
  title,
  children,
  subject,
  actions,
  note,
  details,
  full = false,
  className,
  style,
}: StatePageProps) {
  return (
    <div
      className={`ui-state-page${full ? " is-full" : ""}${className ? ` ${className}` : ""}`}
      style={style}
    >
      {/* Only when this page IS the window: inside a shell the navigation is
          already saying which product is speaking, and a second lockup under it
          reads as a page from somewhere else. */}
      {full && brand && <BrandLockup brand={brand} size={30} style={{ marginBottom: 4 }} />}
      <div className="ui-state-body">
        {art && <div className="ui-state-art">{art}</div>}
        {code && <div className="ui-state-code">{code}</div>}
        <h1 className="ui-state-title">{title}</h1>
        {children && <p className="ui-state-text">{children}</p>}
        {subject && <div className="ui-state-subject">{subject}</div>}
        {actions && actions.length > 0 && (
          <div className="ui-state-actions">
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
        {note && <div className="ui-state-note">{note}</div>}
        {details && (
          <details className="ui-state-details">
            <summary>Technical details</summary>
            <div className="ui-state-details-body">{details}</div>
          </details>
        )}
      </div>
    </div>
  );
}

/**
 * Planned work is in progress and the service is deliberately not answering.
 *
 * The distinction from an outage is the entire message, and it is carried by
 * the drawing as much as by the words: gears turning, not an alert. Nothing on
 * this page suggests anybody did anything wrong, because nobody did.
 */
export function MaintenancePage({
  title = "Under maintenance",
  children = "Planned work is in progress. This service is unavailable until it completes; no data is affected while it runs.",
  art,
  code = "Maintenance",
  ...rest
}: Omit<StatePageProps, "title"> & { title?: ReactNode }) {
  return (
    <StatePage art={art ?? <MaintenanceArt size={140} />} code={code} title={title} {...rest}>
      {children}
    </StatePage>
  );
}

/**
 * The address does not name anything this application serves.
 *
 * `path` is shown because these arrive from a bookmark, a chat thread or a
 * ticket, and the reader's next move is almost always to check the address
 * against the one they were given - which they cannot do if the page will not
 * say what it received.
 */
export function NotFoundPage({
  title = "Page not found",
  children = "This address does not match any page in this application. It may have been renamed, or the link that led here may be incomplete.",
  path,
  art,
  code = "404",
  subject,
  ...rest
}: Omit<StatePageProps, "title"> & { title?: ReactNode; path?: string }) {
  return (
    <StatePage
      art={art ?? <NotFoundArt size={140} />}
      code={code}
      title={title}
      subject={subject ?? (path ? <span className="ui-mono">{path}</span> : undefined)}
      {...rest}
    >
      {children}
    </StatePage>
  );
}

/**
 * Something failed that no state in this application accounts for.
 *
 * The message and the request identifier are shown - folded away, but shown.
 * An error screen that says only "something went wrong" is a screen that costs
 * a support round trip to get past, and the person reading it is usually the
 * one who would have fixed it from the message alone.
 */
export function ErrorPage({
  title = "Unexpected error",
  children = "This page stopped before it finished. The work behind it is unaffected, and reloading starts it again.",
  detail,
  requestId,
  art,
  code,
  details,
  ...rest
}: Omit<StatePageProps, "title"> & {
  title?: ReactNode;
  /** what actually failed, in the words the failure used */
  detail?: ReactNode;
  /** the identifier a support request should quote */
  requestId?: string;
}) {
  const body =
    details ??
    (detail || requestId ? (
      <>
        {detail && <div className="ui-mono ui-state-detail-line">{detail}</div>}
        {requestId && <div className="ui-state-detail-meta">Request {requestId}</div>}
      </>
    ) : undefined);
  return (
    <StatePage art={art ?? <ErrorArt size={140} />} code={code} title={title} details={body} {...rest}>
      {children}
    </StatePage>
  );
}

/**
 * The wait before a screen can be drawn.
 *
 * `label` names the work rather than describing the sensation of waiting, so
 * it is worth reading: "Loading this page", "Checking the service". It is
 * announced politely, which is the only way somebody on a screen reader learns
 * that anything is happening at all.
 *
 * Deliberately indeterminate: nothing on it fills or advances, because a page
 * whose code is in flight has no denominator and a bar that filled would be
 * stating a position it does not have. A caller that DOES know its progress
 * wants a progress bar, not this.
 */
export function LoadingPage({
  label = "Loading",
  art,
  brand,
  full = false,
  className,
  style,
}: {
  /** what is being waited for, named */
  label?: string;
  art?: ReactNode;
  brand?: BrandIdentity;
  full?: boolean;
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <div
      className={`ui-state-page is-loading${full ? " is-full" : ""}${className ? ` ${className}` : ""}`}
      style={style}
    >
      {full && brand && <BrandLockup brand={brand} size={30} style={{ marginBottom: 4 }} />}
      <div className="ui-state-body">
        <div className="ui-state-art">{art ?? <LoadingArt size={132} />}</div>
        <div className="ui-state-loading-label" role="status" aria-live="polite">
          {label}
        </div>
      </div>
    </div>
  );
}
