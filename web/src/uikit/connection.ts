import { useEffect, useRef, useState, useSyncExternalStore } from "react";

// IS THE SERVICE THERE? - asked once, for the whole application.
//
// # The failure this replaces
//
// An application whose service goes away has no shortage of evidence: every
// read fails, every save fails, every poll fails. What it lacks is a PLACE to
// put that evidence, so each failure is reported on its own - one toast per
// request, stacking up the side of the window, each one saying the same thing
// in the same words about the same single cause. A person looking at twelve of
// them learns nothing they did not know from the first, cannot tell whether the
// count means twelve problems or one, and has no idea whether the thing is
// still down NOW or was down eight seconds ago.
//
// Worse is what happens next: a screen somewhere decides the outage is
// permanent and replaces itself with an error page, taking the work on it with
// it. Almost every outage a browser sees is transient - a rolling restart, a
// pod moving, a laptop's wifi - and the correct behaviour for all of them is to
// wait a few seconds and carry on.
//
// So reachability is ONE fact with ONE owner. Everything that talks to the
// service reports what happened to it; it decides when that adds up to an
// outage, keeps checking on a schedule of its own, and says so in exactly one
// place on screen. Nothing is unmounted, nothing is thrown away, and a blip
// that resolves in four seconds is never mentioned at all.
//
// # What is deliberately NOT here
//
// No product name, no endpoint, no HTTP. This folder is copied between tools
// (see README.md), and the address of a service is exactly the kind of thing
// two tools do not share. The application hands in a `probe` and this file
// decides when to call it.

/**
 * What the connection is, as far as anybody may act on it.
 *
 * `unstable` is the state that makes this worth having. It is a failure that
 * has NOT yet been confirmed: one request came back wrong, which happens on a
 * healthy system several times a day. Saying "offline" then would make the
 * interface cry wolf; saying nothing at all would leave the reader watching a
 * dead screen. So it is a state with a quiet face and a probe already in
 * flight, and it usually ends within a second or two having said almost
 * nothing.
 */
export type ConnectionPhase =
  /** Nothing has been asked yet. */
  | "starting"
  /** The service answered. */
  | "online"
  /** Something failed once and a check is deciding whether it meant anything. */
  | "unstable"
  /** Confirmed: the service is not answering. */
  | "offline"
  /** The service answered and refused: a restart, a planned window, a probe it failed. */
  | "unavailable";

/** What a probe found. */
export type ProbeVerdict =
  /** It answered as itself. */
  | "ok"
  /** Nothing answered: DNS, a refused connection, a timeout, a dead proxy. */
  | "unreachable"
  /** It answered, and answered that it is not serving right now (503). */
  | "unavailable";

export interface ProbeResult {
  verdict: ProbeVerdict;
  /**
   * The technical sentence, for the disclosure and for a ticket. Never the
   * headline: the headline is written from the phase, in words that assume
   * nothing about who is reading.
   */
  detail?: string;
  /** The status it answered with, when it answered at all. */
  status?: number;
}

export type Probe = (signal: AbortSignal) => Promise<ProbeResult>;

export interface ConnectionSnapshot {
  phase: ConnectionPhase;
  /** A check is in flight right now. */
  checking: boolean;
  /** The device itself has no network, which is a different sentence. */
  deviceOffline: boolean;
  /** Consecutive failed checks. Drives the backoff, and is worth showing. */
  failures: number;
  /** When the service last answered. The age of everything on screen. */
  lastOkAt?: number;
  /** When the last check was made, answered or not. */
  lastCheckAt?: number;
  /** When the next automatic check is due. */
  nextCheckAt?: number;
  /** When the current phase began. */
  since: number;
  /** Set for a few seconds after a recovery, so a surface can say so and go. */
  restoredAt?: number;
  /** The last technical sentence, for the disclosure. */
  detail?: string;
  /** The last status the service answered with. */
  status?: number;
  /**
   * How many screens have declared unsaved work (see `holdWork`).
   *
   * Part of the snapshot rather than a separate getter because it is read by a
   * component, and a fact a component renders that is not in the snapshot is a
   * fact that changes without a re-render - which is a note about preserved
   * work appearing several seconds after the work was declared, or not at all.
   */
  heldWork: number;
}

