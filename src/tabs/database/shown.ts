import type { AppError, DbTable, QueryState } from "../../api";
import type { ResultSet } from "./rows";

/** The request on screen: a query's text, or a table opened from the tree. */
export type Shown = {
  queryId: string;
  text?: string;
  path?: string[];
  table: DbTable | null;
  sets: ResultSet[];
  state?: QueryState;
  done: boolean;
  loading: boolean;
  error?: string;
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
