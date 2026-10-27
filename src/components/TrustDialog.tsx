import { useState } from "react";
import { errorInfo, type HostKeyPrompt } from "../api";
import { t } from "../i18n";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Field, Input } from "../ui/Field";

/** An RDP server's or file host's certificate, rather than an SSH key. */
export const isCert = (p: HostKeyPrompt) => !!p.kind?.endsWith("Cert");

/** What an unknown key or certificate is, for the trust prompt. */
export function trustQuestion(p: HostKeyPrompt) {
  const vars = { host: p.host, port: p.port };
  if (isCert(p)) return t("trust.rdpCert", vars);
  return p.hop && p.hop !== "0" ? t("trust.jumpKey", vars) : t("trust.hostKey", vars);
}

/** The unknown key's type and fingerprint, to check before trusting it. */
export function Fingerprint({ children }: { children: string }) {
  return (
    <pre className="text-xs bg-raised rounded-chip p-2 whitespace-pre-wrap break-all allow-select">{children}</pre>
  );
}

/** Asks to trust an unknown host key or certificate (Tunnels, Sync, RDP, a
 *  file pane's sign-in;
 *  a session tab asks in place, src/connect/ConnectionStatusView.tsx). */
export function TrustDialog({
  prompt,
  onConfirm,
  onCancel,
}: {
  prompt: HostKeyPrompt;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <Dialog
      title={isCert(prompt) ? t("trust.titleCert") : t("trust.title")}
      onClose={onCancel}
      footer={
        <>
          <Button variant="ghost" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" onClick={onConfirm}>
            {t("trust.confirm")}
          </Button>
        </>
      }
    >
      <p className="text-sm text-muted mb-3">{trustQuestion(prompt)}</p>
      <Fingerprint>{`${prompt.keyType} ${prompt.fingerprint}`}</Fingerprint>
    </Dialog>
  );
}

/** Removing a stored host key: only after typing the host name, and the
 *  backend also asks for OS verification before it touches known_hosts (R13).
 *  `onRemove` rejects to keep the form, e.g. when verification fails. */
export function RemoveKeyForm({
  host,
  onRemove,
  onCancel,
}: {
  host: string;
  onRemove: (typedHost: string) => Promise<void>;
  onCancel: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <form
      className="flex flex-col gap-3"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        setError("");
        try {
          await onRemove(typed);
        } catch (err) {
          setError(errorInfo(err).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Field label={t("trust.removeType", { host })} error={error}>
        <Input
          className="w-full"
          value={typed}
          autoFocus
          spellCheck={false}
          autoComplete="off"
          onChange={(e) => setTyped(e.target.value)}
        />
      </Field>
      <div className="flex gap-2">
        <Button type="submit" variant="danger" disabled={typed !== host || busy}>
          {t("trust.remove")}
        </Button>
        <Button type="button" variant="ghost" onClick={onCancel}>
          {t("common.cancel")}
        </Button>
      </div>
    </form>
  );
}
