import { useEffect, useMemo, useReducer, useState, type ReactNode } from "react";
import * as api from "../api";
import { useToast } from "../ui/toast";
import {
  initialTransfers,
  summary,
  transfersReducer,
  TransferActionsCtx,
  TransferLandedCtx,
  TransfersCtx,
  TransfersPanelCtx,
  type TransferActions,
  type Transfers,
} from "./transfers";

/** Every transfer task, from the one app-wide "transfer" event. A task that
 *  waits on a conflict opens the panel, which asks on that task's row. */
export function TransfersProvider({ children }: { children: ReactNode }) {
  const { push } = useToast();
  const [state, dispatch] = useReducer(transfersReducer, initialTransfers);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    const off = api.onTransfer((event) => {
      dispatch({ type: "event", event });
      if (event.state === "conflict") setOpen(true);
    });
    return () => {
      off.then((f) => f());
    };
  }, []);

  const actions = useMemo<TransferActions>(() => {
    const fail = (e: unknown) => push("error", api.errorInfo(e).message);
    return {
      start: async (from, paths, to, toDir, label) => {
        try {
          const taskId = await api.transferStart(from, paths, to, toDir);
          dispatch({ type: "start", taskId, label, to, toDir });
          setOpen(true);
        } catch (e) {
          fail(e);
        }
      },
      cancel: (taskId) => api.transferCancel(taskId).catch(fail),
      answer: (requestId, choice, all) => api.transferAnswer(requestId, choice, all).catch(fail),
      dismiss: (taskId) => dispatch({ type: "dismiss", taskId }),
      clear: () => dispatch({ type: "clear" }),
      setOpen,
    };
  }, [push]);
  const value = useMemo<Transfers>(() => ({ ...state, ...actions, open }), [state, actions, open]);
  const { active, waiting } = summary(state);
  const panel = useMemo(() => ({ open, active, waiting }), [open, active, waiting]);

  return (
    <TransferActionsCtx.Provider value={actions}>
      <TransfersPanelCtx.Provider value={panel}>
        <TransferLandedCtx.Provider value={state.landed}>
          <TransfersCtx.Provider value={value}>{children}</TransfersCtx.Provider>
        </TransferLandedCtx.Provider>
      </TransfersPanelCtx.Provider>
    </TransferActionsCtx.Provider>
  );
}
