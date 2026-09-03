import { useId } from "react";
import { Typography } from "antd";
import { FadeIn } from "./motion";

// A small kit of modern, dependency-free SVG illustrations with gentle,
// reduced-motion-aware animations (the keyframes live in components.css).
// They replace the flat component-library Result/Empty glyphs on the "state"
// pages - processing, success, empty, offline, unreachable - so those pages
// read as considered rather than as a default nobody chose.
//
// They are HERE, in the shared kit, for the reason everything else is: a state
// screen is the same state screen in every tool on the platform, and one tool
// having an illustration where the other has a bare sentence is exactly the
// kind of difference that makes two products stop looking like one. Nothing in
// them names a product - they are drawings of situations, not of features.
// Each illustration is theme-aware: the "paper" shapes fill with --ill-surface
// (white in light, a lifted dark tone in dark) instead of a hard-coded white,
// so nothing reads as a bright blob on a dark card.

const OK = "#16a34a";
const OK2 = "#4ade80";
// Track the active accent (theme + preset) instead of a fixed blue, so the
// empty-state art belongs to whatever identity is running.
const BLUE = "var(--brand)";
const BLUE2 = "var(--brand-border)";
const AMBER = "#f59e0b";
// Failure reads a TOKEN rather than a hex: a preset that retunes the palette
// must not leave the error scene painted in the previous identity's red.
const DANGER = "var(--c-danger)";

// The theme-tracking fill for the solid "paper" shapes (trays, shields, cards).
const PAPER = "var(--ill-surface)";

// SuccessArt: a soft ring that pops in and a checkmark that draws itself, with
// two sparks - a warmer "done" than a static tick.
export function SuccessArt({ size = 132 }: { size?: number }) {
  const uid = useId();
  const ring = `ok-ring-${uid}`;
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Success" className="ill">
      <defs>
        <linearGradient id={ring} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor={OK2} />
          <stop offset="1" stopColor={OK} />
        </linearGradient>
      </defs>
      <circle cx="66" cy="66" r="52" fill={OK} opacity="0.10" className="ill-ripple" />
      <circle cx="66" cy="66" r="40" fill="none" stroke={`url(#${ring})`} strokeWidth="6" className="ill-pop" style={{ transformOrigin: "66px 66px" }} />
      <path
        d="M46 67 L61 81 L88 51"
        fill="none"
        stroke={`url(#${ring})`}
        strokeWidth="7"
        strokeLinecap="round"
        strokeLinejoin="round"
        className="ill-draw"
        pathLength={1}
      />
      <circle cx="98" cy="40" r="3" fill={OK2} className="ill-spark" style={{ animationDelay: "0.5s" }} />
      <circle cx="34" cy="52" r="2.5" fill={OK} className="ill-spark" style={{ animationDelay: "0.7s" }} />
    </svg>
  );
}

// ScanArt: a document with a scan beam sweeping over it and settings "found"
// as it passes - for the reading/processing state.
export function ScanArt({ size = 132 }: { size?: number }) {
  const uid = useId();
  const beam = `scan-beam-${uid}`;
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Scanning" className="ill">
      <defs>
        <linearGradient id={beam} x1="0" y1="0" x2="1" y2="0">
          <stop offset="0" stopColor={BLUE} stopOpacity="0" />
          <stop offset="0.5" stopColor={BLUE} stopOpacity="0.55" />
          <stop offset="1" stopColor={BLUE} stopOpacity="0" />
        </linearGradient>
      </defs>
      <g className="ill-floaty" style={{ transformOrigin: "66px 66px" }}>
        <rect x="40" y="30" width="52" height="68" rx="8" fill={PAPER} stroke={BLUE2} strokeWidth="2" />
        <rect x="40" y="30" width="52" height="68" rx="8" fill={BLUE} opacity="0.05" />
        {[44, 54, 64, 74, 84].map((y, i) => (
          <rect key={y} x="49" y={y} width={i % 2 ? 22 : 34} height="4" rx="2" fill={BLUE} opacity="0.28" />
        ))}
        {/* the sweeping beam */}
        <g className="ill-sweep">
          <rect x="40" y="30" width="52" height="10" fill={`url(#${beam})`} />
          <rect x="40" y="39" width="52" height="1.5" fill={BLUE} opacity="0.7" />
        </g>
      </g>
      <circle cx="98" cy="44" r="3.5" fill={BLUE} className="ill-spark" style={{ animationDelay: "0.2s" }} />
      <circle cx="34" cy="80" r="3" fill={BLUE2} className="ill-spark" style={{ animationDelay: "0.9s" }} />
    </svg>
  );
}

