import type { ReactNode } from "react";
import { Badge, Tooltip } from "antd";
import type { BrandIdentity } from "../brand";
import { BrandLockup, BrandMark } from "./BrandLockup";
import { ChevronLeftIcon, ChevronRightIcon } from "../icons";

// THE APPLICATION CHROME, shared whole.
//
// The navigation and the bar above the page are the two surfaces a person sees
// on every screen of every tool, so they are the two that must not be written
// twice. Two products that share a palette but each draw their own sidebar do
// not look like one product; they look like two products with the same colours,
// which is worse than not trying, because the difference reads as carelessness
// rather than as intent.
//
// So the STRUCTURE lives here - the widths, the paddings, the item heights, the
// hover and active language, the collapse behaviour, the profile card at the
// foot, the bar's height and how its two ends are arranged - and each app hands
// in only what is genuinely its own: which entries there are, what they do, and
// what belongs in its bar. Nothing here names a product.

export interface NavItem {
  key: string;
  label: string;
  icon: ReactNode;
  /** a count worth interrupting for; 0 or undefined draws nothing */
  badge?: number;
  disabled?: boolean;
  /** why it is disabled, said in the tooltip rather than left to guesswork */
  disabledHint?: string;
  onClick?: () => void;
}

export interface NavProfile {
  name: string;
  /** the second line: a login, a role, an account */
  sub?: string;
  avatarUrl?: string;
  /** shown when there is no avatar image; the initials are derived if omitted */
  initials?: string;
  active?: boolean;
  onClick?: () => void;
  tooltip?: string;
}

/** One entry in the navigation. Exported because a sign-in entry and the
 *  collapse control are the same shape as a destination. */
export function NavEntry({
  item,
  active,
  collapsed,
}: {
  item: NavItem;
  active: boolean;
  collapsed: boolean;
}) {
  const disabled = !!item.disabled;
  const inner = (
    <div
      className={`ui-nav-item${active ? " is-active" : ""}${disabled ? " is-disabled" : ""}${
        collapsed ? " is-collapsed" : ""
      }`}
      onClick={disabled ? undefined : item.onClick}
      onKeyDown={(e) => {
        if (disabled) return;
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          item.onClick?.();
        }
      }}
      role="button"
      tabIndex={disabled ? -1 : 0}
      aria-disabled={disabled}
      aria-current={active ? "page" : undefined}
    >
      <Badge
        count={item.badge ?? 0}
        size="small"
        offset={collapsed ? [2, 0] : [4, 0]}
        color="var(--nav-bg-active)"
      >
        <span className="ui-nav-item-icon">{item.icon}</span>
      </Badge>
      {!collapsed && <span className="ui-nav-item-label">{item.label}</span>}
    </div>
  );
  const tip = disabled ? (item.disabledHint ?? item.label) : collapsed ? item.label : "";
  return tip ? (
    <Tooltip title={tip} placement="right">
      {inner}
    </Tooltip>
  ) : (
    inner
  );
}

/**
 * The navigation: the product's one piece of dark chrome.
 *
 * Items are quiet by default, lighten on hover, and the active one becomes a
 * brand-filled pill. Collapsed, it keeps the icons and moves every label into a
 * tooltip, so the same component serves both widths rather than a second
 * narrow one drifting away from the wide one.
 */
export function SideNav({
  brand,
  items,
  activeKey,
  collapsed = false,
  onToggleCollapse,
  profile,
  profileSlot,
  footer,
  collapseLabel = "Collapse",
}: {
  brand: BrandIdentity;
  items: NavItem[];
  activeKey?: string;
  collapsed?: boolean;
  /** omit to make the navigation a fixed width with no collapse control */
  onToggleCollapse?: () => void;
  /** the person, at the foot; omit on a tool with no identity to show */
  profile?: NavProfile | null;
  /** what stands in the profile's place when there is no person to show - a
   *  sign-in entry, most usefully. Identity is where a session starts, so the
   *  foot must not simply go blank when nobody is signed in. */
  profileSlot?: ReactNode;
  /** anything below the profile: a deployment chip, a version */
  footer?: ReactNode;
  collapseLabel?: string;
}) {
  return (
    <nav className={`ui-nav${collapsed ? " is-collapsed" : ""}`} aria-label="Main">
      <div className="ui-nav-brand">
        {collapsed ? (
          <BrandMark brand={brand} />
        ) : (
          <BrandLockup brand={brand} onDark />
        )}
      </div>

      <div className="ui-nav-items">
        {items.map((it) => (
          <NavEntry key={it.key} item={it} active={activeKey === it.key} collapsed={collapsed} />
        ))}
      </div>

      <div className="ui-nav-foot">
        {profile ? <ProfileCard profile={profile} collapsed={collapsed} /> : profileSlot}
        {onToggleCollapse && (
          <NavEntry
            item={{
              key: "collapse",
              label: collapseLabel,
              icon: collapsed ? <ChevronRightIcon /> : <ChevronLeftIcon />,
              onClick: onToggleCollapse,
            }}
            active={false}
            collapsed={collapsed}
          />
        )}
        {!collapsed && footer}
      </div>
    </nav>
  );
}