export interface ConnectionOptions {
  probe?: Probe;
  /**
   * How long after an unconfirmed failure the confirming check runs.
   *
   * Short, because this is the delay before the interface may say anything at
   * all, and long enough that a single dropped request on a busy connection
   * resolves silently.
   */
  confirmDelay?: number;
  /**
   * The wait before each subsequent check, indexed by consecutive failure.
   *
   * It climbs and then stops climbing. A service that has been down for two
   * minutes is usually down for a reason that will take somebody minutes more,
   * and a browser hammering it every second is both useless and, across a few
   * hundred open tabs, part of the problem. It stops at a minute rather than
   * backing off further because the interface promises to notice when the
   * service returns, and a five-minute ceiling makes that promise a lie.
   */
  backoff?: number[];
  /**
   * How stale the last answer may get before a healthy application checks
   * anyway.
   *
   * Ordinary traffic is the real health signal - see `reportReachable` - so on
   * a screen doing anything at all this never fires. It exists for the screen
   * doing nothing, where "connected" would otherwise be a claim about a minute
   * ago.
   */
  heartbeat?: number;
  /** How long a check may take before it counts as no answer. */
  timeout?: number;
}

const DEFAULTS = {
  confirmDelay: 1200,
  backoff: [3000, 6000, 12000, 20000, 30000, 60000],
  heartbeat: 45000,
  timeout: 8000,
} as const;

/**
 * A failure reported by ordinary traffic, folded into the same state.
 *
 * Separate from a probe result because a page that fans out twelve reads
 * produces twelve of these for one cause, within milliseconds of each other.
 * They are counted once - see `roundMs` below.
 */
export interface ConnectionReport {
  verdict: Exclude<ProbeVerdict, "ok">;
  detail?: string;
  status?: number;
}

/** Two failures closer together than this are one failure. */
const ROUND_MS = 1500;

/** How long "Connection restored" stays true after a recovery. */
export const RESTORED_MS = 5000;

export interface ConnectionMonitor {
  getSnapshot(): ConnectionSnapshot;
  subscribe(listener: () => void): () => void;
  /** Point it at a service. Called once, by the application's entry point. */
  configure(options: ConnectionOptions): void;
  /** Ordinary traffic succeeded: the service is there, no probe needed. */
  reportReachable(): void;
  /** Ordinary traffic failed in a way that says something about reachability. */
  reportUnreachable(report: ConnectionReport): void;
  /** Check now. Returns when the check settles. */
  check(): Promise<void>;
  /**
   * Run something the moment the service comes back.
   *
   * This is the half of the design that protects work in progress: a save that
   * failed during an outage does not have to be retried by hand if the screen
   * that owns it asks to be told. Returns an unsubscribe.
   */
  onRestore(listener: () => void): () => void;
  /**
   * Declare that there is unsaved work on screen.
   *
   * Nothing here blocks anything, so this changes no behaviour - it changes
   * what the outage surface SAYS, which is the difference between a person
   * copying a half-finished form into a text file and a person waiting four
   * seconds. Returns a release; call it when the work is saved or abandoned.
   */
  holdWork(): () => void;
}

