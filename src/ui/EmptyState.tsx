import type { ReactNode } from "react";
import type { Icon } from "./icons";

/** What an empty list or view shows: what is missing and what to do next. */
export function EmptyState({
  icon: Icon,
  title,
  actions,
  children,
}: {
  icon?: Icon;
  title?: ReactNode;
  actions?: ReactNode;
  /** The description. */
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-1.5 py-10 text-center">
      {Icon && <Icon className="size-6 text-muted mb-1" aria-hidden />}
      {title && <p className="text-sm font-medium">{title}</p>}
      {children && <p className="text-sm text-muted max-w-sm">{children}</p>}
      {actions && <div className="flex gap-2 mt-2">{actions}</div>}
    </div>
  );
}
