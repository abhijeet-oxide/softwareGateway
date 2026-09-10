import { useEffect, useState, type ReactNode } from "react";
import { Button, Popover } from "antd";
import {
  connection as defaultMonitor,
  useConnection,
  useCountdown,
  type ConnectionMonitor,
  type ConnectionSnapshot,
} from "../connection";
import { CheckCircleIcon, ChevronDownIcon, RefreshIcon, SignalIcon, SignalOffIcon } from "../icons";

// WHAT AN OUTAGE LOOKS LIKE, in every tool that copies this folder.
//
// Three surfaces, and the reason there are three rather than one is that an
// outage is not one moment. It is a second where something might be wrong, a
// minute where something is wrong and the work on screen must survive it, and
// a page that was trying to load while it happened.
//
//   ConnectionPill    the standing indicator, in the bar. Quiet when healthy,
//                     and the only thing on screen at all for a brief blip.
//   ConnectionAlert   the confirmed outage, as a card in a corner. Says what
//                     is happening, when it was last checked, when it will be
//                     checked again, and that nothing has been thrown away.
//   ConnectionNotice  the same fact inside a page that has nothing to show,
//                     in place of that page's own error state.
//
// # The rules they are built on
//
// **Nothing is taken away from anybody.** No modal, no full-page takeover, no
// disabled interface, no unmounted form. A person halfway through filling
// something in during a thirty-second restart must be able to keep typing and
// press save when it returns, and every one of those devices would have made
// that impossible. The alert is a card in the corner the page does not use,
// and it can be dismissed.
//
// **It says what it knows, including the time.** "Last connected 15:04:22"
// tells somebody how old the numbers they are reading are - which is the
// question stale data actually raises - and "Checking again in 8s" is what
// stops them pressing a retry button every two seconds. A spinner says
// neither.
//
// **The technical sentence is kept, and folded.** An operator wants to know
// that the address answered 502; a release manager wants to know that the
// screen is not lying to them. Putting the first in front of the second is how
// interfaces end up telling people to check that a service is reachable from a
// host they have never heard of, so it lives behind a disclosure.

/** How a phase reads to somebody who does not know what a service is. */
export interface ConnectionCopy {
  /** The pill's word. One word where possible: it sits in a bar. */
  label: string;
  /** The card's heading. */
  title: string;
  /** One sentence: what is happening, and what is being done about it. */
  sentence: string;
  tone: "ok" | "checking" | "warn" | "down";
}

const DEFAULT_SERVICE = "the service";

