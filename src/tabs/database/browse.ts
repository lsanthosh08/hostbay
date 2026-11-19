import type { DbBrowse, DbFilter } from "../../api";

/** The browse after a click on column's header: ascending, then
 *  descending, then unsorted. */
export function nextSort(b: DbBrowse, column: string): DbBrowse {
  if (b.sort !== column) return { ...b, sort: column, desc: false };
  if (!b.desc) return { ...b, desc: true };
  return { ...b, sort: undefined, desc: undefined };
}

/** Whether a filter's operator compares with a value (the NULL tests do not). */
export const takesValue = (f: DbFilter) => f.op === "contains" || f.op === "equals";

/** The filters worth sending: a value test with an empty value is still
 *  being typed, so it is left out rather than matching everything or nothing. */
export const ready = (filters: DbFilter[]) => filters.filter((f) => f.column && (!takesValue(f) || f.value !== ""));
