import type { AppError, DbBrowse, DbTable, QueryState } from "../../api";
import type { ResultSet } from "./rows";

/** The request on screen: a query's text, or a table opened from the tree. */
export type Shown = {
  queryId: string;
  text?: string;
  path?: string[];
  /** A table's sort and filters (the "browse" capability). */
  browse?: DbBrowse;
  /** Its row count: undefined until asked for, null while counting. */
  count?: number | null;
  table: DbTable | null;
  sets: ResultSet[];
  state?: QueryState;
  done: boolean;
  loading: boolean;
  error?: string;
  /** When the request started, and how long it took to its first page or
   *  its end (Date.now() milliseconds). */
  started?: number;
  took?: number;
};

/** Whether the request is working now: opening, or reading a page. Go
 *  reports a cursor paused after a page, waiting for "Load more", as
 *  "running" until it is read to the end, so that state alone would keep
 *  Run disabled and Cancel lit with nothing running. */
export const isRunning = (s: Shown | null) => !!s && s.loading && !s.error && s.state?.state !== "cancelled";


/** s once cancelled: nothing runs, and the status line says so. */
export const cancelled = (s: Shown): Shown => ({ ...s, loading: false, state: { queryId: s.queryId, state: "cancelled", rows: s.state?.rows ?? 0 } });

/** s after its request failed with e. A cancel is not an error: Go
 *  reports it as one ("context canceled") to the page that was reading. */
export const failed = (s: Shown, e: AppError): Shown =>
  e.details?.category === "cancelled" ? cancelled(s) : { ...s, loading: false, error: e.message };

/** text cut to n characters for a dialog that quotes it. */
export const clip = (text: string, n = 600) => (text.length > n ? text.slice(0, n) + "…" : text);

/** A duration for the status line: 0.25 s, 12.3 s, 2 min 5 s. */
export function duration(ms: number): string {
  if (ms < 10_000) return `${(ms / 1000).toFixed(2)} s`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)} min ${s % 60} s`;
}
