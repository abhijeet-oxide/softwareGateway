import type { CSSProperties, ReactNode } from "react";

// StatTile is the one number-on-a-card in the system: a quiet label, a strong
// value, an optional sub-line and leading icon. A clickable tile lifts under
// the pointer and goes to where the number came from - a number nobody can
// open is a number nobody can check.
export default function StatTile({
  label,
  value,
  sub,
  icon,
  onClick,
  style,
}: {
  label: ReactNode;
  value: ReactNode;
  sub?: ReactNode;
  icon?: ReactNode;
  onClick?: () => void;
  style?: CSSProperties;
}) {
  return (
    <div
      className={`ui-stat${onClick ? " is-clickable" : ""}`}
      onClick={onClick}
      onKeyDown={
        onClick
          ? (e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                onClick();
              }
            }
          : undefined
      }
      role={onClick ? "button" : undefined}
      tabIndex={onClick ? 0 : undefined}
      style={style}
    >
      {icon && <div className="ui-stat-icon">{icon}</div>}
      <div className="ui-stat-main">
        <div className="ui-stat-label">{label}</div>
        <div className="ui-stat-value ui-num">{value}</div>
        {sub && <div className="ui-stat-sub">{sub}</div>}
      </div>
    </div>
  );
}
