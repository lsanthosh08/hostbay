import { useState } from "react";
import type { DbCell, DbColumn, DbTable } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { EditIcon } from "../../ui/icons";
import { Tabs } from "../../ui/Tabs";
import { canEdit, CellText, editable, rowKey, type ResultSet } from "./rows";

/**
 * A request's result sets, one tab each, as a text-only grid. A cut or
 * binary cell offers its full value, saved to a file Go asks for. With
 * `table` (a table opened from the tree whose engine edits rows, KTD23) a
 * row opens the row editor, which changes it by the table's own key.
 */
export function ResultGrid({
  sets,
  table,
  onSaveCell,
  onEditRow,
  onEditDocument,
}: {
  sets: ResultSet[];
  table: DbTable | null;
  onSaveCell: (result: number, row: number, col: number) => void;
  onEditRow: (key: Record<string, string | null>, set: Record<string, string | null>) => Promise<boolean>;
  /** Set when rows are documents, edited whole by their _id (documentEdit). */
  onEditDocument?: (id: string) => void;
}) {
  const [tab, setTab] = useState("0");
  const [editing, setEditing] = useState<number | null>(null);
  const at = Math.min(Number(tab), Math.max(sets.length - 1, 0));
  const set = sets[at];
  const rowsEdit = !!table && at === 0;
  const edit = (r: number) => {
    if (!onEditDocument) return setEditing(r);
    const id = table && set && rowKey(table, set.columns, set.rows[r])?._id;
    if (id) onEditDocument(id);
  };
  return (
    <div className="flex flex-col h-full min-h-0">
      {sets.length > 1 && (
        <Tabs
          label={t("database.results")}
          className="px-2 border-b border-line"
          value={String(at)}
          onChange={setTab}
          items={sets.map((_, i) => ({ id: String(i), label: t("database.result", { n: i + 1 }) }))}
        />
      )}
      {set && !set.columns.length ? (
        <p className="p-3 text-sm text-muted">{t("database.noRows")}</p>
      ) : set ? (
        <div className="flex-1 min-h-0 overflow-auto allow-select">
          <table className="text-xs font-mono border-collapse">
            <thead className="sticky top-0 bg-surface">
              <tr>
                {rowsEdit && <th className="border-b border-line" aria-label={t("database.editRow")} />}
                {set.columns.map((c, i) => (
                  <th key={i} scope="col" className="text-left font-semibold px-2 py-1 border-b border-line whitespace-nowrap" title={c.type}>
                    {c.name}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {set.rows.map((row, r) => (
                <tr key={r} className="hover:bg-hover" onDoubleClick={() => rowsEdit && edit(r)}>
                  {rowsEdit && (
                    <td className="px-1 border-b border-line align-top">
                      <IconButton size="sm" icon={EditIcon} label={onEditDocument ? t("database.editDocument") : t("database.editRow")} onClick={() => edit(r)} />
                    </td>
                  )}
                  {row.map((c, i) => (
                    <td key={i} className="px-2 py-0.5 border-b border-line align-top max-w-96">
                      <div className="flex items-start gap-1">
                        <span className="truncate">
                          <CellText cell={c} />
                        </span>
                        {(c.c || c.b) && (
                          <button
                            type="button"
                            className="shrink-0 text-accent hover:underline"
                            onClick={(e) => {
                              e.stopPropagation();
                              onSaveCell(at, r, i);
                            }}
                          >
                            {t("database.saveValue")}
                          </button>
                        )}
                      </div>
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {editing !== null && table && set && (
        <RowEditor
          table={table}
          columns={set.columns}
          row={set.rows[editing]}
          onClose={() => setEditing(null)}
          onSave={async (key, changes) => {
            if (await onEditRow(key, changes)) setEditing(null);
          }}
        />
      )}
    </div>
  );
}

/** Edits one row's cells; only changed ones are sent, and NULL is its own
 *  choice unless the engine refuses it. Only the columns the table names
 *  as changeable open. A row whose key is not shown in full cannot be
 *  named, so it cannot be edited. */
function RowEditor({
  table,
  columns,
  row,
  onClose,
  onSave,
}: {
  table: DbTable;
  columns: DbColumn[];
  row: DbCell[];
  onClose: () => void;
  onSave: (key: Record<string, string | null>, set: Record<string, string | null>) => void;
}) {
  const key = rowKey(table, columns, row);
  const [values, setValues] = useState<(string | null)[]>(row.map((c) => (c.n ? null : c.t)));
  const changes: Record<string, string | null> = {};
  columns.forEach((c, i) => {
    const before = row[i].n ? null : row[i].t;
    if (canEdit(table, c.name, row[i]) && values[i] !== before) changes[c.name] = values[i];
  });
  const put = (i: number, v: string | null) => setValues((xs) => xs.map((x, j) => (j === i ? v : x)));
  return (
    <Dialog
      title={t("database.editRow")}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" disabled={!key || !Object.keys(changes).length} onClick={() => key && onSave(key, changes)}>
            {t("database.editSave")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {!key && <p className="text-sm text-warning">{t("database.keyNotShown")}</p>}
        {columns.map((c, i) => (
          <Field
            key={i}
            label={c.name}
            hint={!editable(row[i]) ? t("database.notEditable") : canEdit(table, c.name, row[i]) ? c.type : t("database.columnFixed")}
          >
            <div className="flex items-center gap-2">
              <Input
                className="flex-1 min-w-0 font-mono"
                spellCheck={false}
                disabled={!canEdit(table, c.name, row[i]) || values[i] === null}
                value={values[i] ?? ""}
                onChange={(e) => put(i, e.target.value)}
              />
              {!table.noNull && (
                <label className="flex items-center gap-1 text-xs shrink-0">
                  <input
                    type="checkbox"
                    className="accent-accent size-3.5"
                    disabled={!canEdit(table, c.name, row[i])}
                    checked={values[i] === null}
                    onChange={(e) => put(i, e.target.checked ? null : "")}
                  />
                  {t("database.null")}
                </label>
              )}
            </div>
          </Field>
        ))}
      </div>
    </Dialog>
  );
}