// EmptyArt: a friendly open tray with a document lifting out - nothing here
// yet, in a calm way. Drawn with a legible stroked body (not a washed-out
// fill) so it reads clearly on any surface, light or dark.
export function EmptyArt({ size = 120 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 120 120" role="img" aria-label="Nothing here" className="ill">
      {/* soft ground shadow */}
      <ellipse cx="60" cy="99" rx="32" ry="5.5" fill={BLUE} opacity="0.12" className="ill-ripple" style={{ transformOrigin: "60px 99px" }} />
      <g className="ill-floaty" style={{ transformOrigin: "60px 62px" }}>
        {/* the tray body: paper fill + a clear brand outline so it is always visible */}
        <path
          d="M28 60 h18 l6 9 h16 l6 -9 h18 l-7 28 a7 7 0 0 1 -7 5 H42 a7 7 0 0 1 -7 -5 Z"
          fill={PAPER}
          stroke={BLUE}
          strokeWidth="2.5"
          strokeLinejoin="round"
        />
        <path
          d="M28 60 h18 l6 9 h16 l6 -9 h18 l-7 28 a7 7 0 0 1 -7 5 H42 a7 7 0 0 1 -7 -5 Z"
          fill={BLUE}
          opacity="0.06"
        />
        {/* a single document lifting out of the tray */}
        <g className="ill-lift" style={{ transformOrigin: "60px 44px" }}>
          <rect x="44" y="26" width="32" height="26" rx="4" fill={PAPER} stroke={BLUE2} strokeWidth="2" />
          <rect x="50" y="33" width="20" height="3" rx="1.5" fill={BLUE} opacity="0.35" />
          <rect x="50" y="40" width="14" height="3" rx="1.5" fill={BLUE} opacity="0.28" />
        </g>
      </g>
      <circle cx="88" cy="34" r="3" fill={BLUE2} className="ill-spark" style={{ animationDelay: "0.4s" }} />
      <circle cx="30" cy="46" r="2.5" fill={BLUE} className="ill-spark" style={{ animationDelay: "0.9s" }} />
    </svg>
  );
}

// AllClearArt: a shield with a drawn check and calm ripple - everything is in
// order (no drift, nothing failing, nothing waiting).
export function AllClearArt({ size = 132 }: { size?: number }) {
  const uid = useId();
  const grad = `clear-shield-${uid}`;
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="All clear" className="ill">
      <defs>
        <linearGradient id={grad} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor={OK2} />
          <stop offset="1" stopColor={OK} />
        </linearGradient>
      </defs>
      <circle cx="66" cy="66" r="52" fill={OK} opacity="0.08" className="ill-ripple" />
      <g className="ill-floaty" style={{ transformOrigin: "66px 66px" }}>
        <path
          d="M66 30 l28 10 v22 c0 18 -12 30 -28 38 c-16 -8 -28 -20 -28 -38 v-22 Z"
          fill={PAPER}
          stroke={`url(#${grad})`}
          strokeWidth="3.5"
          strokeLinejoin="round"
        />
        <path
          d="M66 30 l28 10 v22 c0 18 -12 30 -28 38 c-16 -8 -28 -20 -28 -38 v-22 Z"
          fill={OK}
          opacity="0.07"
        />
        <path
          d="M54 66 L63 75 L80 55"
          fill="none"
          stroke={`url(#${grad})`}
          strokeWidth="6"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="ill-draw"
          pathLength={1}
        />
      </g>
      <circle cx="102" cy="42" r="3" fill={OK2} className="ill-spark" style={{ animationDelay: "0.5s" }} />
      <circle cx="30" cy="58" r="2.5" fill={OK} className="ill-spark" style={{ animationDelay: "0.8s" }} />
    </svg>
  );
}

