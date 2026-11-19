import { useEffect, useRef, useState } from "react";
import * as api from "../../api";
import type { DbBrowse, DbColumn, DbTable, UnlistenFn } from "../../api";
import { t } from "../../i18n";
import type { Connection } from "../../state/sessions";
import { Button } from "../../ui/Button";
import { IconButton } from "../../ui/IconButton";
import { AddIcon } from "../../ui/icons";
import { Spinner } from "../../ui/kit";
import { usePrompt } from "../../ui/prompt";
import { useAsyncToast, useToast } from "../../ui/toast";
import { AskAI } from "./AskAI";
import { nextSort } from "./browse";
import { BrowseBar } from "./BrowseBar";
import { databaseCapabilities } from "./capabilities";
import { DocumentView } from "./DocumentView";
import { ObjectTree } from "./ObjectTree";
import { QueryEditor } from "./QueryEditor";
import { QueryLibrary } from "./QueryLibrary";
import { firstLine } from "./library";
import { ResultGrid } from "./ResultGrid";
import { InsertDialog, ReviewDialog, type Pending } from "./RowChange";
import { addPage } from "./rows";
import { cancelled, clip, duration, failed, isRunning, type Shown } from "./shown";
import { addTab, closeTab, completions, renameTab, setTabText, type Completion, type QueryTab } from "./tabs";
import { Tabs } from "../../ui/Tabs";

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
  const canBrowse = caps.includes("browse");
  const canExplain = caps.includes("explain");
  // Row changes Go can show before running go through a review step.
  const canWrite = caps.includes("rowWrite");
  const [pending, setPending] = useState<Pending | null>(null);
  const [inserting, setInserting] = useState(false);
  const [insert, setInsert] = useState<{ sql: string; n: number }>();
  const { push } = useToast();
  const toast = useAsyncToast();
  const { confirm, prompt } = usePrompt();
  const [readOnly, setReadOnly] = useState(false);
  const [library, setLibrary] = useState(false);
  const putInEditor = (sql: string) => setInsert((x) => ({ sql, n: (x?.n ?? 0) + 1 }));
  // The export running now; cancelling its promise stops it in Go.
  const [exporting, setExporting] = useState<{ cancel: () => void } | null>(null);
  // Query tabs share the session and the results area below them.
  const [tabs, setTabs] = useState<QueryTab[]>(() => addTab([], (n) => t("database.queryTab", { n })).tabs);
  const [activeTab, setActiveTab] = useState("1");
  const tab = tabs.find((x) => x.id === activeTab) ?? tabs[0];
  const text = tab.text;
  const setText = (v: string) => setTabs((ts) => setTabText(ts, tab.id, v));
  const [schema, setSchema] = useState<Completion[]>([]);
  useEffect(() => {
    if (canAsk)
      api.databaseSchema(databaseId).then(
        (s) => setSchema(completions(s)),
        () => {}
      );
  }, [databaseId, canAsk]);
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
        update(queryId, (s) => ({ ...s, sets: addPage(s.sets, p), done: p.done, took: s.took ?? Date.now() - (s.started ?? Date.now()) }));
        if (p.done || p.rows.length || empty >= MAX_EMPTY_PAGES) break;
      }
      update(queryId, (s) => ({ ...s, loading: false }));
    } catch (e) {
      update(queryId, (s) => failed(s, api.errorInfo(e)));
    }
  };

  const start = async (open: () => Promise<{ queryId: string; table: DbTable | null }>, what: Pick<Shown, "text" | "explain" | "path" | "browse">) => {
    forget();
    const mine = seq.current;
    const started = Date.now();
    setShown({ queryId: "", table: null, sets: [], done: false, loading: true, started, ...what });
    try {
      const { queryId, table } = await open();
      if (mine !== seq.current) return void api.databaseCloseQuery(queryId).catch(() => {});
      current.current = queryId;
      setShown({ queryId, table: canEdit || canEditDocs ? table : null, sets: [], done: false, loading: true, started, ...what });
      const off = await api.onQueryState(queryId, (st) => update(queryId, (s) => ({ ...s, state: st })));
      if (current.current === queryId) unlisten.current = off;
      else off();
      await fetchPage(queryId);
    } catch (e) {
      if (mine === seq.current) setShown((s) => s && failed(s, api.errorInfo(e)));
    }
  };

  const runQuery = (q: string) => start(() => api.databaseQuery(databaseId, q).then((queryId) => ({ queryId, table: null })), { text: q });
  // Go plans the statement without running it, so no confirm is asked.
  const explain = (q: string) => start(() => api.databaseExplain(databaseId, q).then((queryId) => ({ queryId, table: null })), { text: q, explain: true });
  // A request Go does not judge read-only runs only once confirmed, and not
  // at all in read-only mode, where Go would refuse it anyway.
  const run = async (q: string) => {
    if (!(await api.databaseReadOnly(databaseId, q).catch(() => false))) {
      if (readOnly) return push("error", t("database.readOnlyRefused"));
      if (!(await confirm(t("database.confirmWrite", { text: clip(q) }), { confirmLabel: t("database.runAnyway") }))) return;
    }
    runQuery(q);
  };
  const setMode = (on: boolean) =>
    api.databaseSetReadOnly(databaseId, on).then(
      () => setReadOnly(on),
      (e) => push("error", api.errorInfo(e).message)
    );
  const openTable = (path: string[], browse: DbBrowse = {}) => start(() => api.databaseOpenTable(databaseId, path, browse), { path, browse });
  const count = (s: Shown) => {
    update(s.queryId, (x) => ({ ...x, count: null }));
    api.databaseCountRows(databaseId, s.path!, s.browse ?? {}).then(
      (n) => update(s.queryId, (x) => ({ ...x, count: n })),
      (e) => {
        update(s.queryId, (x) => ({ ...x, count: undefined }));
        push("error", api.errorInfo(e).message);
      }
    );
  };
  // The table's columns, kept across its browses: a browse that fails
  // returns none, and the filters still need them.
  const tableCols = useRef<{ path: string; columns: DbColumn[] }>({ path: "", columns: [] });
  const where = JSON.stringify(shown?.path ?? null);
  if (shown?.path && shown.sets[0]?.columns.length) tableCols.current = { path: where, columns: shown.sets[0].columns };
  const columns = tableCols.current.path === where ? tableCols.current.columns : [];
  // A request still opening has no query ID to cancel yet: drop it, so the
  // query it starts is closed as it arrives.
  const cancel = () => {
    if (shown?.queryId) return void api.databaseCancel(shown.queryId).catch(() => {});
    forget();
    setShown((s) => s && cancelled(s));
  };
  const exportAs = (s: Shown, format: "csv" | "json") => {
    const p = api.databaseExport(databaseId, { text: s.path ? undefined : s.text, path: s.path, browse: s.browse ?? {}, format });
    setExporting({ cancel: () => p.cancel() });
    p.then(
      (r) => r?.saved && push("success", t("database.exported", { n: r.rows.toLocaleString(), path: r.path ?? "" })),
      (e) => api.errorInfo(e).details?.category !== "cancelled" && push("error", api.errorInfo(e).message)
    ).finally(() => setExporting(null));
  };
  const rerun = () => shown && (shown.path ? openTable(shown.path, shown.browse) : shown.text && (shown.explain ? explain(shown.text) : runQuery(shown.text)));

  const state = shown?.state?.state;
  // While a request runs, the status line counts up a few times a second.
  const [now, setNow] = useState(Date.now());
  const ticking = isRunning(shown) && shown?.took === undefined;
  useEffect(() => {
    if (!ticking) return;
    const id = setInterval(() => setNow(Date.now()), 250);
    return () => clearInterval(id);
  }, [ticking]);
  const elapsed = shown?.took ?? (ticking && shown?.started ? now - shown.started : undefined);
  const running = isRunning(shown);
  const rows = shown?.sets.reduce((n, s) => n + s.rows.length, 0) ?? 0;

  return (
    <div className="flex flex-1 h-full min-h-0">
      <aside className="w-64 shrink-0 border-r border-line min-h-0">
        <ObjectTree databaseId={databaseId} onOpen={openTable} />
      </aside>
      <div className="flex-1 min-w-0 flex flex-col min-h-0">
        {canAsk && <AskAI databaseId={databaseId} onSQL={putInEditor} />}
        <div className="h-2/5 min-h-32 border-b border-line flex flex-col">
          <div className="flex items-center gap-1 px-2 border-b border-line text-xs">
            <Tabs
              label={t("database.queryTabs")}
              className="min-w-0"
              value={tab.id}
              onChange={setActiveTab}
              items={tabs.map((x) => ({
                id: x.id,
                label: (
                  <span
                    onDoubleClick={async () => {
                      const name = await prompt(t("database.renameTab"), x.name);
                      if (name !== null && name !== undefined) setTabs((ts) => renameTab(ts, x.id, name));
                    }}
                  >
                    {x.name}
                  </span>
                ),
                title: t("database.renameTabHint"),
                onClose:
                  tabs.length > 1
                    ? () => {
                        const next = closeTab(tabs, x.id, tab.id);
                        setTabs(next.tabs);
                        setActiveTab(next.active);
                      }
                    : undefined,
              }))}
            />
            <IconButton
              size="sm"
              icon={AddIcon}
              label={t("database.newTab")}
              onClick={() => {
                const next = addTab(tabs, (n) => t("database.queryTab", { n }));
                setTabs(next.tabs);
                setActiveTab(next.id);
              }}
            />
          </div>
          <div className="flex-1 min-h-0">
            <QueryEditor
              path={`database/${databaseId}/${tab.id}.sql`}
              schema={schema}
              insert={insert}
              value={text}
              onChange={setText}
              onRun={run}
              onExplain={canExplain ? explain : undefined}
              readOnly={readOnly}
              onReadOnly={setMode}
              onLibrary={() => setLibrary(true)}
              running={running}
              onCancel={cancel}
            />
          </div>
        </div>
        <div role="status" aria-live="polite" className="flex flex-wrap items-center gap-2 px-2 py-1 text-xs border-b border-line min-h-8">
          {running && <Spinner />}
          {shown && (shown.path || shown.text) && (
            <span
              className="text-muted truncate max-w-xs font-mono"
              title={t("database.resultsOf", { what: shown.path ? shown.path.join(".") : clip(shown.text ?? "") })}
            >
              {shown.explain && t("database.planOf")} {shown.path ? shown.path.join(".") : firstLine(shown.text ?? "")}
            </span>
          )}
          {shown && !shown.error && <span className="text-muted">{t(rows === 1 ? "database.row" : "database.rows", { n: rows })}</span>}
          {elapsed !== undefined && <span className="text-muted tabular-nums">{t("database.took", { time: duration(Math.max(0, elapsed)) })}</span>}
          {state === "cancelled" && <span className="text-muted">{t("database.cancelled")}</span>}
          {shown?.error && <span className="text-danger allow-select break-words">{shown.error}</span>}
          {state === "expired" && shown?.state?.rerun && (
            <Button size="sm" onClick={rerun}>
              {t("database.rerun")}
            </Button>
          )}
          {readOnly && <span className="text-warning">{t("database.readOnlyOn")}</span>}
          {shown?.path && !shown.table && !shown.loading && !shown.error && canEdit && (
            <span className="text-muted">{t("database.readOnly")}</span>
          )}
          {shown && !shown.done && !shown.loading && !shown.error && state !== "expired" && state !== "cancelled" && (
            <Button size="sm" onClick={() => fetchPage(shown.queryId)}>
              {t("database.loadMore")}
            </Button>
          )}
          {shown && !shown.error && !shown.explain && (shown.path || shown.text) && (
            <span className="ml-auto flex items-center gap-1">
              {exporting ? (
                <>
                  <Spinner />
                  <span className="text-muted">{t("database.exporting")}</span>
                  <Button size="sm" variant="ghost" onClick={exporting.cancel}>
                    {t("database.cancel")}
                  </Button>
                </>
              ) : (
                <>
                  <span className="text-muted">{t("database.export")}</span>
                  <Button size="sm" variant="ghost" title={t("database.exportHint")} onClick={() => exportAs(shown, "csv")}>
                    CSV
                  </Button>
                  <Button size="sm" variant="ghost" title={t("database.exportHint")} onClick={() => exportAs(shown, "json")}>
                    JSON
                  </Button>
                </>
              )}
            </span>
          )}
        </div>
        {shown?.path && canBrowse && (
          <BrowseBar
            key={where}
            columns={columns}
            browse={shown.browse ?? {}}
            onApply={(b) => openTable(shown.path!, b)}
            onRefresh={rerun}
            count={shown.count}
            onCount={() => count(shown)}
            json={caps.includes("browseJSON")}
            onInsert={canWrite && shown.table && !readOnly ? () => setInserting(true) : undefined}
          />
        )}
        <div className="flex-1 min-h-0">
          {shown && (
            <ResultGrid
              key={shown.queryId}
              sets={shown.sets}
              done={shown.done}
              sort={shown.path && canBrowse ? (shown.browse ?? {}) : undefined}
              onSort={shown.path && canBrowse ? (column) => openTable(shown.path!, { ...nextSort(shown.browse ?? {}, column), order: undefined }) : undefined}
              table={readOnly ? null : shown.table}
              onSaveCell={(result, row, col) =>
                toast(() => api.databaseSaveCell(shown.queryId, result, row, col)).then((saved) => saved && push("success", t("database.saved")))
              }
              onEditRow={async (key, changes) => {
                if (!shown.table) return false;
                if (canWrite) {
                  setPending({ op: "update", edit: { path: shown.table.path, key, set: changes } });
                  return true;
                }
                try {
                  await api.databaseEditRow(databaseId, shown.table.path, key, changes);
                } catch (e) {
                  push("error", api.errorInfo(e).message);
                  return false;
                }
                push("success", t("database.edited"));
                openTable(shown.table.path, shown.browse);
                return true;
              }}
              onEditDocument={canEditDocs && shown.table ? (id) => setDoc({ path: shown.table!.path, id }) : undefined}
              onDeleteRow={canWrite && shown.table ? (key) => setPending({ op: "delete", edit: { path: shown.table!.path, key } }) : undefined}
            />
          )}
        </div>
      </div>
      {library && <QueryLibrary databaseId={databaseId} current={text} onUse={putInEditor} onClose={() => setLibrary(false)} />}
      {inserting && shown?.table && (
        <InsertDialog
          columns={columns}
          onClose={() => setInserting(false)}
          onReview={(set) => {
            setInserting(false);
            setPending({ op: "insert", edit: { path: shown.table!.path, set } });
          }}
        />
      )}
      {pending && (
        <ReviewDialog
          databaseId={databaseId}
          pending={pending}
          onClose={() => setPending(null)}
          onDone={() => {
            const { op, edit } = pending;
            setPending(null);
            push("success", { insert: t("database.inserted"), delete: t("database.deleted"), update: t("database.edited") }[op]);
            openTable(edit.path, JSON.stringify(edit.path) === where ? shown?.browse : undefined);
          }}
        />
      )}
      {doc && (
        <DocumentView
          databaseId={databaseId}
          path={doc.path}
          id={doc.id}
          onClose={() => setDoc(null)}
          onSaved={() => {
            setDoc(null);
            push("success", t("database.documentSaved"));
            openTable(doc.path, JSON.stringify(doc.path) === where ? shown?.browse : undefined);
          }}
        />
      )}
    </div>
  );
}
