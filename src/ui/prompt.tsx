import { createContext, useCallback, useContext, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { Input } from "./Field";

// The webview returns null for window.prompt/confirm (no JS panel delegate),
// so native dialogs silently fail. This provides in-app promise-based
// prompt() and confirm() replacements on the kit's Dialog.

type PromptReq = {
  kind: "prompt" | "confirm";
  title: string;
  defaultValue: string;
  password?: boolean;
  focusConfirm?: boolean;
  resolve: (v: string | null) => void;
};

type Ctx = {
  prompt: (title: string, defaultValue?: string, password?: boolean) => Promise<string | null>;
  // Cancel has focus unless focusConfirm, so Enter never confirms a delete.
  confirm: (title: string, opts?: { focusConfirm?: boolean }) => Promise<boolean>;
};

const PromptCtx = createContext<Ctx>({
  prompt: async () => null,
  confirm: async () => false,
});
export const usePrompt = () => useContext(PromptCtx);

export function PromptProvider({ children }: { children: ReactNode }) {
  const [req, setReq] = useState<PromptReq | null>(null);
  const [value, setValue] = useState("");

  const prompt = useCallback(
    (title: string, defaultValue = "", password = false) =>
      new Promise<string | null>((resolve) => {
        setValue(defaultValue);
        setReq({ kind: "prompt", title, defaultValue, password, resolve });
      }),
    []
  );
  const confirm = useCallback(
    (title: string, opts?: { focusConfirm?: boolean }) =>
      new Promise<boolean>((resolve) => {
        setReq({
          kind: "confirm",
          title,
          defaultValue: "",
          focusConfirm: opts?.focusConfirm,
          resolve: (v) => resolve(v !== null),
        });
      }),
    []
  );

  const close = (result: string | null) => {
    req?.resolve(result);
    setReq(null);
  };

  return (
    <PromptCtx.Provider value={{ prompt, confirm }}>
      {children}
      {req && (
        // Typed text counts as an edit: a stray Escape or click keeps it.
        <Dialog
          label={req.title}
          size="sm"
          onClose={() => close(null)}
          dismissible={req.kind === "confirm" || value === req.defaultValue}
        >
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
                {t("common.cancel")}
              </Button>
              <Button variant="primary" type="submit" autoFocus={req.focusConfirm}>
                {req.kind === "confirm" ? t("common.confirm") : t("common.ok")}
              </Button>
            </div>
          </form>
        </Dialog>
      )}
    </PromptCtx.Provider>
  );
}
