import { useEffect, useRef, useState } from "react";
import * as api from "../../api";
import type { DbTable, QueryState, UnlistenFn } from "../../api";
import { t } from "../../i18n";
import type { Connection } from "../../state/sessions";
import { Button } from "../../ui/Button";
import { Spinner } from "../../ui/kit";
import { useAsyncToast, useToast } from "../../ui/toast";
import { AskAI } from "./AskAI";
import { databaseCapabilities } from "./capabilities";
import { DocumentView } from "./DocumentView";
import { ObjectTree } from "./ObjectTree";
import { QueryEditor } from "./QueryEditor";
import { ResultGrid } from "./ResultGrid";
import { addPage, type ResultSet } from "./rows";

/** The request on screen: a query's text, or a table opened from the tree. */
type Shown = {
  queryId: string;
  text?: string;
  path?: string[];
  table: DbTable | null;
  sets: ResultSet[];
  state?: QueryState;
  done: boolean;
  loading: boolean;
  error?: string;
};

// A page with no rows moves to the next result set; this many in a row is
// a request of empty statements, shown as they are.
const MAX_EMPTY_PAGES = 50;

/**
 * A database host's tab (R56): the object tree, the query editor and the
 * results. The session ID is the databaseId; rows come a page at a time and
 * the row editor shows only when the engine offers it (KTD23).
 */
