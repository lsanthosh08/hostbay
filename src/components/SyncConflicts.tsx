import { useCallback, useEffect, useState } from "react";
import { errorInfo, syncConflicts, syncDismissConflict, syncRestoreConflict, type SyncConflict } from "../api";
import { t, type MessageKey } from "../i18n";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";

const what: Record<string, MessageKey> = {
  hosts: "sync.what.host",
  folders: "sync.what.folder",
  keys: "sync.what.key",
  scripts: "sync.what.script",
  bookmarks: "sync.what.bookmark",
  tunnels: "sync.what.tunnel",
};

/** Versions that lost when two computers changed the same item (R37). */
export function SyncConflicts({ onChanged }: { onChanged?: () => void }) {
  const [items, setItems] = useState<SyncConflict[]>([]);
  const [error, setError] = useState("");

  const load = useCallback(() => {
    syncConflicts()
      .then(setItems)
      .catch((e) => setError(errorInfo(e).message));
  }, []);
  useEffect(load, [load]);

  const act = async (fn: () => Promise<void>) => {
    setError("");
    try {
      await fn();
      load();
      onChanged?.();
    } catch (e) {
      setError(errorInfo(e).message);
    }
  };

  if (items.length === 0) return <EmptyState>{t("sync.conflict.none")}</EmptyState>;
  return (
    <div className="flex flex-col gap-2">
      {error && <p className="text-sm text-danger">{error}</p>}
      <ul className="flex flex-col gap-1">
        {items.map((c) => (
          <li key={c.id} className="flex items-center gap-2 text-sm rounded border border-line px-3 py-2">
            <div className="flex-1 min-w-0">
              <div className="truncate">
                {what[c.collection] ? t(what[c.collection]) : c.collection} <b>{c.label || c.itemId}</b>
              </div>
              <div className="text-xs opacity-60">
                {t(c.kind === "purge" ? "sync.conflict.deleted" : "sync.conflict.changed", {
                  device: c.deviceId.slice(0, 8),
                  when: new Date(c.modifiedAt).toLocaleString(),
                })}
              </div>
            </div>
            <Button onClick={() => act(() => syncRestoreConflict(c.id))} title={t("sync.conflict.restoreHint")}>
              {t(c.kind === "purge" ? "sync.conflict.delete" : "sync.conflict.restore")}
            </Button>
            <Button variant="ghost" onClick={() => act(() => syncDismissConflict(c.id))}>
              {t("common.dismiss")}
            </Button>
          </li>
        ))}
      </ul>
    </div>
  );
}