// InboxZeroArt: an open tray with a small check floating in - the queue is
// empty in a good way (nothing waiting for you).
export function InboxZeroArt({ size = 132 }: { size?: number }) {
  const uid = useId();
  const grad = `zero-ok-${uid}`;
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Nothing waiting" className="ill">
      <defs>
        <linearGradient id={grad} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor={OK2} />
          <stop offset="1" stopColor={OK} />
        </linearGradient>
      </defs>
      <circle cx="66" cy="70" r="44" fill={BLUE} opacity="0.05" className="ill-ripple" />
      <g className="ill-floaty" style={{ transformOrigin: "66px 72px" }}>
        <ellipse cx="66" cy="106" rx="36" ry="6" fill={BLUE} opacity="0.08" />
        <path
          d="M34 66 h20 l6 9 h12 l6 -9 h20 l-7 30 a6 6 0 0 1 -6 5 H47 a6 6 0 0 1 -6 -5 Z"
          fill={PAPER}
          stroke={BLUE}
          strokeWidth="2.5"
          strokeLinejoin="round"
        />
        <path
          d="M34 66 h20 l6 9 h12 l6 -9 h20 l-7 30 a6 6 0 0 1 -6 5 H47 a6 6 0 0 1 -6 -5 Z"
          fill={BLUE}
          opacity="0.08"
        />
        <path d="M44 66 v-14 a5 5 0 0 1 5 -5 h34 a5 5 0 0 1 5 5 v14" fill="none" stroke={BLUE} strokeWidth="2.5" opacity="0.55" />
        <circle cx="66" cy="38" r="14" fill={PAPER} stroke={`url(#${grad})`} strokeWidth="3" className="ill-pop" style={{ transformOrigin: "66px 38px" }} />
        <path
          d="M59 38.5 L64 43.5 L73.5 32.5"
          fill="none"
          stroke={`url(#${grad})`}
          strokeWidth="3.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="ill-draw"
          pathLength={1}
        />
      </g>
      <circle cx="100" cy="52" r="3" fill={BLUE2} className="ill-spark" style={{ animationDelay: "0.4s" }} />
      <circle cx="32" cy="84" r="2.5" fill={OK2} className="ill-spark" style={{ animationDelay: "0.9s" }} />
    </svg>
  );
}

// InSyncArt: a git graph whose two branches have converged, with a soft check
// where they meet - Configer and the repository are in step, nothing has
// drifted. A friendlier "all caught up" than a security shield.
export function InSyncArt({ size = 132 }: { size?: number }) {
  const uid = useId();
  const grad = `sync-ok-${uid}`;
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="In sync" className="ill">
      <defs>
        <linearGradient id={grad} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor={OK2} />
          <stop offset="1" stopColor={OK} />
        </linearGradient>
      </defs>
      <circle cx="66" cy="66" r="50" fill={OK} opacity="0.07" className="ill-ripple" />
      <g className="ill-floaty" style={{ transformOrigin: "66px 66px" }}>
        {/* the trunk */}
        <line x1="42" y1="34" x2="42" y2="98" stroke={BLUE2} strokeWidth="3" strokeLinecap="round" opacity="0.55" />
        {/* a branch that leaves and merges back */}
        <path d="M42 52 C42 44, 84 44, 84 62 C84 80, 42 80, 42 88" fill="none" stroke={BLUE} strokeWidth="3" strokeLinecap="round" opacity="0.5" />
        {[34, 70, 98].map((cy) => (
          <circle key={cy} cx="42" cy={cy} r="5.5" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        ))}
        <circle cx="84" cy="62" r="5.5" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        {/* the convergence check */}
        <circle cx="84" cy="62" r="15" fill={PAPER} stroke={`url(#${grad})`} strokeWidth="3" className="ill-pop" style={{ transformOrigin: "84px 62px" }} />
        <path
          d="M77 62.5 L82 67.5 L91.5 56.5"
          fill="none"
          stroke={`url(#${grad})`}
          strokeWidth="3.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="ill-draw"
          pathLength={1}
        />
      </g>
      <circle cx="104" cy="40" r="3" fill={OK2} className="ill-spark" style={{ animationDelay: "0.5s" }} />
      <circle cx="28" cy="76" r="2.5" fill={BLUE2} className="ill-spark" style={{ animationDelay: "0.9s" }} />
    </svg>
  );
}

