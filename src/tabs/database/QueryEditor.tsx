import Editor, { type OnMount } from "@monaco-editor/react";
import { useEffect, useRef } from "react";
import { t } from "../../i18n";
import { applyEditorTheme } from "../../monaco";
import { Button } from "../../ui/Button";
import { StartIcon, StopIcon } from "../../ui/icons";

type MonacoEditor = Parameters<OnMount>[0];

/**
 * The query editor. Run sends the selection, or the whole text when nothing
 * is selected, as one request (Ctrl or Cmd+Enter too); Cancel stops it.
 * Inserted SQL (from Ask AI) replaces the selection and is left selected,
 * so Run sends that statement alone.
 */
export function QueryEditor({
  value,
  onChange,
  onRun,
  running,
  onCancel,
  insert,
}: {
  value: string;
  onChange: (text: string) => void;
  onRun: (text: string) => void;
  running: boolean;
  onCancel: () => void;
  /** SQL to insert; n changes for each insert. */
  insert?: { sql: string; n: number };
}) {
  const editor = useRef<MonacoEditor | null>(null);
  // The latest onRun, for the key binding Monaco keeps from mount.
  const run = useRef(onRun);
  run.current = onRun;
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
        <span className="text-xs text-muted">{t("database.runHint")}</span>
      </div>
      <div className="flex-1 min-h-0" aria-label={t("database.editor")}>
        <Editor
          height="100%"
          theme="hostbay"
          language="sql"
          beforeMount={applyEditorTheme}
          value={value}
          onChange={(v) => onChange(v ?? "")}
          onMount={(e, monaco) => {
            editor.current = e;
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
