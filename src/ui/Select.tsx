import type { SelectHTMLAttributes } from "react";
import { controlCls, useFieldControl } from "./Field";

/** The native select, styled like Input. `compact` fits a toolbar row. */
export function Select({
  compact = false,
  className = "",
  ...props
}: SelectHTMLAttributes<HTMLSelectElement> & { compact?: boolean }) {
  return (
    <select
      {...useFieldControl(props)}
      {...props}
      className={`${compact ? "h-7 px-2 text-xs" : "h-9 px-3"} ${controlCls} ${className}`}
    />
  );
}
