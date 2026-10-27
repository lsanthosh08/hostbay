import { createContext, useContext } from "react";
import type { ConflictChoice, TransferEvent } from "../api";

/** A transfer task as the panel shows it: the engine's latest event plus
 *  what it was started as. */
export type Task = TransferEvent & { label: string; to: string; toDir: string };

export type TransfersState = {
  /** Tasks in the order they started. */
  tasks: Task[];
  /** The last task to finish, so the pane showing its destination lists it again. */
  landed: { to: string; toDir: string; n: number } | null;
};

export const initialTransfers: TransfersState = { tasks: [], landed: null };

export type TransfersAction =
  | { type: "start"; taskId: string; label: string; to: string; toDir: string }
  | { type: "event"; event: TransferEvent }
  | { type: "dismiss"; taskId: string }
  | { type: "clear" };

export const finished = (t: Pick<Task, "state">) => t.state === "done" || t.state === "failed" || t.state === "cancelled";

export function transfersReducer(s: TransfersState, a: TransfersAction): TransfersState {
  switch (a.type) {
    case "start": {
      // The task's first event can arrive before TransferStart returns.
      const known = s.tasks.find((t) => t.taskId === a.taskId);
      const info = { label: a.label, to: a.to, toDir: a.toDir };
      return {
        // One that already finished landed before its destination was known.
        landed: known && finished(known) ? { to: a.to, toDir: a.toDir, n: (s.landed?.n ?? 0) + 1 } : s.landed,
        tasks: known
          ? s.tasks.map((t) => (t.taskId === a.taskId ? { ...t, ...info } : t))
          : [...s.tasks, { taskId: a.taskId, state: "running", bytes: 0, total: 0, files: 0, fileCount: 0, ...info }],
      };
    }
    case "event": {
      const e = a.event;
      const old = s.tasks.find((t) => t.taskId === e.taskId);
      // Each event is the whole state, so a conflict that was answered is gone.
      const task: Task = { label: "", to: "", toDir: "", ...old, ...e, conflict: e.conflict };
      const tasks = old ? s.tasks.map((t) => (t.taskId === e.taskId ? task : t)) : [...s.tasks, task];
      const landed = finished(e) && old && !finished(old) ? { to: task.to, toDir: task.toDir, n: (s.landed?.n ?? 0) + 1 } : s.landed;
      return { tasks, landed };
    }
    case "dismiss":
      return { ...s, tasks: s.tasks.filter((t) => t.taskId !== a.taskId) };
    case "clear":
      return { ...s, tasks: s.tasks.filter((t) => !finished(t)) };
  }
}

/** Tasks still going, and how many of them wait on a conflict answer. */
export function summary(s: TransfersState) {
  const active = s.tasks.filter((t) => !finished(t));
  return { active: active.length, waiting: active.filter((t) => t.state === "conflict").length };
}

/** What the transfer provider offers (src/state/TransfersProvider.tsx). */
export type Transfers = TransfersState & {
  /** Copies `paths` of one source into the folder `toDir` of another. */
  start: (from: string, paths: string[], to: string, toDir: string, label: string) => Promise<void>;
  cancel: (taskId: string) => void;
  answer: (requestId: string, choice: ConflictChoice, all: boolean) => void;
  dismiss: (taskId: string) => void;
  clear: () => void;
  /** The transfer panel in the shell's drawer. */
  open: boolean;
  setOpen: (open: boolean) => void;
};

/** The provider's functions, which never change, so calling them does not
 *  re-render a component on every progress event. */
export type TransferActions = Pick<Transfers, "start" | "cancel" | "answer" | "dismiss" | "clear" | "setOpen">;
/** The panel's open flag and the status bar's counts, a new value only when one changes. */
export type TransfersPanel = { open: boolean } & ReturnType<typeof summary>;

export const TransfersCtx = createContext<Transfers | null>(null);
export const TransferActionsCtx = createContext<TransferActions | null>(null);
export const TransfersPanelCtx = createContext<TransfersPanel | null>(null);
/** The last finished task's destination, a new value only when one lands. */
export const TransferLandedCtx = createContext<TransfersState["landed"]>(null);
export const useTransfers = () => useContext(TransfersCtx)!;
export const useTransferActions = () => useContext(TransferActionsCtx)!;
export const useTransfersPanel = () => useContext(TransfersPanelCtx)!;
export const useTransferLanded = () => useContext(TransferLandedCtx);
