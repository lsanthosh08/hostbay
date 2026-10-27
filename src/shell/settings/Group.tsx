import type { ReactNode } from "react";

/** One titled block of a settings category, with an optional description. */
export function Group({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <div>
        <h3 className="text-sm font-semibold">{title}</h3>
        {description && <p className="text-sm text-muted mt-0.5 max-w-prose">{description}</p>}
      </div>
      {children}
    </section>
  );
}

/** A row in a settings list: what it is on the left, its actions on the right. */
export function Row({ children, actions }: { children: ReactNode; actions?: ReactNode }) {
  return (
    <li className="flex items-center gap-3 px-3 py-2 text-sm odd:bg-raised/60 rounded-control">
      <div className="flex-1 min-w-0">{children}</div>
      {actions && <div className="flex gap-1.5 shrink-0">{actions}</div>}
    </li>
  );
}
