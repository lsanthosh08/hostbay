import { useEffect, useRef, useState } from "react";
import type { DbBrowse, DbCell, DbColumn, DbTable } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { EditIcon, ExpandedIcon } from "../../ui/icons";
import { Tabs } from "../../ui/Tabs";
import { useContextMenu, type MenuItem } from "../../ui/contextmenu";
import { clampWidth, copyText, rowText, windowRange } from "./grid";
import { ValueView } from "./ValueView";
import { canEdit, CellText, editable, rowKey, type ResultSet } from "./rows";

// Every row has one height, so only the rows in view need to be drawn.
const ROW = 24;
const EDIT_COL = 32;

/** A column's resize handle: drag it, or focus it and use the arrow keys;
 *  a double click puts the default width back. */
function ResizeHandle({ name, width, onWidth }: { name: string; width: number; onWidth: (w: number) => void }) {
  return (
    <span
      role="separator"
      aria-orientation="vertical"
      aria-label={t("database.resizeColumn", { name })}
      aria-valuenow={width}
      tabIndex={0}
      className="absolute right-0 top-0 h-full w-1.5 cursor-col-resize hover:bg-accent focus-visible:bg-accent outline-none"
      onClick={(e) => e.stopPropagation()}
      onDoubleClick={() => onWidth(clampWidth(Math.max(name.length * 8 + 32, 120)))}
      onKeyDown={(e) => {
        if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
          e.preventDefault();
          onWidth(clampWidth(width + (e.key === "ArrowLeft" ? -16 : 16)));
        }
      }}
      onPointerDown={(e) => {
        e.preventDefault();
        const x0 = e.clientX;
        const move = (m: PointerEvent) => onWidth(clampWidth(width + m.clientX - x0));
        const up = () => {
          window.removeEventListener("pointermove", move);
          window.removeEventListener("pointerup", up);
        };
        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", up);
      }}
    />
  );
}

/**
 * A request's result sets, one tab each, as a text-only grid that draws
 * only the rows in view. Columns resize; a cell's menu copies it or its
 * row, or opens it in the value viewer. A cut or binary cell offers its
 * full value, saved to a file Go asks for. With
 * `table` (a table opened from the tree whose engine edits rows, KTD23) a
 * row opens the row editor, which changes it by the table's own key.
 */
