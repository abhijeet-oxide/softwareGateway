import type { ReactNode } from "react";

// PageHeader standardizes the top block of every screen: a strong title, an
// optional description in secondary text, and a right-aligned actions slot.
// Every page in both tools opens the same way, which is most of what makes
// them feel like one product.
export default function PageHeader({
  title,
  description,
  actions,
  children,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  /** optional extra row under the title (context chips, filter tabs) */
  children?: ReactNode;
}) {
  return (
    <div className="ui-page-header">
      <div className="ui-page-header-row">
        <div className="ui-page-header-main">
          <div className="ui-page-header-title">{title}</div>
          {description && <div className="ui-page-header-desc">{description}</div>}
        </div>
        {actions && <div className="ui-page-header-actions">{actions}</div>}
      </div>
      {children}
    </div>
  );
}
