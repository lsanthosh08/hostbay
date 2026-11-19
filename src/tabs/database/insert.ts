import type { DbColumn } from "../../api";

/** One column of a new row: a value, NULL, or left out for its default. */
export type InsertField = { mode: "value" | "null" | "default"; value: string };

/** The columns a new row sets: a column left out takes its default. */
export function insertSet(columns: DbColumn[], fields: InsertField[]): Record<string, string | null> {
  const set: Record<string, string | null> = {};
  columns.forEach((c, i) => {
    const f = fields[i];
    if (f?.mode === "value") set[c.name] = f.value;
    else if (f?.mode === "null") set[c.name] = null;
  });
  return set;
}
