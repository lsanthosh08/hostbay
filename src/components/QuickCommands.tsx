import { useEffect, useState } from "react";
import * as api from "../api";
import type { Script } from "../api";
import { t } from "../i18n";
import { PanelHeader } from "../shell/SidePanel";
import { sendToActive } from "../tabs/terminal/active";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { QuickCommandsIcon, RefreshIcon } from "../ui/icons";
import { useToast } from "../ui/toast";

/** Sends text to the focused terminal, or says why it cannot (R71). */
export function useSendToTerminal() {
  const { push } = useToast();
  return (text: string, mode: "run" | "paste") =>
    sendToActive(text, mode).then(
      (refused) => refused && push("error", t(refused)),
      (e) => push("error", api.errorInfo(e).message)
    );
}

/** The quick-commands panel: saved scripts (R11), run or pasted into the
 *  focused terminal, or copied. */
export function QuickCommands() {
  const { push } = useToast();
  const send = useSendToTerminal();
  const [scripts, setScripts] = useState<Script[]>([]);
  const [filter, setFilter] = useState("");
  const load = () => api.listScripts().then(setScripts).catch((e) => push("error", api.errorInfo(e).message));
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const q = filter.trim().toLowerCase();
  const shown = scripts.filter((s) => !q || s.label.toLowerCase().includes(q) || s.script.toLowerCase().includes(q));

  return (
    <div className="flex flex-col min-h-0 flex-1">
      <PanelHeader title={t("nav.quickCommands")}>
        <IconButton size="sm" label={t("kubernetes.refresh")} icon={RefreshIcon} onClick={load} />
      </PanelHeader>
      <div className="px-3 pb-2">
        <Input className="w-full" aria-label={t("quick.filter")} placeholder={t("quick.filter")} value={filter} onChange={(e) => setFilter(e.target.value)} />
      </div>
      {scripts.length === 0 ? (
        <EmptyState icon={QuickCommandsIcon} title={t("quick.empty")} />
      ) : (
        <ul className="flex-1 min-h-0 overflow-auto px-2 pb-2 flex flex-col gap-1">
          {shown.map((s) => (
            <li key={s.id} className="rounded-control px-2 py-1.5 hover:bg-hover flex flex-col gap-1">
              <span className="text-sm font-medium truncate">{s.label}</span>
              <code className="text-xs text-muted truncate allow-select" title={s.script}>
                {s.script}
              </code>
              <div className="flex gap-1">
                <Button size="sm" onClick={() => send(s.script, "run")}>
                  {t("quick.run")}
                </Button>
                <Button size="sm" variant="ghost" onClick={() => send(s.script, "paste")}>
                  {t("quick.paste")}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() =>
                    navigator.clipboard.writeText(s.script).then(
                      () => push("success", t("quick.copied")),
                      (e) => push("error", api.errorInfo(e).message)
                    )
                  }
                >
                  {t("quick.copy")}
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
