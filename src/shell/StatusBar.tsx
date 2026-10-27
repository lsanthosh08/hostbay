import { useEffect, useState } from "react";
import * as api from "../api";
import type { SyncStatus } from "../api";
import { t } from "../i18n";
import { isRunning, useTunnelStates } from "../components/TunnelsPanel";
import { sessionAddress, connLabel, useSessions } from "../state/sessions";
import { StatusDot } from "../ui/StatusDot";
import { useTransferActions, useTransfersPanel } from "../state/transfers";
import { SyncIcon, TransfersIcon, TunnelIcon } from "../ui/icons";

export type SyncState = "off" | "on" | "paused" | "conflicts" | "error";

export function syncState(s: SyncStatus | null): SyncState {
  if (!s?.enabled) return "off";
  if (s.paused) return "paused";
  if (s.conflicts || s.holds.length) return "conflicts";
  return s.lastError ? "error" : "on";
}

const SYNC_LABEL = {
  off: "sync.off",
  on: "sync.on",
  paused: "sync.paused",
  conflicts: "sync.conflicts",
  error: "sync.error",
} as const;
export const syncLabel = (s: SyncState) => t(SYNC_LABEL[s]);

/** Sync's state, read again whenever `refresh` changes. Sync has no events,
 *  so it is also read once a minute, the backend's own sync interval. */
export function useSyncState(refresh: unknown): SyncState {
  const [status, setStatus] = useState<SyncStatus | null>(null);
  useEffect(() => {
    const read = () => api.syncStatus().then(setStatus).catch(() => {});
    read();
    const timer = setInterval(read, 60_000);
    return () => clearInterval(timer);
  }, [refresh]);
  return syncState(status);
}

const item = "inline-flex items-center gap-1.5 px-2 h-full hover:bg-hover hover:text-fg";

/** The bottom line: the active session, running tunnels, transfers (U17)
 *  and sync. */
export function StatusBar({ sync, onTunnels, onSync }: { sync: SyncState; onTunnels: () => void; onSync: () => void }) {
  const { active } = useSessions();
  const tunnels = Object.values(useTunnelStates()).filter(isRunning).length;
  const { open: transfersOpen, active: moving, waiting } = useTransfersPanel();
  const { setOpen: setTransfersOpen } = useTransferActions();
  return (
    <footer className="h-6 shrink-0 flex items-center text-xs text-muted border-t border-line panel">
      <span className="flex items-center gap-1.5 px-3 min-w-0">
        {active ? (
          <>
            <StatusDot status={active.status} />
            <span className="shrink-0">{connLabel(active)}</span>
            <span className="truncate font-mono">{sessionAddress(active)}</span>
          </>
        ) : (
          t("statusBar.noSession")
        )}
      </span>
      <span className="flex-1" />
      <button className={item} onClick={onTunnels}>
        <TunnelIcon className="size-3.5" aria-hidden />
        {t("statusBar.tunnels", { n: tunnels })}
      </button>
      {/* A task waiting on a conflict says so here until it is answered. */}
      <button
        className={`${item} ${waiting ? "text-warning" : ""}`}
        aria-expanded={transfersOpen}
        onClick={() => setTransfersOpen(!transfersOpen)}
      >
        <TransfersIcon className="size-3.5" aria-hidden />
        {waiting ? t("statusBar.transfersWaiting", { n: waiting }) : t("statusBar.transfers", { n: moving })}
      </button>
      <button className={item} onClick={onSync}>
        <SyncIcon className="size-3.5" aria-hidden />
        {syncLabel(sync)}
      </button>
    </footer>
  );
}
