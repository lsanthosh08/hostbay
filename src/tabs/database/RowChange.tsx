import { useEffect, useState } from "react";
import * as api from "../../api";
import type { DbColumn, DbRowEdit, DbRowOp, DbRowPreview } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { Spinner } from "../../ui/kit";
import { Select } from "../../ui/Select";
import { insertSet, type InsertField } from "./insert";

/** A row change waiting for review. */
export type Pending = { op: DbRowOp; edit: DbRowEdit };

/**
 * The review step of a row change: the statement Go built and will run,
 * with its values, before anything changes. Cancel has focus.
 */
export function ReviewDialog({ databaseId, pending, onDone, onClose }: { databaseId: string; pending: Pending; onDone: () => void; onClose: () => void }) {
  const [preview, setPreview] = useState<DbRowPreview | null>(null);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    api.databasePreviewRow(databaseId, pending.op, pending.edit).then((p) => setPreview({ ...p, values: p.values ?? [] }), (e) => setError(api.errorInfo(e).message));
  }, [databaseId, pending]);
  const { op, edit } = pending;
  const run = async () => {
    setBusy(true);
    setError(undefined);
    try {
      if (op === "insert") await api.databaseInsertRow(databaseId, edit.path, edit.set ?? {});
      else if (op === "delete") await api.databaseDeleteRow(databaseId, edit.path, edit.key ?? {});
      else await api.databaseEditRow(databaseId, edit.path, edit.key ?? {}, edit.set ?? {});
      onDone();
    } catch (e) {
      setError(api.errorInfo(e).message);
    } finally {
      setBusy(false);
    }
  };
  const label = { insert: t("database.insertRow"), delete: t("database.deleteRow"), update: t("database.editSave") }[op];
  return (
    <Dialog
      title={t("database.review")}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" autoFocus onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant={op === "delete" ? "danger" : "primary"} disabled={!preview || busy} onClick={run}>
            {label}
          </Button>
        </>
      }
    >
      <p className="text-sm mb-2">{t("database.reviewHint")}</p>
      {!preview && !error && <Spinner />}
      {preview && (
        <>
          <pre className="text-xs font-mono whitespace-pre-wrap break-words allow-select p-2 rounded bg-control-bg">{preview.statement}</pre>
          {preview.values.length > 0 && (
            <ol className="mt-2 text-xs font-mono allow-select list-decimal pl-6">
              {preview.values.map((v, i) => (
                <li key={i} className={v === null ? "text-muted italic" : "break-all"}>
                  {v === null ? "NULL" : JSON.stringify(v)}
                </li>
              ))}
            </ol>
          )}
        </>
      )}
      {error && (
        <p role="alert" className="mt-2 text-sm text-danger allow-select break-words">
          {error}
        </p>
      )}
    </Dialog>
  );
}

/**
 * A new row's values, one field per column. Each column takes a value,
 * NULL, or is left out so the database gives it its default.
 */
export function InsertDialog({ columns, onReview, onClose }: { columns: DbColumn[]; onReview: (set: Record<string, string | null>) => void; onClose: () => void }) {
  const [fields, setFields] = useState<InsertField[]>(columns.map(() => ({ mode: "default", value: "" })));
  const put = (i: number, f: Partial<InsertField>) => setFields((fs) => fs.map((x, j) => (j === i ? { ...x, ...f } : x)));
  const set = insertSet(columns, fields);
  return (
    <Dialog
      title={t("database.insertRow")}
      onClose={onClose}
      dismissible={!Object.keys(set).length}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" disabled={!Object.keys(set).length} onClick={() => onReview(set)}>
            {t("database.reviewButton")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {columns.map((c, i) => (
          <Field key={c.name} label={c.name} hint={c.type}>
            <div className="flex items-center gap-2">
              <Select compact aria-label={t("database.insertMode", { name: c.name })} value={fields[i].mode} onChange={(e) => put(i, { mode: e.target.value as InsertField["mode"] })}>
                <option value="default">{t("database.insertDefault")}</option>
                <option value="value">{t("database.insertValue")}</option>
                <option value="null">NULL</option>
              </Select>
              {fields[i].mode === "value" && (
                <Input className="flex-1 min-w-0 font-mono" spellCheck={false} value={fields[i].value} onChange={(e) => put(i, { value: e.target.value })} />
              )}
            </div>
          </Field>
        ))}
      </div>
    </Dialog>
  );
}
