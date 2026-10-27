import type { ReactNode } from "react";

export function Panel({
  title,
  actions,
  children,
  className = "",
  bodyClassName = "p-4",
}: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  /** Override body padding/layout. Pass "flex-1 min-h-0" for fill+scroll panels. */
  bodyClassName?: string;
}) {
  return (
    <div className={`panel rounded-card flex flex-col min-h-0 ${className}`}>
      {(title || actions) && (
        <div className="px-4 py-3 border-b border-line flex items-center justify-between shrink-0">
          <h2 className="font-semibold text-sm">{title}</h2>
          <div className="flex items-center gap-2">{actions}</div>
        </div>
      )}
      <div className={bodyClassName}>{children}</div>
    </div>
  );
}

export function Spinner() {
  return (
    <span className="inline-block size-4 border-2 border-current/30 border-t-current rounded-full animate-spin" />
  );
}

export function Badge({
  tone = "neutral",
  children,
}: {
  tone?: "neutral" | "ok" | "bad" | "warn";
  children: ReactNode;
}) {
  const cls = {
    neutral: "bg-control-bg text-fg",
    ok: "bg-success-soft text-success",
    bad: "bg-danger-soft text-danger",
    warn: "bg-warning-soft text-warning",
  }[tone];
  return <span className={`px-2 py-0.5 rounded-full text-xs ${cls}`}>{children}</span>;
}
