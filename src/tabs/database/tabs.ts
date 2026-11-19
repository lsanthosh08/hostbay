import type { DbSchemaTable } from "../../api";

/** One query tab of a database tab: its own text, run on the one session. */
export type QueryTab = { id: string; name: string; text: string };

/** Adds a tab after the others, numbered after every tab so far: last is
 *  the highest number given out, closed tabs included, since each tab's
 *  number names its Monaco model and a reused one would bring back a
 *  closed tab's text. */
export function addTab(tabs: QueryTab[], name: (n: number) => string, last = 0): { tabs: QueryTab[]; id: string } {
  const ids = tabs.map((t) => Number(t.id));
  const n = Math.max(last, ...ids) + 1;
  const id = String(n);
  return { tabs: [...tabs, { id, name: name(n), text: "" }], id };
}

/** Closes a tab; the last one stays. The active tab moves to a neighbour
 *  when it is the one closed. */
export function closeTab(tabs: QueryTab[], id: string, active: string): { tabs: QueryTab[]; active: string } {
  const at = tabs.findIndex((t) => t.id === id);
  if (at < 0 || tabs.length === 1) return { tabs, active };
  const next = tabs.filter((t) => t.id !== id);
  return { tabs: next, active: active === id ? next[Math.min(at, next.length - 1)].id : active };
}

/** Renames a tab; a blank name keeps the old one. */
export const renameTab = (tabs: QueryTab[], id: string, name: string) =>
  name.trim() ? tabs.map((t) => (t.id === id ? { ...t, name: name.trim() } : t)) : tabs;

export const setTabText = (tabs: QueryTab[], id: string, text: string) => tabs.map((t) => (t.id === id ? { ...t, text } : t));

/** A completion: a table (by its full name and, when schema-qualified,
 *  its own name too) or a column with the tables that have it. */
export type Completion = { label: string; kind: "table" | "column"; detail: string };

export function completions(schema: DbSchemaTable[]): Completion[] {
  const out: Completion[] = [];
  const seen = new Set<string>();
  const cols = new Map<string, string[]>();
  for (const t of schema) {
    const short = t.name.slice(t.name.lastIndexOf(".") + 1);
    for (const label of [t.name, short]) {
      if (seen.has(label)) continue;
      seen.add(label);
      out.push({ label, kind: "table", detail: t.name });
    }
    for (const c of t.columns ?? []) cols.set(c, [...(cols.get(c) ?? []), short]);
  }
  for (const [label, in_] of cols) if (!seen.has(label)) out.push({ label, kind: "column", detail: in_.join(", ") });
  return out;
}
