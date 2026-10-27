import type { ButtonHTMLAttributes } from "react";
import type { Icon } from "./icons";

export const focusRing = "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus";

export function Button({
  variant = "default",
  size = "md",
  icon: Icon,
  className = "",
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "primary" | "danger" | "ghost";
  size?: "sm" | "md";
  /** Drawn before the label. */
  icon?: Icon;
}) {
  const base = `inline-flex items-center justify-center gap-1.5 rounded-control font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${focusRing}`;
  const sizes = { sm: "min-h-7 px-2 py-0.5 text-xs", md: "min-h-8 px-3 py-1 text-sm" }[size];
  const styles = {
    default: "bg-control-bg text-fg hover:bg-control-hover",
    primary: "bg-primary text-on-fill hover:bg-primary-hover",
    danger: "bg-danger-fill text-on-fill hover:bg-danger-fill-hover",
    ghost: "border border-control text-fg hover:bg-hover",
  }[variant];
  return (
    <button className={`${base} ${sizes} ${styles} ${className}`} {...props}>
      {Icon && <Icon className={size === "sm" ? "size-3.5" : "size-4"} aria-hidden />}
      {children}
    </button>
  );
}
