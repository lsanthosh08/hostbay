import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { Input } from "./Field";

// The webview returns null for window.prompt/confirm (no JS panel delegate),
// so native dialogs silently fail. This provides in-app promise-based
// prompt() and confirm() replacements on the kit's Dialog.

type ConfirmOpts = { focusConfirm?: boolean; confirmLabel?: string; cancelLabel?: string };

type PromptReq = ConfirmOpts & {
  id: number;
  kind: "prompt" | "confirm";
  title: string;
  defaultValue: string;
  password?: boolean;
  resolve: (v: string | null) => void;
};

type Ctx = {
  prompt: (title: string, defaultValue?: string, password?: boolean) => Promise<string | null>;
  // Cancel has focus unless focusConfirm, so Enter never confirms a delete.
  confirm: (title: string, opts?: ConfirmOpts) => Promise<boolean>;
};

const PromptCtx = createContext<Ctx>({
  prompt: async () => null,
  confirm: async () => false,
});
export const usePrompt = () => useContext(PromptCtx);

export function PromptProvider({ children }: { children: ReactNode }) {
  // One dialog at a time: a question asked while another is open (the helper
  // and update prompts both ask at start) waits its turn instead of
  // replacing it and leaving the first one unanswered.
  const [queue, setQueue] = useState<PromptReq[]>([]);
  const next = useRef(0);
  const ask = useCallback((r: Omit<PromptReq, "id">) => setQueue((q) => [...q, { ...r, id: next.current++ }]), []);

  const prompt = useCallback(
    (title: string, defaultValue = "", password = false) =>
      new Promise<string | null>((resolve) => ask({ kind: "prompt", title, defaultValue, password, resolve })),
    [ask]
  );
  const confirm = useCallback(
    (title: string, opts?: ConfirmOpts) =>
      new Promise<boolean>((resolve) =>
        ask({
          kind: "confirm",
          title,
          defaultValue: "",
          ...opts,
          resolve: (v) => resolve(v !== null),
        })
      ),
    [ask]
  );

  const req = queue[0];
  const close = (result: string | null) => {
    req?.resolve(result);
    setQueue((q) => q.filter((r) => r.id !== req?.id));
  };

  return (
    <PromptCtx.Provider value={{ prompt, confirm }}>
      {children}
      {req && <PromptDialog key={req.id} req={req} close={close} />}
    </PromptCtx.Provider>
  );
}

function PromptDialog({ req, close }: { req: PromptReq; close: (result: string | null) => void }) {
  const [value, setValue] = useState(req.defaultValue);
  return (
    // Typed text counts as an edit: a stray Escape or click keeps it.
    <Dialog label={req.title} size="sm" onClose={() => close(null)} dismissible={req.kind === "confirm" || value === req.defaultValue}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          close(req.kind === "confirm" ? "" : value);
        }}
      >
        <p className="text-sm font-medium mb-3 whitespace-pre-line allow-select">{req.title}</p>
        {req.kind === "prompt" && (
          <Input
            autoFocus
            type={req.password ? "password" : "text"}
            aria-label={req.title}
            className="w-full mb-4"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
        )}
        <div className="flex gap-2 justify-end">
          <Button variant="ghost" type="button" onClick={() => close(null)}>
            {req.cancelLabel ?? t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" autoFocus={req.focusConfirm}>
            {req.kind === "confirm" ? (req.confirmLabel ?? t("common.confirm")) : t("common.ok")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
