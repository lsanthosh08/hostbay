import Editor, { type OnMount } from "@monaco-editor/react";
import type { editor, Position } from "monaco-editor";
import { useEffect, useRef } from "react";
import { t } from "../../i18n";
import { applyEditorTheme } from "../../monaco";
import { Button } from "../../ui/Button";
import { LogsIcon, StartIcon, StopIcon } from "../../ui/icons";
import type { Completion } from "./tabs";

type MonacoEditor = Parameters<OnMount>[0];

/**
 * The query editor. Run sends the selection, or the whole text when nothing
 * is selected, as one request (Ctrl or Cmd+Enter too); Cancel stops it.
 * Inserted SQL (from Ask AI) replaces the selection and is left selected,
 * so Run sends that statement alone. Each query tab is its own Monaco
 * model (by `path`), so undo stays with its tab. On SQL engines the
 * editor completes the table and column names in `schema`.
 */
export function QueryEditor({
  value,
  onChange,
  onRun,
  running,
  onCancel,
  insert,
  readOnly,
  onReadOnly,
  onLibrary,
  onExplain,
  path,
  schema,
}: {
  value: string;
  onChange: (text: string) => void;
  onRun: (text: string) => void;
  running: boolean;
  onCancel: () => void;
  /** SQL to insert; n changes for each insert. */
  insert?: { sql: string; n: number };
  /** The tab's read-only mode, which Go enforces. */
  readOnly: boolean;
  onReadOnly: (on: boolean) => void;
  /** Opens the history and saved queries. */
  onLibrary: () => void;
  /** Shows the selection's plan instead of running it (the explain capability). */
  onExplain?: (text: string) => void;
  /** The active query tab's model path; models under one tab share a prefix. */
  path: string;
  /** Names to complete; empty offers none. */
  schema: Completion[];
}) {
  const editor = useRef<MonacoEditor | null>(null);
  // The latest onRun, for the key binding Monaco keeps from mount.
  const run = useRef(onRun);
  run.current = onRun;
  const names = useRef(schema);
  names.current = schema;
  // The completion provider is Monaco's, for every SQL model: it answers
  // only for this tab's models.
  const prefix = path.slice(0, path.lastIndexOf("/") + 1);
  const provider = useRef<{ dispose: () => void } | null>(null);
  useEffect(() => () => provider.current?.dispose(), []);
  useEffect(() => {
    const e = editor.current;
    const model = e?.getModel();
    if (!insert) return;
    if (!e || !model) return onChange(insert.sql);
    const sel = e.getSelection() ?? model.getFullModelRange();
    // At a cursor on a line that holds text, the SQL goes on a line of its own.
    const sep = sel.isEmpty() && model.getLineContent(sel.startLineNumber).trim() ? "\n" : "";
    const start = model.getOffsetAt(sel.getStartPosition()) + sep.length;
    e.executeEdits("ask-ai", [{ range: sel, text: sep + insert.sql, forceMoveMarkers: true }]);
    const a = model.getPositionAt(start);
    const b = model.getPositionAt(start + insert.sql.length);
    e.setSelection({ startLineNumber: a.lineNumber, startColumn: a.column, endLineNumber: b.lineNumber, endColumn: b.column });
    e.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [insert?.n]);
  const text = () => {
    const e = editor.current;
    const sel = e?.getSelection();
    const picked = sel && !sel.isEmpty() ? e?.getModel()?.getValueInRange(sel) : "";
    return (picked || e?.getValue() || value).trim();
  };

  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center gap-2 px-2 py-1.5 border-b border-line">
        <Button size="sm" variant="primary" icon={StartIcon} disabled={running} onClick={() => text() && onRun(text())}>
          {t("database.run")}
        </Button>
        <Button size="sm" icon={StopIcon} disabled={!running} onClick={onCancel}>
          {t("database.cancel")}
        </Button>
        {onExplain && (
          <Button size="sm" disabled={running} title={t("database.explainHint")} onClick={() => text() && onExplain(text())}>
            {t("database.explain")}
          </Button>
        )}
        <Button size="sm" variant="ghost" icon={LogsIcon} onClick={onLibrary}>
          {t("database.history")}
        </Button>
        <span className="text-xs text-muted min-w-0 truncate" title={t("database.runHint")}>
          {t("database.runHint")}
        </span>
        <label className="ml-auto flex items-center gap-1.5 text-xs shrink-0" title={t("database.readOnlyModeHint")}>
          <input type="checkbox" className="accent-accent size-3.5" checked={readOnly} onChange={(e) => onReadOnly(e.target.checked)} />
          {t("database.readOnlyMode")}
        </label>
      </div>
      <div className="flex-1 min-h-0" aria-label={t("database.editor")}>
        <Editor
          height="100%"
          theme="hostbay"
          language="sql"
          path={path}
          beforeMount={applyEditorTheme}
          value={value}
          onChange={(v) => onChange(v ?? "")}
          onMount={(e, monaco) => {
            editor.current = e;
            provider.current = monaco.languages.registerCompletionItemProvider("sql", {
              provideCompletionItems: (model: editor.ITextModel, pos: Position) => {
                if (!model.uri.path.includes(prefix) || !names.current.length) return { suggestions: [] };
                const w = model.getWordUntilPosition(pos);
                const range = { startLineNumber: pos.lineNumber, endLineNumber: pos.lineNumber, startColumn: w.startColumn, endColumn: w.endColumn };
                return {
                  suggestions: names.current.map((c) => ({
                    label: c.label,
                    kind: c.kind === "table" ? monaco.languages.CompletionItemKind.Class : monaco.languages.CompletionItemKind.Field,
                    detail: c.detail,
                    insertText: c.label,
                    range,
                  })),
                };
              },
            });
            e.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.Enter, () => {
              const q = text();
              if (q) run.current(q);
            });
          }}
          options={{ fontSize: 13, minimap: { enabled: false }, wordWrap: "on", automaticLayout: true, scrollBeyondLastLine: false }}
        />
      </div>
    </div>
  );
}