/** The person, at the navigation's foot: who you are, and one click to every
 *  personal preference. Same quiet/hover/active language as an entry, so the
 *  foot reads as part of the navigation rather than as a separate widget. */
function ProfileCard({ profile, collapsed }: { profile: NavProfile; collapsed: boolean }) {
  const initials =
    profile.initials ?? (profile.name || "?").slice(0, 2).toUpperCase();
  const inner = (
    <div
      className={`ui-nav-profile${profile.active ? " is-active" : ""}${
        collapsed ? " is-collapsed" : ""
      }`}
      role="button"
      tabIndex={0}
      aria-label={`${profile.name} - open settings`}
      onClick={profile.onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          profile.onClick?.();
        }
      }}
    >
      {profile.avatarUrl ? (
        <img className="ui-nav-avatar" src={profile.avatarUrl} alt="" />
      ) : (
        <span className="ui-nav-avatar is-initials">{initials}</span>
      )}
      {!collapsed && (
        <div className="ui-nav-profile-text">
          <div className="ui-nav-profile-name">{profile.name}</div>
          {profile.sub && <div className="ui-nav-profile-sub">{profile.sub}</div>}
        </div>
      )}
    </div>
  );
  return (
    <Tooltip
      title={collapsed ? profile.name : (profile.tooltip ?? "Profile and settings")}
      placement="right"
    >
      {inner}
    </Tooltip>
  );
}

/**
 * The bar above the page.
 *
 * It carries WHERE YOU ARE on the left and the standing controls on the right,
 * in that order, at the same height, in both tools. What goes in each end is
 * the app's business - a breadcrumb here, a section name there - but the shape
 * is not, which is why they are slots rather than two separate bars.
 *
 * `line-height: normal` is set in the stylesheet rather than left to the
 * component library, whose header sets a 64px line box that anything inline
 * inherits: a 26px pill became a 72px ellipse hanging out of the bar.
 */
export function TopBar({
  title,
  left,
  right,
  children,
}: {
  /** where you are, in one strong phrase - the common case, styled once so
   *  every tool says it at the same size and weight */
  title?: ReactNode;
  /** where you are, when it needs more than a phrase (a breadcrumb, a
   *  switcher). Layout only: it carries no type of its own, so what goes in
   *  keeps its own. */
  left?: ReactNode;
  /** the standing controls: search, theme, notifications, the account */
  right?: ReactNode;
  /** anything that should sit between the two and take the slack */
  children?: ReactNode;
}) {
  return (
    <header className="ui-topbar">
      {title && <div className="ui-topbar-title">{title}</div>}
      {left && <div className="ui-topbar-left">{left}</div>}
      {children}
      <div className="ui-topbar-spacer" />
      {right && <div className="ui-topbar-right">{right}</div>}
    </header>
  );
}

/**
 * The whole frame: navigation beside a column of bar-then-page.
 *
 * The page area is the only thing that scrolls. The application is exactly the
 * size of the window and every scrollable region inside it manages its own bar,
 * so the navigation and the top bar cannot slide away from a reader who is
 * halfway down a long table.
 */
export function AppShell({
  nav,
  header,
  children,
  /** the page manages its own edges AND its own scrolling: for a workspace
   *  built of panes that each scroll independently, which must not also sit
   *  inside a scroller or the whole frame slides and the navigation with it */
  flush = false,
}: {
  /** omit (or pass null) for a full-bleed view: a focus mode, a wizard */
  nav?: ReactNode;
  header?: ReactNode;
  children: ReactNode;
  flush?: boolean;
}) {
  return (
    <div className="ui-shell">
      {nav}
      <div className="ui-shell-main">
        {header}
        <main className={`ui-shell-page${flush ? " is-flush" : ""}`}>{children}</main>
      </div>
    </div>
  );
}
