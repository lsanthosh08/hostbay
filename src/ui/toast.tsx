import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { Button } from "./Button";
import { IconButton } from "./IconButton";
import { CloseIcon, ErrorIcon, InfoIcon, SuccessIcon } from "./icons";
import { layer } from "./layers";
import { notices } from "./notices";

type Kind = "info" | "error" | "success";
type Action = { label: string; onClick: () => void };
type Toast = { id: number; kind: Kind; msg: string; action?: Action };
type Ctx = { push: (kind: Kind, msg: string, opts?: { action?: Action }) => void };

const ToastCtx = createContext<Ctx>({ push: () => {} });
export const useToast = () => useContext(ToastCtx);

const look = {
  info: [InfoIcon, "text-muted"],
  success: [SuccessIcon, "text-success"],
  error: [ErrorIcon, "text-danger"],
} as const;

/** Errors stay until dismissed, so their text is never lost; the rest fade
 *  after 5 s. Toasts sit above dialogs, and above a drawer's footer
 *  (styles.css) so they never cover its Save. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const dismiss = useCallback((id: number) => setToasts((t) => t.filter((x) => x.id !== id)), []);
  const push = useCallback<Ctx["push"]>(
    (kind, msg, opts) => {
      const id = Date.now() + Math.floor(Math.random() * 1000);
      setToasts((t) => [...t, { id, kind, msg, action: opts?.action }]);
      notices.add({ id: `toast:${id}`, kind, msg });
      if (kind !== "error") setTimeout(() => dismiss(id), 5000);
    },
    [dismiss]
  );

  return (
    <ToastCtx.Provider value={{ push }}>
      {children}
      <div
        aria-live="polite"
        data-toasts
        className={`fixed bottom-4 right-4 ${layer.toast} flex flex-col gap-2 w-96 max-w-[90vw]`}
      >
        {toasts.map((x) => {
          const [Icon, tone] = look[x.kind];
          return (
            <div
              key={x.id}
              role={x.kind === "error" ? "alert" : "status"}
              className="panel rounded-control shadow-float pl-3 pr-1.5 py-2 text-sm flex items-start gap-2"
            >
              <Icon className={`size-4 mt-0.5 shrink-0 ${tone}`} aria-hidden />
              <p className="flex-1 min-w-0 py-0.5 break-words allow-select">{x.msg}</p>
              {x.action && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    dismiss(x.id);
                    x.action!.onClick();
                  }}
                >
                  {x.action.label}
                </Button>
              )}
              <IconButton size="sm" label={t("common.dismiss")} icon={CloseIcon} onClick={() => dismiss(x.id)} />
            </div>
          );
        })}
      </div>
    </ToastCtx.Provider>
  );
}

/** Prefixes errors raised inside with the tab's name, so one from a tab in
 *  the background is not taken for the tab on screen. */
export function ToastScope({ name, children }: { name: string; children: ReactNode }) {
  const { push } = useToast();
  // push stays the same while name does: views keep it in effect deps.
  const value = useMemo<Ctx>(() => ({ push: (kind, msg, opts) => push(kind, kind === "error" ? `${name}: ${msg}` : msg, opts) }), [push, name]);
  return <ToastCtx.Provider value={value}>{children}</ToastCtx.Provider>;
}

/** Wrap an async action; on throw, show a toast with the message. */
export function useAsyncToast() {
  const { push } = useToast();
  return useCallback(
    async <T,>(fn: () => Promise<T>, okMsg?: string): Promise<T | undefined> => {
      try {
        const r = await fn();
        if (okMsg) push("success", okMsg);
        return r;
      } catch (e) {
        push("error", e instanceof Error ? e.message : String(e));
        return undefined;
      }
    },
    [push]
  );
}
