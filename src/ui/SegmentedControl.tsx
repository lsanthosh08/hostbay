import type { KeyboardEvent, ReactNode } from "react";
import { focusRing } from "./Button";
import type { Icon } from "./icons";

export type Segment<T extends string> = {
  value: T;
  label: ReactNode;
  icon?: Icon;
  disabled?: boolean;
  title?: string;
};

/** One choice out of a few, side by side. Arrow keys move the choice. */
export function SegmentedControl<T extends string>({
  value,
  onChange,
  options,
  label,
  className = "",
}: {
  value: T;
  onChange: (v: T) => void;
  options: Segment<T>[];
  /** Accessible name of the group. */
  label: string;
  className?: string;
}) {
  const onKeyDown = (e: KeyboardEvent) => {
    const step = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const enabled = options.filter((o) => !o.disabled);
    const next = enabled[(enabled.findIndex((o) => o.value === value) + step + enabled.length) % enabled.length];
    if (!next) return;
    onChange(next.value);
    (e.currentTarget.querySelector(`[data-value="${next.value}"]`) as HTMLElement | null)?.focus();
  };
  return (
    <div
      role="radiogroup"
      aria-label={label}
      onKeyDown={onKeyDown}
      className={`inline-flex gap-0.5 rounded-control border border-control p-0.5 w-fit ${className}`}
    >
      {options.map((o) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={on}
            data-value={o.value}
            tabIndex={on ? 0 : -1}
            disabled={o.disabled}
            title={o.title}
            onClick={() => onChange(o.value)}
            className={
              `inline-flex items-center gap-1.5 px-3 py-1 rounded-chip text-sm whitespace-nowrap transition-colors disabled:opacity-30 disabled:cursor-not-allowed ${focusRing} ` +
              (on ? "bg-primary text-on-fill" : "text-muted hover:text-fg hover:bg-hover")
            }
          >
            {o.icon && <o.icon className="size-4" aria-hidden />}
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