function createMonitor(): ConnectionMonitor {
  let options: Required<Omit<ConnectionOptions, "probe">> & { probe?: Probe } = {
    ...DEFAULTS,
    backoff: [...DEFAULTS.backoff],
  };

  let snapshot: ConnectionSnapshot = {
    phase: "starting",
    checking: false,
    deviceOffline: typeof navigator !== "undefined" && navigator.onLine === false,
    failures: 0,
    since: Date.now(),
    heldWork: 0,
  };

  const listeners = new Set<() => void>();
  const restoreListeners = new Set<() => void>();
  let held = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let inFlight: Promise<void> | undefined;
  let lastCountedFailure = 0;
  let restoredTimer: ReturnType<typeof setTimeout> | undefined;

  function emit() {
    for (const l of listeners) l();
  }

  /**
   * Writes the next snapshot.
   *
   * The object is replaced rather than mutated because `useSyncExternalStore`
   * compares by identity, and a mutated object is invisible to it. The
   * converse matters just as much: when nothing meaningful changed, the same
   * object is kept, so a successful read on a healthy application - which
   * happens constantly - re-renders nothing.
   */
  function set(next: Partial<ConnectionSnapshot>) {
    const merged: ConnectionSnapshot = { ...snapshot, ...next };
    if (merged.phase !== snapshot.phase) merged.since = Date.now();
    let changed = false;
    for (const key of Object.keys(merged) as (keyof ConnectionSnapshot)[]) {
      if (merged[key] !== snapshot[key]) {
        changed = true;
        break;
      }
    }
    if (!changed) return;
    snapshot = merged;
    emit();
  }

  function clearTimer() {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  }

  const hidden = () => typeof document !== "undefined" && document.visibilityState === "hidden";

  /**
   * Schedules the next automatic check.
   *
   * Nothing is scheduled for a hidden tab. A laptop lid closed on a broken
   * service would otherwise wake up having made two hundred requests to it,
   * and the answer to all of them is one check made when somebody is actually
   * looking - which `watch` below fires on the visibility change.
   */
  function schedule(delay: number) {
    clearTimer();
    if (hidden()) {
      set({ nextCheckAt: undefined });
      return;
    }
    // A little noise on the interval, because a service that fell over
    // disconnected every browser watching it at the same instant, and a
    // synchronised retry from all of them is a second outage arriving on a
    // schedule.
    const jittered = Math.round(delay * (0.85 + Math.random() * 0.3));
    set({ nextCheckAt: Date.now() + jittered });
    timer = setTimeout(() => {
      timer = undefined;
      void check();
    }, jittered);
  }

  /** The wait before the next check, given how many have failed. */
  function backoffFor(failures: number): number {
    const ladder = options.backoff;
    if (ladder.length === 0) return DEFAULTS.backoff[0];
    const index = Math.min(Math.max(failures - 1, 0), ladder.length - 1);
    return ladder[index] ?? ladder[ladder.length - 1] ?? DEFAULTS.backoff[0];
  }

  /** The service answered. */
  function succeed() {
    const wasDown = snapshot.phase !== "online" && snapshot.phase !== "starting";
    const now = Date.now();
    set({
      phase: "online",
      failures: 0,
      lastOkAt: now,
      lastCheckAt: now,
      nextCheckAt: undefined,
      detail: undefined,
      status: undefined,
      restoredAt: wasDown ? now : undefined,
    });
    if (wasDown) {
      // The screens that were showing stale data are told before anything
      // else, so the interface is already refreshing itself by the time the
      // "restored" confirmation is read.
      for (const l of restoreListeners) l();
      if (restoredTimer !== undefined) clearTimeout(restoredTimer);
      restoredTimer = setTimeout(() => {
        restoredTimer = undefined;
        set({ restoredAt: undefined });
      }, RESTORED_MS);
    }
    schedule(options.heartbeat);
  }

  /**
   * Something did not answer.
   *
   * `confirmed` separates a probe's verdict from a passing request's: a probe
   * asked the one question and got no answer, which is evidence; a single
   * failed read might be anything, so the first one only buys a check.
   */
  function fail(report: ConnectionReport, confirmed: boolean) {
    const now = Date.now();
    // A PROBE'S ANSWER ALWAYS COUNTS; a report from ordinary traffic counts
    // once per round.
    //
    // The rounding exists for the page that fans out a read per product and
    // produces a dozen failures inside a second for one cause. Applying it to
    // probe results as well was a bug with two faces: the failure count stopped
    // climbing, so the backoff never widened - and, because the schedule only
    // moves on a counted failure, a monitor whose backoff was shorter than the
    // rounding window stopped scheduling checks altogether after the first one.
    // It only survived in this tool because the shipped ladder happens to start
    // wider than the window, which is not a thing to leave a copied folder
    // depending on.
    const fresh = confirmed || now - lastCountedFailure > ROUND_MS;
    if (fresh) lastCountedFailure = now;
    const failures = fresh ? snapshot.failures + 1 : snapshot.failures;

    // A service that answers 503 has answered: there is nothing to confirm and
    // no ambiguity to protect the reader from, and the state it is in has its
    // own name and its own sentence.
    const phase: ConnectionPhase =
      report.verdict === "unavailable"
        ? "unavailable"
        : confirmed || snapshot.phase === "offline" || snapshot.phase === "unavailable"
          ? "offline"
          : "unstable";
    const moved = phase !== snapshot.phase;

    set({
      phase,
      failures,
      lastCheckAt: now,
      detail: report.detail ?? snapshot.detail,
      status: report.status,
      restoredAt: undefined,
    });

    // Only a NEW failure moves the schedule - and a probe's answer is always
    // new, so the loop cannot stall. A page that fans out a read per product
    // reports a dozen failures inside a second for one cause, and rescheduling
    // on each would push the confirming check further away every time one
    // arrived, leaving the interface silent for exactly as long as the failures
    // kept coming, which is the whole outage.
    if (fresh || moved) {
      schedule(phase === "unstable" ? options.confirmDelay : backoffFor(failures));
    }
  }

  async function check(): Promise<void> {
    if (inFlight) return inFlight;
    const probe = options.probe;
    if (!probe) return;
    // The operating system says there is no network. A request would fail
    // regardless of the service's health, so making one would only teach the
    // interface something it already knows, and the sentence it should be
    // showing is about the device rather than about the service.
    if (typeof navigator !== "undefined" && navigator.onLine === false) {
      set({ deviceOffline: true, phase: "offline", nextCheckAt: undefined });
      clearTimer();
      return;
    }

    clearTimer();
    set({ checking: true, nextCheckAt: undefined });

    const controller = new AbortController();
    const cutoff = setTimeout(() => controller.abort(), options.timeout);

    inFlight = (async () => {
      try {
        const result = await probe(controller.signal);
        if (result.verdict === "ok") succeed();
        else fail({ ...result, verdict: result.verdict }, true);
      } catch (error) {
        fail(
          {
            verdict: "unreachable",
            detail: error instanceof Error ? error.message : String(error),
          },
          true,
        );
      } finally {
        clearTimeout(cutoff);
        set({ checking: false, lastCheckAt: Date.now() });
      }
    })();

    try {
      await inFlight;
    } finally {
      inFlight = undefined;
    }
  }

  /**
   * The browser's own signals, watched for as long as anybody is subscribed.
   *
   * `online` is worth acting on immediately: a laptop that has just rejoined a
   * network should not sit through the rest of a sixty-second backoff to
   * discover that everything works. `visibilitychange` is the same argument
   * for a tab coming back to the front - and the same argument in reverse for
   * one going away, which stops checking entirely.
   */
  function watch(): () => void {
    if (typeof window === "undefined") return () => {};
    const onOnline = () => {
      set({ deviceOffline: false });
      void check();
    };
    const onOffline = () => {
      clearTimer();
      set({ deviceOffline: true, phase: "offline", nextCheckAt: undefined });
    };
    const onVisibility = () => {
      if (hidden()) {
        clearTimer();
        set({ nextCheckAt: undefined });
        return;
      }
      if (snapshot.phase === "online" && Date.now() - (snapshot.lastOkAt ?? 0) < options.heartbeat) {
        schedule(options.heartbeat);
        return;
      }
      void check();
    };
    window.addEventListener("online", onOnline);
    window.addEventListener("offline", onOffline);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      window.removeEventListener("online", onOnline);
      window.removeEventListener("offline", onOffline);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }

  let unwatch: (() => void) | undefined;

  return {
    getSnapshot: () => snapshot,

    subscribe(listener) {
      listeners.add(listener);
      // The first subscriber starts the clock: a monitor nobody is looking at
      // has no reason to hold timers or event handlers.
      if (listeners.size === 1) {
        unwatch = watch();
        if (snapshot.phase === "starting" && options.probe) void check();
      }
      return () => {
        listeners.delete(listener);
        if (listeners.size === 0) {
          clearTimer();
          unwatch?.();
          unwatch = undefined;
        }
      };
    },

    configure(next) {
      options = {
        ...options,
        ...next,
        backoff: next.backoff ? [...next.backoff] : options.backoff,
      };
      if (options.probe && snapshot.phase === "starting" && listeners.size > 0) void check();
    },

    reportReachable() {
      // The cheap path, and it is walked on every successful request in the
      // application, so it does the least it can: a timestamp, and a schedule
      // pushed back. `set` keeps the snapshot identical when nothing else
      // moved, so this re-renders nothing on a healthy screen.
      const now = Date.now();
      const wasDown = snapshot.phase !== "online";
      if (wasDown) {
        succeed();
        return;
      }
      set({ lastOkAt: now, lastCheckAt: now, failures: 0 });
      if (timer === undefined && !hidden()) schedule(options.heartbeat);
    },

    reportUnreachable(report) {
      if (snapshot.checking) return;
      fail(report, false);
    },

    check,

    onRestore(listener) {
      restoreListeners.add(listener);
      return () => restoreListeners.delete(listener);
    },

    holdWork() {
      held += 1;
      set({ heldWork: held });
      let released = false;
      return () => {
        if (released) return;
        released = true;
        held = Math.max(0, held - 1);
        set({ heldWork: held });
      };
    },
  };
}

