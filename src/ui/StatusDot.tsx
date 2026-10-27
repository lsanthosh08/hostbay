import { t } from "../i18n";

export type ConnStatus = "connecting" | "live" | "error";

const look = {
  live: ["bg-success", "status.live"],
  connecting: ["bg-warning animate-pulse", "status.connecting"],
  error: ["bg-danger", "status.error"],
  idle: ["bg-muted/40", "status.idle"],
} as const;

/** The words for a status, as screen readers hear the dot. */
export const statusLabel = (status?: ConnStatus) => t(look[status ?? "idle"][1]);

/** Shared status dot. undefined = not connected (dim). */
export function StatusDot({ status }: { status?: ConnStatus }) {
  const [cls, label] = look[status ?? "idle"];
  return <span role="img" aria-label={t(label)} className={`size-2 rounded-full shrink-0 ${cls}`} />;
}
