import { useEffect, useState } from "react";
import * as api from "../../api";
import type { DbSavedQuery } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { DeleteIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { Tabs } from "../../ui/Tabs";
import { useToast } from "../../ui/toast";
import { firstLine, matching } from "./library";

type View = "history" | "saved";

/**
 * The host's request history (newest first) and its saved queries. Use
 * puts a request into the editor in place of the selection, as Ask AI
 * does, so nothing typed is lost and nothing runs until Run.
 */
export function QueryLibrary({
  databaseId,
  current,
  onUse,
  onClose,
}: {
  databaseId: string;
  /** The editor's text, offered to Save. */
  current: string;
  onUse: (text: string) => void;
  onClose: () => void;
}) {
  const [view, setView] = useState<View>("history");
  const [history, setHistory] = useState<string[] | null>(null);
  const [saved, setSaved] = useState<DbSavedQuery[] | null>(null);
  const [filter, setFilter] = useState("");
  const { prompt, confirm } = usePrompt();
  const { push } = useToast();
  const fail = (e: unknown) => push("error", api.errorInfo(e).message);
  useEffect(() => {
    api.databaseHistory(databaseId).then((h) => setHistory(h.slice().reverse()), fail);
    api.databaseSavedQueries(databaseId).then(setSaved, fail);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [databaseId]);

  const save = async () => {
    const name = await prompt(t("database.saveQueryName"), firstLine(current).slice(0, 60));
    if (name?.trim()) api.databaseSaveQuery(databaseId, name, current).then(setSaved, fail);
  };
  const use = (text: string) => {
    onUse(text);
    onClose();
  };
  const rows =
    view === "history"
      ? matching(history ?? [], filter, (q) => q).map((q, i) => ({ key: `${i}`, name: firstLine(q), text: q }))
      : matching(saved ?? [], filter, (q) => q.name + "\n" + q.text).map((q) => ({ key: q.name, name: q.name, text: q.text }));
  const loading = view === "history" ? history === null : saved === null;

  return (
    <Dialog
      title={t("database.library")}
      size="xl"
      onClose={onClose}
      footer={
        <>
          {view === "history" ? (
            <Button
              variant="ghost"
              disabled={!history?.length}
              onClick={async () => {
                if (await confirm(t("database.clearHistoryConfirm"), { confirmLabel: t("database.clearHistory") }))
                  api.databaseClearHistory(databaseId).then(() => setHistory([]), fail);
              }}
            >
              {t("database.clearHistory")}
            </Button>
          ) : (
            <Button disabled={!current.trim()} onClick={save}>
              {t("database.saveQuery")}
            </Button>
          )}
          <Button variant="primary" onClick={onClose}>
            {t("common.close")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-2">
        <Tabs<View>
          label={t("database.library")}
          value={view}
          onChange={setView}
          items={[
            { id: "history", label: t("database.history") },
            { id: "saved", label: t("database.savedQueries") },
          ]}
        />
        <Input aria-label={t("database.libraryFilter")} placeholder={t("database.libraryFilter")} value={filter} onChange={(e) => setFilter(e.target.value)} />
        {!loading && !rows.length && (
          <p className="text-sm text-muted py-2">{view === "history" ? t("database.historyEmpty") : t("database.savedEmpty")}</p>
        )}
        <ul className="flex flex-col divide-y divide-line">
          {rows.map((r) => (
            <li key={r.key} className="flex items-center gap-2 py-1.5 min-w-0">
              <div className="flex-1 min-w-0">
                <div className="text-sm truncate" title={r.text}>
                  {r.name}
                </div>
                {view === "saved" && <div className="text-xs text-muted font-mono truncate">{firstLine(r.text)}</div>}
              </div>
              <Button size="sm" onClick={() => use(r.text)}>
                {t("database.useQuery")}
              </Button>
              {view === "saved" && (
                <IconButton
                  size="sm"
                  icon={DeleteIcon}
                  label={t("database.deleteQuery", { name: r.name })}
                  onClick={async () => {
                    if (await confirm(t("database.deleteQueryConfirm", { name: r.name }), { confirmLabel: t("database.deleteSaved") }))
                      api.databaseDeleteQuery(databaseId, r.name).then(setSaved, fail);
                  }}
                />
              )}
            </li>
          ))}
        </ul>
      </div>
    </Dialog>
  );
}