// OfflineArt: a cloud with a gently pulsing dashed link - the service is
// briefly unreachable, not broken.
export function OfflineArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Reconnecting" className="ill">
      <circle cx="66" cy="66" r="46" fill={AMBER} opacity="0.08" className="ill-ripple" />
      <g className="ill-floaty" style={{ transformOrigin: "66px 60px" }}>
        <path
          d="M48 74 a15 15 0 0 1 1 -30 a20 20 0 0 1 38 6 a13 13 0 0 1 -3 24 Z"
          fill={PAPER}
          stroke={AMBER}
          strokeWidth="2.5"
        />
        <path d="M52 84 h28" stroke={AMBER} strokeWidth="3" strokeLinecap="round" strokeDasharray="2 6" className="ill-dash" />
      </g>
    </svg>
  );
}

// --- the identity & availability family -------------------------------------
//
// Sign in, signed out, session expired, service unreachable, access denied and
// "no such page" are the pages a person meets when they cannot get on with
// their work. They are drawn as ONE family - the same isometric config surface,
// the same brand tones, the same soft ground shadow - so an outage, a lock and a
// welcome all read as parts of this product rather than six unrelated errors.
// Each scene puts a single badge on that surface, and the badge is the whole
// message: a check, a lock, a clock, an alert.

// The isometric plate every scene in this family stands on.
function Plate({ x = 66, y = 104, rx = 42 }: { x?: number; y?: number; rx?: number }) {
  return <ellipse cx={x} cy={y} rx={rx} ry={rx * 0.16} fill={BLUE} opacity="0.12" />;
}

// WorkspaceArt is the sign-in hero: the product's own subject matter - the
// repository's files on the left, the parameter grid they resolve into on the
// right, and the settings gear that turns one into the other. It is the one
// illustration that carries personality; everything else on the page is quiet.
export function WorkspaceArt({ size = 300 }: { size?: number }) {
  const uid = useId();
  const face = `ws-face-${uid}`;
  // The grid inside the window: a name column and two instance columns, with
  // one cell lit - the difference between instances is the whole product.
  const rows = [0, 1, 2, 3];
  return (
    <svg
      width={size}
      height={size * 0.72}
      viewBox="0 0 250 180"
      role="img"
      aria-label="Configuration files resolved into a parameter grid"
      className="ill"
    >
      <defs>
        <linearGradient id={face} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor={BLUE} stopOpacity="0.10" />
          <stop offset="1" stopColor={BLUE} stopOpacity="0.02" />
        </linearGradient>
      </defs>

      <ellipse cx="132" cy="163" rx="88" ry="11" fill={BLUE} opacity="0.10" />

      {/* the repository's own files, waiting to be read */}
      <g className="ill-lift" style={{ transformOrigin: "44px 96px" }}>
        <g transform="rotate(-7 44 100)">
          <rect x="20" y="76" width="44" height="56" rx="6" fill={PAPER} stroke={BLUE2} strokeWidth="2" />
          {[86, 95, 104, 113].map((y, i) => (
            <rect key={y} x="28" y={y} width={i % 2 ? 18 : 27} height="3.5" rx="1.75" fill={BLUE} opacity="0.26" />
          ))}
        </g>
        <g transform="rotate(6 52 84)">
          <rect x="32" y="58" width="44" height="56" rx="6" fill={PAPER} stroke={BLUE} strokeWidth="2" />
          <rect x="32" y="58" width="44" height="56" rx="6" fill={`url(#${face})`} />
          {[68, 77, 86, 95].map((y, i) => (
            <rect key={y} x="40" y={y} width={i % 2 ? 16 : 26} height="3.5" rx="1.75" fill={BLUE} opacity="0.34" />
          ))}
        </g>
      </g>

      {/* the path an edit travels: file -> commit -> grid */}
      <path d="M84 96 C96 96, 96 88, 108 88" fill="none" stroke={BLUE} strokeWidth="2" strokeDasharray="3 5" opacity="0.5" className="ill-dash" />
      <circle cx="84" cy="96" r="4" fill={PAPER} stroke={BLUE} strokeWidth="2" />

      {/* the grid: the window this product is */}
      <g className="ill-floaty" style={{ transformOrigin: "170px 92px" }}>
        <rect x="108" y="44" width="124" height="96" rx="10" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        <path d="M108 62 h124" stroke={BLUE} strokeWidth="2" opacity="0.35" />
        {[118, 127, 136].map((cx) => (
          <circle key={cx} cx={cx} cy="53" r="2.4" fill={BLUE} opacity="0.4" />
        ))}
        {/* column heads: two instances */}
        <rect x="174" y="68" width="22" height="4" rx="2" fill={BLUE} opacity="0.45" />
        <rect x="202" y="68" width="22" height="4" rx="2" fill={BLUE} opacity="0.45" />
        {rows.map((r) => {
          const y = 80 + r * 14;
          return (
            <g key={r}>
              {/* parameter name */}
              <rect x="118" y={y} width={r === 1 ? 34 : 44} height="5" rx="2.5" fill={BLUE} opacity="0.24" />
              {/* the two instance cells; one is edited */}
              <rect x="174" y={y - 2} width="22" height="9" rx="2.5" fill={BLUE} opacity={r === 1 ? 0.45 : 0.12} />
              <rect x="202" y={y - 2} width="22" height="9" rx="2.5" fill={BLUE} opacity={r === 2 ? 0.45 : 0.12} />
            </g>
          );
        })}
        {/* the cell being edited, ringed */}
        <rect x="172" y="92" width="26" height="13" rx="3.5" fill="none" stroke={BLUE} strokeWidth="2" />
      </g>

      {/* settings: what the grid resolves to */}
      <g className="ill-turn" style={{ transformOrigin: "218px 40px" }}>
        <circle cx="218" cy="40" r="16" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        {[0, 45, 90, 135, 180, 225, 270, 315].map((a) => (
          <rect key={a} x="215.5" y="20" width="5" height="8" rx="2" fill={BLUE} opacity="0.8" transform={`rotate(${a} 218 40)`} />
        ))}
        <circle cx="218" cy="40" r="5.5" fill={BLUE} opacity="0.30" />
      </g>

      <circle cx="96" cy="52" r="3" fill={BLUE2} className="ill-spark" style={{ animationDelay: "0.5s" }} />
      <circle cx="240" cy="118" r="2.5" fill={BLUE} className="ill-spark" style={{ animationDelay: "1.1s" }} />
    </svg>
  );
}

