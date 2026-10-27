import { useEffect, useState } from "react";
import * as api from "../api";
import { t } from "../i18n";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Input } from "../ui/Field";

// Hostbay master password (Linux user verification, KTD7). Setting one is
// its own action here; the prompt below only answers the backend and never
// offers setup, so nobody at an unlocked session can create one and pass.

const MIN_LENGTH = 8; // internal/secrets.MinMasterPasswordLen

/** Sets the master password once. The backend refuses to replace it. */
export function MasterPasswordSetupDialog({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const mismatch = confirm !== "" && confirm !== password;
  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await api.setMasterPassword(password);
      onDone();
    } catch (e) {
      setError(api.errorInfo(e).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title={t("master.setTitle")} size="sm" onClose={onCancel} dismissible={password === "" && confirm === ""}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <p className="text-xs text-muted mb-3">{t("master.setHint")}</p>
        <Input
          autoFocus
          type="password"
          className="w-full mb-2"
          placeholder={t("master.placeholder", { n: MIN_LENGTH })}
          aria-label={t("settings.masterPassword")}
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <Input
          type="password"
          className="w-full mb-2"
          placeholder={t("master.confirm")}
          aria-label={t("master.confirm")}
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
        />
        {(mismatch || error) && (
          <p className="text-xs text-danger mb-2">{mismatch ? t("master.mismatch") : error}</p>
        )}
        <div className="flex gap-2 justify-end mt-2">
          <Button variant="ghost" type="button" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button
            variant="primary"
            type="submit"
            disabled={busy || [...password].length < MIN_LENGTH || password !== confirm}
          >
            {t("master.set")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/** Answers a backend request for the master password. */
export function MasterPasswordPromptDialog({
  request,
  onAnswer,
}: {
  request: api.MasterPasswordRequest;
  onAnswer: (password: string) => void; // "" cancels
}) {
  const [password, setPassword] = useState("");
  return (
    <Dialog title={t("master.enterTitle")} size="sm" onClose={() => onAnswer("")} dismissible={password === ""}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (password) onAnswer(password);
        }}
      >
        <p className="text-xs text-muted mb-3">{t("master.needs", { reason: request.reason })}</p>
        <Input
          autoFocus
          type="password"
          className="w-full mb-4"
          aria-label={t("settings.masterPassword")}
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <div className="flex gap-2 justify-end">
          <Button variant="ghost" type="button" onClick={() => onAnswer("")}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" disabled={!password}>
            {t("common.ok")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/** Backend calls waiting for the master password (Linux), answered in order. */
export function MasterPasswordPrompts() {
  const [asks, setAsks] = useState<api.MasterPasswordRequest[]>([]);
  useEffect(() => {
    const sub = api.onMasterPasswordPrompt((r) => setAsks((q) => [...q, r]));
    return () => {
      sub.then((off) => off());
    };
  }, []);
  if (!asks[0]) return null;
  return (
    <MasterPasswordPromptDialog
      key={asks[0].requestId}
      request={asks[0]}
      onAnswer={(password) => {
        const r = asks[0];
        setAsks((q) => q.slice(1));
        // An expired prompt already failed its call, which reports the error.
        api.answerMasterPassword(r.requestId, password).catch(() => {});
      }}
    />
  );
}