export function ResultGrid({
  sets,
  done,
  sort,
  onSort,
  table,
  onSaveCell,
  onEditRow,
  onEditDocument,
}: {
  sets: ResultSet[];
  /** Every row has arrived, so an empty set means no rows match. */
  done: boolean;
  /** A browsed table's sort; a header click asks onSort to change it. */
  sort?: DbBrowse;
  onSort?: (column: string) => void;
  table: DbTable | null;
  onSaveCell: (result: number, row: number, col: number) => void;
  onEditRow: (key: Record<string, string | null>, set: Record<string, string | null>) => Promise<boolean>;
  /** Set when rows are documents, edited whole by their _id (documentEdit). */
  onEditDocument?: (id: string) => void;
}) {
  const [tab, setTab] = useState("0");
  const { menu, node: menuNode } = useContextMenu();
  const scroller = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ top: 0, height: 600 });
  const [viewing, setViewing] = useState<{ r: number; i: number } | null>(null);
  const [editing, setEditing] = useState<number | null>(null);
  const at = Math.min(Number(tab), Math.max(sets.length - 1, 0));
  const set = sets[at];
  const rowsEdit = !!table && at === 0;
  // Widths follow the set on screen; a new set starts from the defaults.
  const [widths, setWidths] = useState<number[]>([]);
  const cols = set?.columns ?? [];
  useEffect(() => setWidths(cols.map((c) => clampWidth(Math.max(c.name.length * 8 + 32, 120)))), [at, cols.length]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setView({ top: el.scrollTop, height: el.clientHeight }));
    ro.observe(el);
    return () => ro.disconnect();
  }, [at, !!set]);
  const { first, end } = windowRange(view.top, view.height, ROW, set?.rows.length ?? 0);
  const copy = (text: string) => navigator.clipboard.writeText(text).catch(() => {});
  const cellMenu = (r: number, i: number): MenuItem[] => {
    const c = set!.rows[r][i];
    return [
      { label: t("database.copyValue"), onClick: () => copy(copyText(c)) },
      { label: t("database.copyRow"), onClick: () => copy(rowText(set!.rows[r])) },
      { label: t("database.viewValue"), onClick: () => setViewing({ r, i }) },
      ...(c.c || c.b ? [{ label: t("database.saveValue"), onClick: () => onSaveCell(at, r, i) }] : []),
    ];
  };
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
        <div
          ref={scroller}
          className="flex-1 min-h-0 overflow-auto allow-select"
          onScroll={(e) => setView({ top: e.currentTarget.scrollTop, height: e.currentTarget.clientHeight })}
        >
          <table className="text-xs font-mono border-collapse" style={{ tableLayout: "fixed", width: (rowsEdit ? EDIT_COL : 0) + widths.reduce((a, b) => a + b, 0) }}>
            <colgroup>
              {rowsEdit && <col style={{ width: EDIT_COL }} />}
              {widths.map((w, i) => (
                <col key={i} style={{ width: w }} />
              ))}
            </colgroup>
            <thead className="sticky top-0 bg-surface z-10">
              <tr>
                {rowsEdit && <th className="border-b border-line" aria-label={t("database.editRow")} />}
                {set.columns.map((c, i) => (
                  <th
                    key={i}
                    scope="col"
                    className="relative text-left font-semibold px-2 py-1 border-b border-line whitespace-nowrap overflow-hidden text-ellipsis"
                    title={`${c.name}: ${c.type}`}
                    aria-sort={sort?.sort === c.name && at === 0 ? (sort.desc ? "descending" : "ascending") : undefined}
                  >
                    {onSort && at === 0 ? (
                      <button type="button" className="flex items-center gap-1 max-w-full hover:underline" title={t("database.sortBy", { name: c.name })} onClick={() => onSort(c.name)}>
                        <span className="truncate">{c.name}</span>
                        {sort?.sort === c.name && <ExpandedIcon className={`size-3 shrink-0 ${sort.desc ? "" : "rotate-180"}`} aria-hidden />}
                      </button>
                    ) : (
                      c.name
                    )}
                    <ResizeHandle name={c.name} width={widths[i]} onWidth={(w) => setWidths((ws) => ws.map((x, j) => (j === i ? w : x)))} />
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {first > 0 && <tr aria-hidden style={{ height: first * ROW }} />}
              {set.rows.slice(first, end).map((row, k) => {
                const r = first + k;
                return (
                  <tr key={r} className="hover:bg-hover" style={{ height: ROW }} onDoubleClick={() => rowsEdit && edit(r)}>
                    {rowsEdit && (
                      <td className="px-1 border-b border-line">
                        <IconButton size="sm" icon={EditIcon} label={onEditDocument ? t("database.editDocument") : t("database.editRow")} onClick={() => edit(r)} />
                      </td>
                    )}
                    {row.map((c, i) => (
                      <td
                        key={i}
                        className="px-2 border-b border-line whitespace-nowrap overflow-hidden text-ellipsis"
                        title={c.c || c.b ? t("database.cellHint") : undefined}
                        onContextMenu={menu(cellMenu(r, i))}
                        onDoubleClick={() => !rowsEdit && setViewing({ r, i })}
                      >
                        <CellText cell={c} />
                      </td>
                    ))}
                  </tr>
                );
              })}
              {end < set.rows.length && <tr aria-hidden style={{ height: (set.rows.length - end) * ROW }} />}
            </tbody>
          </table>
          {done && !set.rows.length && <p className="p-3 text-sm text-muted">{t("database.noMatch")}</p>}
        </div>
      ) : null}
      {menuNode}
      {viewing && set?.rows[viewing.r] && (
        <ValueView
          column={set.columns[viewing.i]}
          cell={set.rows[viewing.r][viewing.i]}
          onSave={() => onSaveCell(at, viewing.r, viewing.i)}
          onClose={() => setViewing(null)}
        />
      )}
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