// SignedOutArt: the session card leaving the surface, with a check - a
// deliberate, completed action, not a failure.
export function SignedOutArt({ size = 132 }: { size?: number }) {
  const uid = useId();
  const grad = `out-ok-${uid}`;
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Signed out" className="ill">
      <defs>
        <linearGradient id={grad} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor={OK2} />
          <stop offset="1" stopColor={OK} />
        </linearGradient>
      </defs>
      <circle cx="66" cy="62" r="46" fill={BLUE} opacity="0.06" className="ill-ripple" />
      <Plate />
      <g className="ill-floaty" style={{ transformOrigin: "66px 64px" }}>
        {/* the workspace door */}
        <path d="M40 34 h34 a4 4 0 0 1 4 4 v52 a4 4 0 0 1 -4 4 h-34 Z" fill={PAPER} stroke={BLUE} strokeWidth="2.5" strokeLinejoin="round" />
        <path d="M40 34 h34 a4 4 0 0 1 4 4 v52 a4 4 0 0 1 -4 4 h-34 Z" fill={BLUE} opacity="0.06" />
        <circle cx="70" cy="64" r="2.5" fill={BLUE} opacity="0.6" />
        {/* leaving it */}
        <g className="ill-exit">
          <path d="M86 64 h18" stroke={BLUE} strokeWidth="3" strokeLinecap="round" />
          <path d="M98 57 l7 7 l-7 7" fill="none" stroke={BLUE} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
        </g>
      </g>
      <circle cx="46" cy="98" r="12" fill={PAPER} stroke={`url(#${grad})`} strokeWidth="3" className="ill-pop" style={{ transformOrigin: "46px 98px" }} />
      <path d="M40 98.5 L44.5 103 L52.5 92.5" fill="none" stroke={`url(#${grad})`} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" className="ill-draw" pathLength={1} />
    </svg>
  );
}

