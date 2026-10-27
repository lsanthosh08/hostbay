// Result rows as the database tab shows them. Every value is text: a cell
// renders through React's text nodes only, never as HTML, so a value such as
// <img onerror=…> shows as itself (KTD23).
import { createElement } from "react";
import type { DbCell, DbColumn, DbPage, DbTable } from "../../api";

/** One result set gathered from its pages. */
export type ResultSet = { columns: DbColumn[]; rows: DbCell[][] };

const size = (n: number) => (n < 1024 ? `${n} bytes` : n < 1 << 20 ? `${Math.round(n / 1024)} KiB` : `${(n / (1 << 20)).toFixed(1)} MiB`);

/** A cell's text: NULL marked, a cut value with its full size, and a binary
 *  one as its hex prefix and size. */
export function cellText(c: DbCell): string {
  if (c.n) return "NULL";
  if (c.b) return `0x${c.t}${c.c ? "…" : ""} (${size(c.s ?? 0)})`;
  if (c.c) return `${c.t}… (${size(c.s ?? 0)})`;
  return c.t;
}

/** A cell as text; the marked kinds are styled apart from real values. */
export function CellText({ cell }: { cell: DbCell }) {
  const marked = cell.n || cell.b || cell.c;
  return createElement("span", { className: marked ? "text-muted italic" : undefined }, cellText(cell));
}

/** Adds a page to the result sets: a page carrying columns starts its set. */
export function addPage(sets: ResultSet[], p: DbPage): ResultSet[] {
  const next = sets.slice();
  const cur = next[p.result] ?? { columns: [], rows: [] };
  next[p.result] = { columns: p.columns ?? cur.columns, rows: cur.rows.concat(p.rows ?? []) };
  return next;
}

/** A cell the row editor can change: one shown in full as text. */
export const editable = (c: DbCell) => !c.c && !c.b;

/** A cell the row editor offers: shown in full, in a column the table's
 *  engine changes. */
export const canEdit = (table: DbTable, column: string, c: DbCell) => editable(c) && (!table.edit?.length || table.edit.includes(column));

/** The row's key values by the table's own key columns, or null when a key
 *  column is missing or not shown in full, so the row cannot be named. */
export function rowKey(table: DbTable, columns: DbColumn[], row: DbCell[]): Record<string, string | null> | null {
  const key: Record<string, string | null> = {};
  for (const name of table.key) {
    const i = columns.findIndex((c) => c.name === name);
    if (i < 0 || !row[i] || !editable(row[i])) return null;
    key[name] = row[i].n ? null : row[i].t;
  }
  return key;
}

/** Lays out compact JSON two spaces an indent, by its characters alone: it
 *  is never parsed, so a number beyond JavaScript's precision keeps every
 *  digit, and the text inside strings is left as it is. */
export function indentJSON(s: string): string {
  let out = "";
  let depth = 0;
  let inString = false;
  const line = () => "\n" + "  ".repeat(depth);
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (inString) {
      out += c;
      if (c === "\\") out += s[++i] ?? "";
      else if (c === '"') inString = false;
    } else if (c === '"') {
      inString = true;
      out += c;
    } else if (c === "{" || c === "[") {
      const close = c === "{" ? "}" : "]";
      let j = i + 1;
      while (/\s/.test(s[j] ?? "")) j++;
      if (s[j] === close) {
        out += c + close; // an empty object or array stays on its line
        i = j;
      } else {
        depth++;
        out += c + line();
      }
    } else if (c === "}" || c === "]") {
      depth--;
      out += line() + c;
    } else if (c === ",") {
      out += c + line();
    } else if (c === ":") {
      out += ": ";
    } else if (!/\s/.test(c)) {
      out += c;
    }
  }
  return out;
}
