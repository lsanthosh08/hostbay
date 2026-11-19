import { useEffect, useState } from "react";
import type { DbBrowse, DbColumn, DbFilter, DbFilterOp } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { AddIcon, CloseIcon, RefreshIcon } from "../../ui/icons";
import { Select } from "../../ui/Select";
import { ready, takesValue } from "./browse";

const OPS: DbFilterOp[] = ["contains", "equals", "null", "notNull"];
const opLabel = (op: DbFilterOp) =>
  ({ contains: t("database.op.contains"), equals: t("database.op.equals"), null: t("database.op.null"), notNull: t("database.op.notNull") })[op];

/**
 * Filters, a row count and refresh for a table opened from the tree. The
 * filters are a draft until Apply (or Enter in a value), then Go runs the
 * browse; every filter must hold. Sorting is a click on a column header.
 * A document engine (`json`) takes a JSON filter and sort instead, which
 * Go parses into the command.
 */
export function BrowseBar({
  columns,
  browse,
  onApply,
  onRefresh,
  count,
  onCount,
  onInsert,
  json = false,
}: {
  columns: DbColumn[];
  browse: DbBrowse;
  onApply: (b: DbBrowse) => void;
  onRefresh: () => void;
  /** The row count: undefined until asked for, null while counting. */
  count: number | null | undefined;
  onCount: () => void;
  /** Set when a row can be added (rowWrite, not read-only). */
  onInsert?: () => void;
  /** Filter and sort as JSON documents (browseJSON) instead of by column. */
  json?: boolean;
}) {
  const [draft, setDraft] = useState<DbFilter[]>(browse.filters ?? []);
  const [query, setQuery] = useState(browse.query ?? "");
  const [order, setOrder] = useState(browse.order ?? "");
  // A header click sorts in place of the JSON sort, and clears it.
  useEffect(() => setOrder(browse.order ?? ""), [browse.order]);
  const put = (i: number, f: Partial<DbFilter>) => setDraft((d) => d.map((x, j) => (j === i ? { ...x, ...f } : x)));
  const apply = (filters = draft) =>
    onApply(json ? { ...browse, query: query.trim() || undefined, order: order.trim() || undefined } : { ...browse, filters: ready(filters) });
  const add = () => setDraft((d) => [...d, { column: columns[0]?.name ?? "", op: "contains", value: "" }]);
  return (
    <form
      className="flex flex-wrap items-center gap-2 px-2 py-1.5 border-b border-line text-xs"
      aria-label={t("database.browse")}
      onSubmit={(e) => {
        e.preventDefault();
        apply();
      }}
    >
      {json && (
        <>
          <Input
            className="h-7 w-64 text-xs font-mono"
            aria-label={t("database.jsonFilter")}
            placeholder={t("database.jsonFilterPlaceholder")}
            spellCheck={false}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          <Input
            className="h-7 w-40 text-xs font-mono"
            aria-label={t("database.jsonSort")}
            placeholder={t("database.jsonSortPlaceholder")}
            spellCheck={false}
            value={order}
            onChange={(e) => setOrder(e.target.value)}
          />
          <Button size="sm" type="submit">
            {t("database.filterApply")}
          </Button>
          {(browse.query || browse.order) && (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => {
                setQuery("");
                setOrder("");
                onApply({ ...browse, query: undefined, order: undefined });
              }}
            >
              {t("database.filterClear")}
            </Button>
          )}
        </>
      )}
      {!json &&
        draft.map((f, i) => (
          <div key={i} className="flex items-center gap-1">
            <Select compact aria-label={t("database.filterColumn")} value={f.column} onChange={(e) => put(i, { column: e.target.value })}>
              {columns.map((c) => (
                <option key={c.name} value={c.name}>
                  {c.name}
                </option>
              ))}
            </Select>
            <Select compact aria-label={t("database.filterOp")} value={f.op} onChange={(e) => put(i, { op: e.target.value as DbFilterOp })}>
              {OPS.map((op) => (
                <option key={op} value={op}>
                  {opLabel(op)}
                </option>
              ))}
            </Select>
            {takesValue(f) && (
              <Input
                className="h-7 w-36 text-xs font-mono"
                aria-label={t("database.filterValue")}
                spellCheck={false}
                value={f.value}
                onChange={(e) => put(i, { value: e.target.value })}
              />
            )}
            <IconButton
              size="sm"
              icon={CloseIcon}
              label={t("database.filterRemove")}
              onClick={() => {
                const next = draft.filter((_, j) => j !== i);
                setDraft(next);
                apply(next);
              }}
            />
          </div>
        ))}
      {!json && (
        <Button type="button" size="sm" variant="ghost" icon={AddIcon} disabled={!columns.length} onClick={add}>
          {t("database.filterAdd")}
        </Button>
      )}
      {!json && draft.length > 0 && (
        <>
          <Button size="sm" type="submit">
            {t("database.filterApply")}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => {
              setDraft([]);
              apply([]);
            }}
          >
            {t("database.filterClear")}
          </Button>
        </>
      )}
      <span className="ml-auto flex items-center gap-2">
        {onInsert && (
          <Button type="button" size="sm" variant="ghost" icon={AddIcon} disabled={!columns.length} onClick={onInsert}>
            {t("database.insertRow")}
          </Button>
        )}
        <Button type="button" size="sm" variant="ghost" disabled={count === null} onClick={onCount}>
          {count === undefined ? t("database.countRows") : count === null ? t("database.counting") : t("database.counted", { n: count.toLocaleString() })}
        </Button>
        <IconButton size="sm" icon={RefreshIcon} label={t("database.refresh")} onClick={onRefresh} />
      </span>
    </form>
  );
}