// SessionExpiredArt: the workspace behind a lock, with a clock badge - time ran
// out, nothing broke.
export function SessionExpiredArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Session expired" className="ill">
      <circle cx="66" cy="62" r="46" fill={BLUE} opacity="0.06" className="ill-ripple" />
      <Plate />
      <g className="ill-floaty" style={{ transformOrigin: "66px 62px" }}>
        <rect x="30" y="30" width="72" height="60" rx="8" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        <path d="M30 44 h72" stroke={BLUE} strokeWidth="2" opacity="0.5" />
        {[38, 45, 52].map((cx) => (
          <circle key={cx} cx={cx} cy="37" r="2.2" fill={BLUE} opacity="0.45" />
        ))}
        {/* the lock on the workspace */}
        <rect x="55" y="60" width="22" height="18" rx="4" fill={BLUE} opacity="0.16" />
        <rect x="55" y="60" width="22" height="18" rx="4" fill="none" stroke={BLUE} strokeWidth="2.5" />
        <path d="M60 60 v-6 a6 6 0 0 1 12 0 v6" fill="none" stroke={BLUE} strokeWidth="2.5" strokeLinecap="round" />
      </g>
      {/* the clock badge: what actually happened */}
      <g className="ill-pop" style={{ transformOrigin: "98px 92px" }}>
        <circle cx="98" cy="92" r="15" fill={PAPER} stroke={AMBER} strokeWidth="3" />
        <path d="M98 84 v9 l6 4" fill="none" stroke={AMBER} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
      </g>
    </svg>
  );
}

// ServiceDownArt: the service's own machines with an alert badge - the thing
// that is unreachable, named in the picture.
export function ServiceDownArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Service unreachable" className="ill">
      <circle cx="66" cy="62" r="46" fill={AMBER} opacity="0.07" className="ill-ripple" />
      <Plate />
      <g className="ill-floaty" style={{ transformOrigin: "66px 62px" }}>
        {[0, 1, 2].map((i) => {
          const y = 34 + i * 22;
          return (
            <g key={i}>
              <rect x="34" y={y} width="64" height="18" rx="5" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
              <rect x="34" y={y} width="64" height="18" rx="5" fill={BLUE} opacity="0.05" />
              <circle cx="44" cy={y + 9} r="3" fill={i === 1 ? AMBER : BLUE} opacity={i === 1 ? 0.9 : 0.4} className={i === 1 ? "ill-spark" : undefined} />
              <rect x="54" y={y + 7} width={i === 1 ? 18 : 30} height="4" rx="2" fill={BLUE} opacity="0.22" />
            </g>
          );
        })}
      </g>
      <g className="ill-pop" style={{ transformOrigin: "100px 90px" }}>
        <circle cx="100" cy="90" r="15" fill={PAPER} stroke={AMBER} strokeWidth="3" />
        <path d="M100 82 v10" stroke={AMBER} strokeWidth="3" strokeLinecap="round" />
        <circle cx="100" cy="97.5" r="1.9" fill={AMBER} />
      </g>
    </svg>
  );
}

// AccessDeniedArt: a shield holding the lock - not an error, a boundary.
export function AccessDeniedArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Not permitted" className="ill">
      <circle cx="66" cy="64" r="46" fill={BLUE} opacity="0.06" className="ill-ripple" />
      <Plate y={110} rx={34} />
      <g className="ill-floaty" style={{ transformOrigin: "66px 64px" }}>
        <path d="M66 26 l30 11 v24 c0 19 -13 32 -30 40 c-17 -8 -30 -21 -30 -40 v-24 Z" fill={PAPER} stroke={BLUE} strokeWidth="2.5" strokeLinejoin="round" />
        <path d="M66 26 l30 11 v24 c0 19 -13 32 -30 40 c-17 -8 -30 -21 -30 -40 v-24 Z" fill={BLUE} opacity="0.06" />
        <rect x="55" y="61" width="22" height="18" rx="4" fill={BLUE} opacity="0.18" />
        <rect x="55" y="61" width="22" height="18" rx="4" fill="none" stroke={BLUE} strokeWidth="2.5" />
        <path d="M60 61 v-6 a6 6 0 0 1 12 0 v6" fill="none" stroke={BLUE} strokeWidth="2.5" strokeLinecap="round" />
      </g>
    </svg>
  );
}

