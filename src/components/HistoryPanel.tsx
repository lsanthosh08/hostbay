import { useEffect, useState } from "react";
import * as api from "../api";
import { t } from "../i18n";
import { PanelHeader } from "../shell/SidePanel";
import { activeTerminal, onActiveTerminal } from "../tabs/terminal/active";
import { EmptyState } from "../ui/EmptyState";
import { Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { DeleteIcon, HistoryIcon, RefreshIcon } from "../ui/icons";
import { usePrompt } from "../ui/prompt";
import { useToast } from "../ui/toast";
import { useSendToTerminal } from "./QuickCommands";

/** The command-history panel: the focused terminal's host's past commands,
 *  newest first, searched here and run in that terminal (R71, KTD39). */
export function HistoryPanel() {
  const { push } = useToast();
  const { confirm } = usePrompt();
  const send = useSendToTerminal();
  const [ptyId, setPtyId] = useState(() => activeTerminal()?.ptyId() ?? null);
  const [state, setState] = useState<api.TerminalState | null>(null);
  const [lines, setLines] = useState<string[]>([]);
  const [query, setQuery] = useState("");

  useEffect(() => onActiveTerminal(() => setPtyId(activeTerminal()?.ptyId() ?? null)), []);
  const load = () => {
    if (!ptyId) return setLines([]);
    api.terminalState(ptyId).then(setState, () => setState(null));
    api.terminalHistory(ptyId).then(
      (h) => setLines(h.reverse()),
      (e) => push("error", api.errorInfo(e).message)
    );
  };
  useEffect(load, [ptyId]); // eslint-disable-line react-hooks/exhaustive-deps

  const q = query.trim().toLowerCase();
  const shown = [...new Set(lines)].filter((l) => !q || l.toLowerCase().includes(q));

  return (
    <div className="flex flex-col min-h-0 flex-1">
      <PanelHeader title={t("nav.history")}>
        <IconButton size="sm" label={t("kubernetes.refresh")} icon={RefreshIcon} onClick={load} />
        <IconButton
          size="sm"
          tone="danger"
          label={t("history.clear")}
          icon={DeleteIcon}
          disabled={!ptyId || lines.length === 0}
          onClick={async () => {
            if (!ptyId || !(await confirm(t("history.clear")))) return;
            api.clearHistory(ptyId).then(load, (e) => push("error", api.errorInfo(e).message));
          }}
        />
      </PanelHeader>
      <div className="px-3 pb-2">
        <Input className="w-full" aria-label={t("history.search")} placeholder={t("history.search")} value={query} onChange={(e) => setQuery(e.target.value)} />
      </div>
      {!ptyId ? (
        <EmptyState icon={HistoryIcon} title={t("history.noTerminal")} />
      ) : state?.localEcho ? (
        <EmptyState icon={HistoryIcon} title={t("history.off")} />
      ) : shown.length === 0 ? (
        <EmptyState icon={HistoryIcon} title={t("history.empty")} />
      ) : (
        <ul className="flex-1 min-h-0 overflow-auto px-2 pb-2 flex flex-col">
          {shown.map((l) => (
            <li key={l}>
              <button
                type="button"
                title={t("quick.run")}
                onClick={() => send(l, "run")}
                className="w-full text-left font-mono text-xs px-2 py-1 rounded-control hover:bg-hover truncate"
              >
                {l}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
