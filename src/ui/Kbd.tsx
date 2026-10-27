import type { ReactNode } from "react";

/** A keyboard shortcut hint, e.g. <Kbd>⌘K</Kbd>. */
export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd className="inline-flex items-center rounded-chip border border-line bg-raised px-1.5 font-mono text-[0.6875rem] leading-4 text-muted">
      {children}
    </kbd>
  );
}
