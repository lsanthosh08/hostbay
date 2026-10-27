import type { ButtonHTMLAttributes } from "react";
import { focusRing } from "./Button";
import type { Icon } from "./icons";

/** An icon-only button. `label` is its accessible name and tooltip. */
export function IconButton({
  label,
  icon: Icon,
  tone = "default",
  size = "md",
  className = "",
  ...props
}: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> & {
  label: string;
  icon: Icon;
  tone?: "default" | "accent" | "danger";
  size?: "sm" | "md";
}) {
  const tones = {
    default: "text-muted hover:text-fg hover:bg-hover",
    accent: "text-accent hover:bg-accent/15",
    danger: "text-danger hover:bg-danger/15",
  }[tone];
  return (
    <button
      type="button"
      title={label}
      aria-label={label}
      className={`inline-flex shrink-0 items-center justify-center rounded-control transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${focusRing} ${size === "sm" ? "size-6" : "size-7"} ${tones} ${className}`}
      {...props}
    >
      <Icon className={size === "sm" ? "size-3.5" : "size-4"} aria-hidden />
    </button>
  );
}
