import { useEffect, useRef, useState } from "react";
import * as api from "../../api";
import type { UnlistenFn } from "../../api";
import { t } from "../../i18n";
import { Dialog } from "../../ui/Dialog";

// The view keeps the last lines only, so a chatty pod cannot grow it forever.
export const MAX_LINES = 5000;

/** Adds a chunk of log text to lines and the unfinished last line,
 *  keeping at most MAX_LINES. */
export function addLog(lines: string[], partial: string, chunk: string): { lines: string[]; partial: string } {
  const parts = (partial + chunk).split("\n");
  const rest = parts.pop()!;
  if (!parts.length) return { lines, partial: rest };
  const next = lines.concat(parts);
  return { lines: next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next, partial: rest };
}

/**
 * A pod container's followed log, as text (R57). Go streams it through the
 * terminal registry's emit loop (KTD9); closing the view ends the stream.
 */
export function LogView({
  clusterId,
  namespace,
  pod,
  container,
  onClose,
}: {
  clusterId: string;
  namespace: string;
  pod: string;
  container: string;
  onClose: () => void;
}) {
  const [log, setLog] = useState<{ lines: string[]; partial: string }>({ lines: [], partial: "" });
  const [ended, setEnded] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const pre = useRef<HTMLPreElement>(null);

  useEffect(() => {
    let disposed = false;
    let ptyId: string | null = null;
    const offs: UnlistenFn[] = [];
    (async () => {
      try {
        const id = await api.kubernetesLogs(clusterId, namespace, pod, container);
        if (disposed) return void api.ptyClose(id).catch(() => {});
        ptyId = id;
        // A listener that arrives after the view closed is removed at once:
        // the cleanup that removes the others has already run.
        const keep = (off: UnlistenFn) => (disposed ? off() : offs.push(off));
        keep(await api.onPtyData(id, (d) => setLog((l) => addLog(l.lines, l.partial, d))));
        keep(await api.onPtyClosed(id, (reason) => setEnded(reason)));
        if (disposed) return;
        await api.ptyAttach(id);
      } catch (e) {
        if (!disposed) setError(api.errorInfo(e).message);
      }
    })();
    return () => {
      disposed = true;
      offs.forEach((off) => off());
      if (ptyId) api.ptyClose(ptyId).catch(() => {});
    };
  }, [clusterId, namespace, pod, container]);

  useEffect(() => {
    const el = pre.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [log]);

  return (
    <Dialog title={t("kubernetes.logTitle", { name: `${pod}/${container}` })} onClose={onClose} size="full" bodyClassName="flex flex-col min-h-0 flex-1 p-0">
      {error && <p className="text-sm text-danger allow-select px-4 py-2">{error}</p>}
      <pre ref={pre} className="flex-1 min-h-0 overflow-auto px-4 py-2 text-xs font-mono whitespace-pre-wrap break-all allow-select">
        {log.lines.join("\n")}
        {log.lines.length && log.partial ? "\n" : ""}
        {log.partial}
      </pre>
      {ended && <p className="text-xs text-muted px-4 py-2 border-t border-line">{t("kubernetes.logEnded", { reason: ended })}</p>}
    </Dialog>
  );
}