export function DatabaseTab({ conn }: { conn: Connection }) {
  const databaseId = conn.sessionId;
  const caps = databaseCapabilities.get(databaseId) ?? [];
  const canEdit = caps.includes("rowEdit");
  const canEditDocs = caps.includes("documentEdit");
  const canAsk = caps.includes("askSql");
  const [insert, setInsert] = useState<{ sql: string; n: number }>();
  const { push } = useToast();
  const toast = useAsyncToast();
  const [text, setText] = useState("");
  const [shown, setShown] = useState<Shown | null>(null);
  const [doc, setDoc] = useState<{ path: string[]; id: string } | null>(null);
  const unlisten = useRef<UnlistenFn | null>(null);
  const current = useRef<string | null>(null);
  // Bumped when a request is dropped: an open still on its way when a newer
  // one starts, or the tab closes, closes its query instead of showing it.
  const seq = useRef(0);

  const forget = () => {
    seq.current++;
    unlisten.current?.();
    unlisten.current = null;
    if (current.current) api.databaseCloseQuery(current.current).catch(() => {});
    current.current = null;
  };
  useEffect(() => forget, []);

  const update = (queryId: string, f: (s: Shown) => Shown) => setShown((s) => (s && s.queryId === queryId ? f(s) : s));

  const fetchPage = async (queryId: string) => {
    update(queryId, (s) => ({ ...s, loading: true }));
    try {
      for (let empty = 0; ; empty++) {
        const p = await api.databaseNextPage(queryId);
        update(queryId, (s) => ({ ...s, sets: addPage(s.sets, p), done: p.done }));
        if (p.done || p.rows.length || empty >= MAX_EMPTY_PAGES) break;
      }
      update(queryId, (s) => ({ ...s, loading: false }));
    } catch (e) {
      update(queryId, (s) => ({ ...s, loading: false, error: api.errorInfo(e).message }));
    }
  };

  const start = async (open: () => Promise<{ queryId: string; table: DbTable | null }>, what: { text?: string; path?: string[] }) => {
    forget();
    const mine = seq.current;
    setShown({ queryId: "", table: null, sets: [], done: false, loading: true, ...what });
    try {
      const { queryId, table } = await open();
      if (mine !== seq.current) return void api.databaseCloseQuery(queryId).catch(() => {});
      current.current = queryId;
      setShown({ queryId, table: canEdit || canEditDocs ? table : null, sets: [], done: false, loading: true, ...what });
      const off = await api.onQueryState(queryId, (st) => update(queryId, (s) => ({ ...s, state: st })));
      if (current.current === queryId) unlisten.current = off;
      else off();
      await fetchPage(queryId);
    } catch (e) {
      if (mine === seq.current) setShown((s) => s && { ...s, loading: false, error: api.errorInfo(e).message });
    }
  };

  const runQuery = (q: string) => start(() => api.databaseQuery(databaseId, q).then((queryId) => ({ queryId, table: null })), { text: q });
  const openTable = (path: string[]) => start(() => api.databaseOpenTable(databaseId, path), { path });
  const rerun = () => shown && (shown.path ? openTable(shown.path) : shown.text && runQuery(shown.text));

  const state = shown?.state?.state;
  const running = !!shown && (shown.loading || state === "running") && !shown.error && state !== "cancelled";
  const rows = shown?.sets.reduce((n, s) => n + s.rows.length, 0) ?? 0;

  return (
    <div className="flex flex-1 h-full min-h-0">
      <aside className="w-64 shrink-0 border-r border-line min-h-0">
        <ObjectTree databaseId={databaseId} onOpen={openTable} />
      </aside>
      <div className="flex-1 min-w-0 flex flex-col min-h-0">
        {canAsk && <AskAI databaseId={databaseId} onSQL={(sql) => setInsert((x) => ({ sql, n: (x?.n ?? 0) + 1 }))} />}
        <div className="h-2/5 min-h-32 border-b border-line">
          <QueryEditor
            insert={insert}
            value={text}
            onChange={setText}
            onRun={runQuery}
            running={running}
            onCancel={() => shown?.queryId && api.databaseCancel(shown.queryId).catch(() => {})}
          />
        </div>
        <div role="status" aria-live="polite" className="flex flex-wrap items-center gap-2 px-2 py-1 text-xs border-b border-line min-h-8">
          {running && <Spinner />}
          {shown && !shown.error && <span className="text-muted">{t(rows === 1 ? "database.row" : "database.rows", { n: rows })}</span>}
          {state === "cancelled" && <span className="text-muted">{t("database.cancelled")}</span>}
          {shown?.error && <span className="text-danger allow-select break-words">{shown.error}</span>}
          {state === "expired" && shown?.state?.rerun && (
            <Button size="sm" onClick={rerun}>
              {t("database.rerun")}
            </Button>
          )}
          {shown?.path && !shown.table && !shown.loading && !shown.error && canEdit && (
            <span className="text-muted">{t("database.readOnly")}</span>
          )}
          {shown && !shown.done && !shown.loading && !shown.error && state !== "expired" && state !== "cancelled" && (
            <Button size="sm" onClick={() => fetchPage(shown.queryId)}>
              {t("database.loadMore")}
            </Button>
          )}
        </div>
        <div className="flex-1 min-h-0">
          {shown && (
            <ResultGrid
              key={shown.queryId}
              sets={shown.sets}
              table={shown.table}
              onSaveCell={(result, row, col) =>
                toast(() => api.databaseSaveCell(shown.queryId, result, row, col)).then((saved) => saved && push("success", t("database.saved")))
              }
              onEditRow={async (key, changes) => {
                if (!shown.table) return false;
                try {
                  await api.databaseEditRow(databaseId, shown.table.path, key, changes);
                } catch (e) {
                  push("error", api.errorInfo(e).message);
                  return false;
                }
                push("success", t("database.edited"));
                openTable(shown.table.path);
                return true;
              }}
              onEditDocument={canEditDocs && shown.table ? (id) => setDoc({ path: shown.table!.path, id }) : undefined}
            />
          )}
        </div>
      </div>
      {doc && (
        <DocumentView
          databaseId={databaseId}
          path={doc.path}
          id={doc.id}
          onClose={() => setDoc(null)}
          onSaved={() => {
            setDoc(null);
            push("success", t("database.documentSaved"));
            openTable(doc.path);
          }}
        />
      )}
    </div>
  );
}