// NotFoundArt: a page being searched for and not found. The number lives in the
// copy, not in the picture.
export function NotFoundArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Page not found" className="ill">
      <circle cx="66" cy="62" r="46" fill={BLUE} opacity="0.06" className="ill-ripple" />
      <Plate />
      <g className="ill-floaty" style={{ transformOrigin: "66px 62px" }}>
        <rect x="36" y="28" width="56" height="66" rx="8" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        {[42, 52, 62].map((y, i) => (
          <rect key={y} x="46" y={y} width={i === 1 ? 24 : 36} height="4" rx="2" fill={BLUE} opacity="0.24" />
        ))}
        <g className="ill-lift" style={{ transformOrigin: "84px 82px" }}>
          <circle cx="80" cy="78" r="16" fill={PAPER} fillOpacity="0.6" stroke={BLUE} strokeWidth="3" />
          <path d="M91 89 l12 12" stroke={BLUE} strokeWidth="4" strokeLinecap="round" />
        </g>
      </g>
    </svg>
  );
}

// MaintenanceArt: the service's own machines, deliberately at rest, with two
// meshed gears turning where the other scenes in this family put a badge.
// Planned work is the one state in the family that is nobody's fault, so it
// gets the family's calm colour treatment and a badge that MOVES rather than
// one that warns: an alert triangle here would say "outage" when the whole
// point is that this was scheduled.
export function MaintenanceArt({ size = 132 }: { size?: number }) {
  // Two gears only read as meshed if the small one turns the OTHER way, and
  // faster in proportion to its radius. Pitch radii of roughly 16 and 10 put
  // their periods at 12s and 7.5s, which is what `.ill-gear` and
  // `.ill-gear-counter` are, and their centres 24 apart, which is why the small
  // one sits where it does.
  const teeth = [0, 45, 90, 135, 180, 225, 270, 315];
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Under maintenance" className="ill">
      <circle cx="66" cy="58" r="44" fill={AMBER} opacity="0.07" className="ill-ripple" />
      <Plate />
      <g className="ill-floaty" style={{ transformOrigin: "66px 58px" }} opacity="0.92">
        {[0, 1, 2].map((i) => {
          const y = 30 + i * 22;
          return (
            <g key={i}>
              <rect x="34" y={y} width="64" height="18" rx="5" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
              <rect x="34" y={y} width="64" height="18" rx="5" fill={BLUE} opacity="0.05" />
              {/* every light quiet: the machines are down on purpose */}
              <circle cx="44" cy={y + 9} r="3" fill={BLUE} opacity="0.32" />
              <rect x="54" y={y + 7} width={i === 1 ? 20 : 30} height="4" rx="2" fill={BLUE} opacity="0.2" />
            </g>
          );
        })}
      </g>
      {/* the driving gear, in the badge's place */}
      <g className="ill-gear" style={{ transformOrigin: "97px 90px" }}>
        <circle cx="97" cy="90" r="11.5" fill={PAPER} stroke={AMBER} strokeWidth="3" />
        {teeth.map((a) => (
          <rect key={a} x="94.6" y="74.5" width="4.8" height="7" rx="1.8" fill={AMBER} transform={`rotate(${a} 97 90)`} />
        ))}
        <circle cx="97" cy="90" r="4" fill={AMBER} opacity="0.28" />
      </g>
      {/* and the one it drives */}
      <g className="ill-gear-counter" style={{ transformOrigin: "76px 102px" }}>
        <circle cx="76" cy="102" r="6.5" fill={PAPER} stroke={AMBER} strokeWidth="2.5" />
        {teeth.map((a) => (
          <rect key={a} x="74.4" y="92.4" width="3.2" height="4.6" rx="1.4" fill={AMBER} opacity="0.85" transform={`rotate(${a} 76 102)`} />
        ))}
      </g>
    </svg>
  );
}

