import type { DbCell } from "../../api";
import { cellText } from "./rows";

/** Rows drawn above and below the visible ones, so a quick scroll does not
 *  show blank space before the next frame. */
export const OVERSCAN = 10;

/** The rows to draw, [first, end), for a scroll offset, a viewport height
 *  and a fixed row height: only these are in the DOM, so tens of thousands
 *  of loaded rows scroll as quickly as fifty. */
export function windowRange(scrollTop: number, height: number, rowHeight: number, total: number, overscan = OVERSCAN) {
  const first = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const end = Math.min(total, Math.ceil((scrollTop + height) / rowHeight) + overscan);
  return { first, end: Math.max(first, end) };
}

/** A cell as copied: its value as text, and NULL as nothing. A cut or
 *  binary cell copies what is shown, with its marker. */
export const copyText = (c: DbCell) => (c.n ? "" : c.c || c.b ? cellText(c) : c.t);

/** A row as copied: its cells tab-separated, as a spreadsheet pastes it. A
 *  tab or line break inside a value becomes a space, so it stays one row. */
export const rowText = (row: DbCell[]) => row.map((c) => copyText(c).replace(/[\t\r\n]+/g, " ")).join("\t");

/** Whether a value reads as a JSON object or array, for the viewer. */
export function isJSON(s: string): boolean {
  const t = s.trim();
  if (!(t.startsWith("{") && t.endsWith("}")) && !(t.startsWith("[") && t.endsWith("]"))) return false;
  try {
    JSON.parse(t);
    return true;
  } catch {
    return false;
  }
}

/** A column width after a resize: never narrower than one character. */
export const clampWidth = (w: number) => Math.max(48, Math.min(1200, Math.round(w)));