/**
 * The application's monitor.
 *
 * A singleton rather than a context, and for one reason: the code that knows
 * whether the service answered is the HTTP client, which is not a component
 * and has no context to read. A context would push that knowledge back into
 * the component tree, where it would be reported by whichever screens
 * remembered to - which is the arrangement this file exists to replace.
 */
export const connection: ConnectionMonitor = createMonitor();

/** For a test, or for a tool that talks to two services. */
export const createConnectionMonitor = createMonitor;

/** Subscribes a component to the connection. */
export function useConnection(monitor: ConnectionMonitor = connection): ConnectionSnapshot {
  return useSyncExternalStore(monitor.subscribe, monitor.getSnapshot, monitor.getSnapshot);
}

/**
 * Runs `fn` when the service comes back, and never on the way down.
 *
 * The natural use is a screen refreshing what it is showing; the important one
 * is a form retrying a save that failed while the service was away.
 */
export function useOnRestore(fn: () => void, monitor: ConnectionMonitor = connection): void {
  // Through a ref, so a caller passing an inline function does not resubscribe
  // on every render - and, more to the point, is not unsubscribed for the
  // instant in which the service comes back.
  const latest = useRef(fn);
  useEffect(() => {
    latest.current = fn;
  }, [fn]);
  useEffect(() => monitor.onRestore(() => latest.current()), [monitor]);
}

/**
 * Declares unsaved work for as long as `pending` is true.
 *
 * Nothing is blocked by this and nothing is prevented; it is what lets the
 * outage surface say that the work on screen is still there, which is the one
 * question somebody halfway through a form actually has.
 */
export function useHeldWork(pending: boolean, monitor: ConnectionMonitor = connection): void {
  useEffect(() => {
    if (!pending) return;
    return monitor.holdWork();
  }, [pending, monitor]);
}

/**
 * A number that counts down to `at`, re-rendered once a second and not at all
 * when there is nothing to count.
 *
 * Its own hook because three surfaces show the same countdown, and a component
 * that set up its own interval would tick while hidden behind a closed
 * popover.
 */
export function useCountdown(at: number | undefined): number | undefined {
  const [, tick] = useState(0);
  useEffect(() => {
    if (at === undefined) return;
    const id = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, [at]);
  if (at === undefined) return undefined;
  return Math.max(0, Math.ceil((at - Date.now()) / 1000));
}