// ErrorArt: the page's own window, split by a fracture that draws itself, under
// an alert triangle. The triangle rather than the family's circular badge is
// deliberate: an unexpected failure is the one state here that is not a normal
// part of operating the system, and it should not look like one.
export function ErrorArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Unexpected error" className="ill">
      <circle cx="66" cy="58" r="44" fill={DANGER} opacity="0.07" className="ill-ripple" />
      <Plate />
      <g className="ill-floaty" style={{ transformOrigin: "66px 58px" }}>
        <rect x="30" y="26" width="72" height="62" rx="8" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        <path d="M30 40 h72" stroke={BLUE} strokeWidth="2" opacity="0.45" />
        {[38, 45, 52].map((cx) => (
          <circle key={cx} cx={cx} cy="33" r="2.2" fill={BLUE} opacity="0.4" />
        ))}
        {/* the content it never finished painting */}
        <rect x="38" y="50" width="26" height="4.5" rx="2.25" fill={BLUE} opacity="0.22" />
        <rect x="38" y="60" width="18" height="4.5" rx="2.25" fill={BLUE} opacity="0.16" />
        <rect x="76" y="50" width="18" height="4.5" rx="2.25" fill={BLUE} opacity="0.16" />
        <rect x="72" y="60" width="22" height="4.5" rx="2.25" fill={BLUE} opacity="0.22" />
        {/* the fracture, drawn once on arrival */}
        <path
          d="M68 40 L61 52 L71 58 L62 72 L69 78 L64 88"
          fill="none"
          stroke={DANGER}
          strokeWidth="3"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="ill-draw"
          pathLength={1}
        />
      </g>
      {/* the badge: what happened, and nothing else */}
      <g className="ill-pop" style={{ transformOrigin: "99px 92px" }}>
        <path d="M99 79 l14 24 h-28 Z" fill={PAPER} stroke={DANGER} strokeWidth="3" strokeLinejoin="round" />
        <path d="M99 88 v6" stroke={DANGER} strokeWidth="2.8" strokeLinecap="round" />
        <circle cx="99" cy="98.4" r="1.8" fill={DANGER} />
      </g>
    </svg>
  );
}

// LoadingArt: two arcs orbiting a card whose lines are still filling in. It is
// the one illustration in the kit that must never look like a RESULT, so it
// carries no badge, no check and no colour that means anything - only motion
// that continues for as long as the wait does.
//
// Indeterminate by construction: nothing here fills, advances or completes, so
// it cannot imply a position the caller does not have. A page that knows its
// denominator should show a progress bar instead.
export function LoadingArt({ size = 132 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 132 132" role="img" aria-label="Loading" className="ill">
      {/* the outer track, and the arc travelling it */}
      <circle cx="66" cy="62" r="41" fill="none" stroke={BLUE} strokeWidth="2" opacity="0.12" />
      <g className="ill-orbit" style={{ transformOrigin: "66px 62px" }}>
        <circle
          cx="66"
          cy="62"
          r="41"
          fill="none"
          stroke={BLUE}
          strokeWidth="3.5"
          strokeLinecap="round"
          strokeDasharray="64 194"
        />
      </g>
      <g className="ill-orbit-counter" style={{ transformOrigin: "66px 62px" }}>
        <circle
          cx="66"
          cy="62"
          r="31"
          fill="none"
          stroke={BLUE}
          strokeWidth="2.5"
          strokeLinecap="round"
          strokeDasharray="30 165"
          opacity="0.42"
        />
      </g>
      {/* the page being assembled inside them */}
      <g className="ill-floaty" style={{ transformOrigin: "66px 62px" }}>
        <rect x="45" y="43" width="42" height="38" rx="8" fill={PAPER} stroke={BLUE} strokeWidth="2.5" />
        <rect x="45" y="43" width="42" height="38" rx="8" fill={BLUE} opacity="0.05" />
        {[53, 61.5, 70].map((y, i) => (
          <rect
            key={y}
            x="53"
            y={y}
            width={i === 1 ? 17 : 26}
            height="4.5"
            rx="2.25"
            fill={BLUE}
            className="ill-pulse"
            style={{ animationDelay: `${i * 0.18}s` }}
          />
        ))}
      </g>
    </svg>
  );
}

// StatePanel is the standard layout for these pages: a centered illustration,
// a title, an optional subtitle, optional extra content, and a row of actions.
export function StatePanel({
  art,
  title,
  subtitle,
  actions,
  children,
  style,
}: {
  art: React.ReactNode;
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  children?: React.ReactNode;
  style?: React.CSSProperties;
}) {
  return (
    <FadeIn
      style={{
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        textAlign: "center",
        gap: 10,
        padding: "28px 20px",
        maxWidth: 560,
        margin: "0 auto",
        ...style,
      }}
    >
      {art}
      <Typography.Title level={4} style={{ margin: "6px 0 0" }}>
        {title}
      </Typography.Title>
      {subtitle && (
        <Typography.Text type="secondary" style={{ fontSize: 13.5, maxWidth: 480 }}>
          {subtitle}
        </Typography.Text>
      )}
      {children}
      {actions && <div style={{ display: "flex", gap: 10, flexWrap: "wrap", justifyContent: "center", marginTop: 8 }}>{actions}</div>}
    </FadeIn>
  );
}