/** Capitalises a sentence that begins with a service name of unknown case. */
function opening(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/**
 * The whole of what an outage says, in one function.
 *
 * Here rather than inside the components because all three surfaces say the
 * same thing at different lengths, and a tool that reworded one of them would
 * be a tool where the pill and the card disagree about what is happening.
 */
export function describeConnection(
  snapshot: ConnectionSnapshot,
  service: string = DEFAULT_SERVICE,
): ConnectionCopy {
  const { phase, deviceOffline, restoredAt } = snapshot;

  if (deviceOffline) {
    return {
      label: "No network",
      title: "This device has no network connection",
      sentence: opening(
        `${service} cannot be reached until the network returns. The connection is checked automatically.`,
      ),
      tone: "warn",
    };
  }

  switch (phase) {
    case "online":
      return restoredAt
        ? {
            label: "Connected",
            title: "Connection restored",
            sentence: opening(`${service} is responding again.`),
            tone: "ok",
          }
        : {
            label: "Connected",
            title: "Connected",
            sentence: opening(`${service} is responding.`),
            tone: "ok",
          };
    case "unavailable":
      return {
        label: "Unavailable",
        title: opening(`${service} is temporarily unavailable`),
        sentence:
          "The service is not accepting requests, which is what a restart or a maintenance " +
          "window looks like. The connection is checked automatically.",
        tone: "warn",
      };
    case "offline":
      return {
        label: "Reconnecting",
        title: opening(`${service} stopped responding`),
        // Short, and it carries the two facts that decide what somebody does
        // next: this is usually nothing, and nobody has to sit and watch it.
        // The first draft spent three clauses on the mechanism - when the
        // connection dropped, that it is checked again, that the page is
        // restored - and a card in the corner of a screen is read in about a
        // second and a half.
        sentence: "This is usually temporary. The connection is checked automatically.",
        tone: "down",
      };
    case "unstable":
      return {
        label: "Checking",
        title: "Checking the connection",
        sentence: opening(`A request to ${service} did not complete. The connection is being checked.`),
        tone: "checking",
      };
    default:
      return {
        label: "Checking",
        title: "Checking the connection",
        sentence: opening(`${service} has not answered yet.`),
        tone: "checking",
      };
  }
}

/** The clock time something happened, which is what gets quoted into a ticket. */
function clockTime(at: number): string {
  return new Date(at).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/**
 * The same age, in as few characters as it can be said in.
 *
 * For the card's one meta line, which has to hold two facts and a countdown
 * inside a corner card: "25 seconds ago" and "11 seconds" spelled out wrap
 * that line onto three, and a footer three lines deep stops being a footer.
 * The unit is still stated - a bare number is a defect - just not in full.
 */
function compactAge(at: number, now: number): string {
  const seconds = Math.max(1, Math.floor((now - at) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h`;
}

/**
 * How long ago, in the largest unit that is still true.
 *
 * Rounded down and never below a second, because "0 seconds ago" reads as a
 * broken clock and "just now" is a conversation.
 */
function ageLabel(at: number, now: number): string {
  const seconds = Math.max(1, Math.floor((now - at) / 1000));
  if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"} ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} minute${minutes === 1 ? "" : "s"} ago`;
  const hours = Math.floor(minutes / 60);
  return `${hours} hour${hours === 1 ? "" : "s"} ago`;
}

/** Re-renders once a second while `active`, so an age on screen stays true. */
function useSecond(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/**
 * The ring that fills between one check and the next.
 *
 * A CSS animation given the interval as its duration rather than a value
 * redrawn every frame: the countdown is already stated in words beside it, and
 * this is the part that makes waiting legible at a glance. Keyed on the
 * deadline so each new interval starts it over.
 */
function CountdownRing({ until, from }: { until: number; from: number }) {
  const total = Math.max(1, (until - from) / 1000);
  // A NEGATIVE DELAY, which is the whole trick and was a visible bug without
  // it: a CSS animation starts at its first frame whenever the element mounts,
  // so a ring joining an interval that is already half spent drew a full
  // circle beside the words "checking again in 6s" and then took another
  // twenty seconds to empty. Offsetting the start by however much of the
  // interval has already passed puts the drawing and the number on the same
  // clock, wherever in the interval the surface happened to appear.
  const elapsed = Math.min(Math.max(0, (Date.now() - from) / 1000), total);
  return (
    <svg className="ui-conn-ring" viewBox="0 0 20 20" aria-hidden="true">
      <circle className="ui-conn-ring-track" cx="10" cy="10" r="8" />
      <circle
        key={until}
        className="ui-conn-ring-arc"
        cx="10"
        cy="10"
        r="8"
        style={{ animationDuration: `${total}s`, animationDelay: `-${elapsed}s` }}
      />
    </svg>
  );
}

function ToneGlyph({ tone }: { tone: ConnectionCopy["tone"] }) {
  if (tone === "ok") return <CheckCircleIcon />;
  if (tone === "down") return <SignalOffIcon />;
  return <SignalIcon />;
}

/**
 * The standing indicator, for the bar above the page.
 *
 * Healthy, it is a dot and nothing else. That is deliberate and it is the
 * whole reason it can be there permanently: a chip that says "Connected" in
 * words is a chip that spends the other 99.9% of the time telling somebody
 * something they already assumed, and an interface where everything is
 * shouting has nothing left to say when something is actually wrong. The dot
 * carries a tooltip and a panel for anybody who wants the detail.
 *
 * Unhealthy, it grows a word. The width transition is the point: movement in
 * the corner of the eye is what a person notices without being interrupted,
 * which is exactly the level of attention a two-second blip deserves.
 */
export function ConnectionPill({
  service,
  monitor = defaultMonitor,
  placement = "bottomRight",
}: {
  /** what to call the service in words, e.g. the product's own name */
  service?: string;
  monitor?: ConnectionMonitor;
  placement?: "bottom" | "bottomRight" | "bottomLeft";
}) {
  const snapshot = useConnection(monitor);
  const copy = describeConnection(snapshot, service);
  const [open, setOpen] = useState(false);
  const settled = copy.tone === "ok" && !snapshot.restoredAt;

  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      trigger="click"
      placement={placement}
      arrow={false}
      content={
        <ConnectionDetail snapshot={snapshot} service={service} monitor={monitor} live={open} />
      }
    >
      <button
        type="button"
        className={`ui-conn-pill tone-${copy.tone}${settled ? " is-quiet" : ""}`}
        aria-label={`Connection: ${copy.label}`}
        aria-expanded={open}
      >
        <span className="ui-conn-dot" />
        {!settled && <span className="ui-conn-pill-label">{copy.label}</span>}
      </button>
    </Popover>
  );
}

/**
 * The panel behind the pill: every fact the monitor holds, and the one control.
 *
 * `live` stops the clock when the panel is closed. An age that re-renders once
 * a second behind a closed popover is a timer running for nobody, on every
 * screen of the application, for the life of the session.
 */
export function ConnectionDetail({
  snapshot,
  service,
  monitor = defaultMonitor,
  live = true,
}: {
  snapshot: ConnectionSnapshot;
  service?: string;
  monitor?: ConnectionMonitor;
  live?: boolean;
}) {
  const copy = describeConnection(snapshot, service);
  const now = useSecond(live);
  const left = useCountdown(live ? snapshot.nextCheckAt : undefined);

  return (
    <div className={`ui-conn-detail tone-${copy.tone}`}>
      <div className="ui-conn-detail-head">
        <span className="ui-conn-detail-icon">
          <ToneGlyph tone={copy.tone} />
        </span>
        <span className="ui-conn-detail-title">{copy.title}</span>
      </div>
      <dl className="ui-conn-facts">
        <div>
          <dt>Last answer</dt>
          <dd>
            {snapshot.lastOkAt
              ? `${clockTime(snapshot.lastOkAt)} (${ageLabel(snapshot.lastOkAt, now)})`
              : "None in this session"}
          </dd>
        </div>
        <div>
          <dt>Last check</dt>
          <dd>{snapshot.lastCheckAt ? clockTime(snapshot.lastCheckAt) : "None"}</dd>
        </div>
        <div>
          <dt>Next check</dt>
          <dd>
            {snapshot.checking
              ? "Running"
              : left !== undefined
                ? `In ${left}s`
                : "When this tab is in front"}
          </dd>
        </div>
        {snapshot.failures > 0 && (
          <div>
            <dt>Failed checks</dt>
            <dd>{snapshot.failures}</dd>
          </div>
        )}
      </dl>
      {snapshot.detail && <p className="ui-conn-technical">{snapshot.detail}</p>}
      <Button
        size="small"
        block
        icon={<RefreshIcon />}
        loading={snapshot.checking}
        onClick={() => void monitor.check()}
      >
        Check now
      </Button>
    </div>
  );
}

/**
 * The confirmed outage, as a card in the corner.
 *
 * # Where it sits, and why that is not arbitrary
 *
 * Bottom LEFT. Toasts land bottom-right in both tools, and a card that shared
 * that corner would be buried under the first three notifications of an outage
 * that produces one per request. A form's own controls sit bottom-right or
 * under the fields; the bottom-left corner is the one part of a working screen
 * that is reliably empty, which is the entire requirement for something that
 * must be seen without being in the way.
 *
 * # What it will not do
 *
 * It will not appear for an unconfirmed failure - that is the pill's job, and
 * a card that flashed up for every dropped request would train people to
 * dismiss it before reading. It will not cover a modal's actions, it takes no
 * focus, and dismissing it dismisses it for that outage rather than for four
 * seconds.
 */
export function ConnectionAlert({
  service,
  monitor = defaultMonitor,
  /** an extra line the app owns: what is being preserved, and where */
  workNote,
}: {
  service?: string;
  monitor?: ConnectionMonitor;
  workNote?: ReactNode;
}) {
  const snapshot = useConnection(monitor);
  const copy = describeConnection(snapshot, service);
  const [dismissedAt, setDismissedAt] = useState<number | undefined>();
  const [showDetail, setShowDetail] = useState(false);

  const down = snapshot.phase === "offline" || snapshot.phase === "unavailable";
  const restored = Boolean(snapshot.restoredAt);
  // Dismissal is scoped to the outage it dismissed. `since` moves when the
  // phase changes, so a new outage - or the recovery from this one - is a new
  // thing to say and says it.
  const dismissed = dismissedAt !== undefined && dismissedAt === snapshot.since;
  const visible = (down || restored) && !dismissed;

  const now = useSecond(visible && down);
  const left = useCountdown(visible && down ? snapshot.nextCheckAt : undefined);

  useEffect(() => {
    if (!down) setShowDetail(false);
  }, [down]);

  if (!visible) return null;

  return (
    <div className={`ui-conn-alert tone-${copy.tone}`} role="status" aria-live="polite">
      <span className="ui-conn-alert-icon">
        <ToneGlyph tone={copy.tone} />
      </span>
      <div className="ui-conn-alert-title">{copy.title}</div>
      {/*
        ONE PARAGRAPH, not a sentence and then a tinted box under it.

        What is happening and what has been preserved are the same thought -
        "this is usually temporary, and nothing has been lost" - and splitting
        them across two blocks made the card twice as tall to say it, with the
        reassuring half in a panel of its own that read as a second, separate
        problem.
      */}
      <p className="ui-conn-alert-text">
        {copy.sentence}
        {down && workNote ? <> {workNote}</> : null}
      </p>

      {/*
        The diagnosis, as a section rather than as a control in the footer.

        It sits directly under the sentence it elaborates, where a disclosure
        belongs: the person opening it is following the message downwards, and
        a Details button beside Check now made two unrelated things - one that
        explains and one that acts - look like a pair of equal choices.
      */}
      {down && snapshot.detail && (
        <div className="ui-conn-alert-disclosure">
          <button
            type="button"
            className={`ui-conn-more${showDetail ? " is-open" : ""}`}
            aria-expanded={showDetail}
            onClick={() => setShowDetail((v) => !v)}
          >
            <ChevronDownIcon />
            Details
          </button>
          {showDetail && (
            <p className="ui-conn-technical">
              {snapshot.detail}
              {snapshot.status ? ` (HTTP ${snapshot.status})` : ""}
              {snapshot.lastOkAt ? ` Last connected at ${clockTime(snapshot.lastOkAt)}.` : ""}
            </p>
          )}
        </div>
      )}

      {/*
        THE FOOTER: what is known on the left, the one action on the right.

        Right, because that is where a dialog's confirming button is in every
        application on the platform and where a right hand already is. It had
        been on the left, in front of the facts, which put the button somebody
        does not need to press in front of the line that tells them not to
        bother pressing it.

        The two facts share a line for the same reason the paragraph is one
        paragraph: they are one thought - how old this screen is, and how long
        until it is checked again - and stacking them made a two-line footer
        out of nine words.
      */}
      {down && (
        <div className="ui-conn-alert-foot">
          <span className="ui-conn-alert-meta">
            {snapshot.lastOkAt && (
              <>
                Last connected {compactAge(snapshot.lastOkAt, now)} ago
                <span className="ui-conn-sep">·</span>
              </>
            )}
            {snapshot.checking ? (
              "Checking now"
            ) : left !== undefined ? (
              <>
                {snapshot.nextCheckAt !== undefined && snapshot.lastCheckAt !== undefined && (
                  <CountdownRing until={snapshot.nextCheckAt} from={snapshot.lastCheckAt} />
                )}
                Checking again in {left}s
              </>
            ) : snapshot.deviceOffline ? (
              "Waiting for the network"
            ) : (
              "Checking when this tab is in front"
            )}
          </span>
          <div className="ui-conn-alert-actions">
            <Button
              size="small"
              type="primary"
              icon={<RefreshIcon />}
              loading={snapshot.checking}
              onClick={() => void monitor.check()}
            >
              Check now
            </Button>
          </div>
        </div>
      )}

      <button
        type="button"
        className="ui-conn-alert-close"
        aria-label="Dismiss"
        onClick={() => setDismissedAt(snapshot.since)}
      >
        <svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true">
          <path
            d="m4.4 4.4 7.2 7.2M11.6 4.4l-7.2 7.2"
            stroke="currentColor"
            strokeWidth="1.6"
            strokeLinecap="round"
          />
        </svg>
      </button>
    </div>
  );
}

/**
 * The same fact, inside a page that has nothing to show.
 *
 * A first load that failed because the service was away is NOT an error and
 * must not be dressed as one: there is nothing wrong with the request, nothing
 * for the reader to correct, and a red alert with a Try again button asks them
 * to do by hand the thing that is already happening on a timer. This states
 * the position, counts down to the next check, and gets out of the way when
 * the data arrives.
 */
export function ConnectionNotice({
  service,
  monitor = defaultMonitor,
}: {
  service?: string;
  monitor?: ConnectionMonitor;
}) {
  const snapshot = useConnection(monitor);
  const copy = describeConnection(snapshot, service);
  const left = useCountdown(snapshot.nextCheckAt);

  return (
    <div className={`ui-conn-inline tone-${copy.tone}`} role="status" aria-live="polite">
      <span className="ui-conn-inline-icon">
        <ToneGlyph tone={copy.tone} />
      </span>
      <div className="ui-conn-inline-main">
        <div className="ui-conn-inline-title">{copy.title}</div>
        <p className="ui-conn-inline-text">{copy.sentence}</p>
      </div>
      <div className="ui-conn-inline-side">
        <Button
          size="small"
          icon={<RefreshIcon />}
          loading={snapshot.checking}
          onClick={() => void monitor.check()}
        >
          Check now
        </Button>
        <span className="ui-conn-inline-next">
          {snapshot.checking
            ? "Checking"
            : left !== undefined
              ? `Checking again in ${left}s`
              : "Checking when this tab is in front"}
        </span>
      </div>
    </div>
  );
}
