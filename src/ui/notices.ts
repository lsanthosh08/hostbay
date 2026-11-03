import { useSyncExternalStore } from "react";

// Every notice of this session, for the rail's Notifications panel: each
// toast and each AI or MCP approval request. Kept in memory only.
export type Notice = { id: string; kind: "info" | "error" | "success" | "approval"; msg: string; at: number; pending?: boolean };

const MAX = 200;
let snap: { list: Notice[]; unread: number } = { list: [], unread: 0 };
const subs = new Set<() => void>();
const set = (next: typeof snap) => {
  snap = next;
  subs.forEach((f) => f());
};

export const notices = {
  add(n: Omit<Notice, "at">) {
    set({ list: [{ ...n, at: Date.now() }, ...snap.list.filter((x) => x.id !== n.id)].slice(0, MAX), unread: snap.unread + 1 });
  },
  /** An approval left the tray: answered, withdrawn or expired. */
  settle(id: string) {
    if (snap.list.some((x) => x.id === id && x.pending))
      set({ ...snap, list: snap.list.map((x) => (x.id === id ? { ...x, pending: false } : x)) });
  },
  read() {
    if (snap.unread) set({ ...snap, unread: 0 });
  },
  /** Clear keeps what still waits for an answer, unless all is set. */
  clear(all = false) {
    set({ list: all ? [] : snap.list.filter((x) => x.pending), unread: 0 });
  },
  subscribe(f: () => void) {
    subs.add(f);
    return () => void subs.delete(f);
  },
  get: () => snap,
};

export const useNotices = () => useSyncExternalStore(notices.subscribe, notices.get);
