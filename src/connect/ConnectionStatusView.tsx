import { useEffect, useState, type ReactNode } from "react";
import * as api from "../api";
import { Fingerprint, isCert, RemoveKeyForm, trustQuestion } from "../components/TrustDialog";
import { useHostManager } from "../components/HostManager";
import { t, type MessageKey } from "../i18n";
import { useHosts } from "../state/hosts";
import { sessionAddress, useSessions, type Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { Field, Input } from "../ui/Field";
import { RefreshIcon } from "../ui/icons";
import { Spinner } from "../ui/kit";
import { StatusDot } from "../ui/StatusDot";
import { answerTrust, removeKey } from "./useConnect";

// How long a woken host gets to boot before the connect runs again.
const WAKE_WAIT = 25;

/** A dialing session's tab body: the connect's progress with Cancel, then
 *  whatever the router asks of the user, in place, until they act (AE16). */
export function ConnectionStatusView({ c }: { c: Connection }) {
  const { retry, disconnect, wakeHost, showSettings, cancelPrompt } = useSessions();
  const manager = useHostManager();
  const { reloadHosts } = useHosts();
  const { step, host } = c;
  const close = () => disconnect(c.sessionId);
  const cancel = () => cancelPrompt(c.sessionId);
  const again = () => retry(c.sessionId);
  const edit = (
    <Button variant="ghost" onClick={() => manager.editHost(host)}>
      {t("connect.editHost")}
    </Button>
  );

  // Wake-on-LAN: a visible countdown, then one retry; Cancel stops it.
  const [wait, setWait] = useState<number | null>(null);
  useEffect(() => {
    if (wait === null) return;
    if (wait === 0) {
      setWait(null);
      again();
      return;
    }
    const timer = setTimeout(() => setWait(wait - 1), 1000);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wait]);

  const [password, setPassword] = useState("");
  const [removing, setRemoving] = useState(false);
  const [removed, setRemoved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // A new step starts with a clean form.
  useEffect(() => {
    setPassword("");
    setRemoving(false);
    setError("");
  }, [step]);

  let title: string;
  let body: ReactNode = step && <p className="text-sm allow-select break-words">{step.message}</p>;
  let actions: ReactNode;
  switch (step?.kind) {
    case undefined:
      title = t("connect.connecting", { name: host.alias });
      body = null;
      actions = <Button onClick={close}>{t("common.cancel")}</Button>;
      break;
    case "trust":
      title = isCert(step.prompt) ? t("trust.titleCert") : t("trust.title");
      body = (
        <>
          <p className="text-sm">{trustQuestion(step.prompt)}</p>
          <Fingerprint>{`${step.prompt.keyType} ${step.prompt.fingerprint}`}</Fingerprint>
        </>
      );
      actions = (
        <>
          <Button
            variant="primary"
            onClick={() => answerTrust(step.prompt, true).then(again, (e) => setError(api.errorInfo(e).message))}
          >
            {t("trust.confirm")}
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              answerTrust(step.prompt, false).catch(() => {});
              cancel();
            }}
          >
            {t("common.cancel")}
          </Button>
        </>
      );
      break;
    case "keyChanged": {
      // A refusal: never Retry or accept. Removing the old key needs the
      // typed host name and OS verification; only then can it connect again.
      const { change } = step;
      const cert = !!change.kind?.endsWith("Cert");
      title = cert ? t("connect.certChanged.title") : t("connect.keyChanged.title");
      body = (
        <>
          <p className="text-sm allow-select break-words">{step.message}</p>
          <Fingerprint>
            {`${t("connect.keyChanged.saved")} ${change.knownFingerprints.split(",").join(", ")}\n${t("connect.keyChanged.now")} ${[change.keyType, change.fingerprint].filter(Boolean).join(" ")}`}
          </Fingerprint>
          {removed ? (
            <p className="text-sm text-success">{t("connect.keyChanged.removed", { host: change.host })}</p>
          ) : (
            // A certificate's refusal message already says what to check.
            !cert && <p className="text-sm text-muted">{t("connect.keyChanged.advice")}</p>
          )}
          {removing && !removed && (
            <RemoveKeyForm
              host={change.host}
              onCancel={() => setRemoving(false)}
              onRemove={async (typed) => {
                await removeKey(change, typed);
                setRemoved(true);
              }}
            />
          )}
        </>
      );
      actions = removed ? (
        <Button variant="primary" onClick={again}>
          {t("connect.connectAgain")}
        </Button>
      ) : (
        <>
          {change.removeId && !removing && (
            <Button variant="danger" onClick={() => setRemoving(true)}>
              {t("trust.removeStart")}
            </Button>
          )}
          {edit}
          <Button variant="ghost" onClick={close}>
            {t("common.close")}
          </Button>
        </>
      );
      break;
    }
    case "secret": {
      const needs = step.host;
      title = t("connect.secret.title");
      body = (
        <form
          id={`secret-${c.sessionId}`}
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            try {
              // Saved sealed, then the connect runs again with it.
              await api.saveHost({ ...needs, password });
              await reloadHosts();
              again();
            } catch (err) {
              setError(api.errorInfo(err).message);
            } finally {
              setBusy(false);
            }
          }}
        >
          {/* Why it asks: missing, or refused by the server (a VNC server says so). */}
          <p className="text-sm allow-select break-words mb-3">{step.message}</p>
          <Field
            label={t("connect.secret.label", { name: needs.alias, address: `${needs.user ? `${needs.user}@` : ""}${needs.hostname}` })}
            error={error}
          >
            <Input type="password" className="w-full" autoFocus value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
        </form>
      );
      actions = (
        <>
          <Button variant="primary" type="submit" form={`secret-${c.sessionId}`} disabled={!password || busy}>
            {t("connect.connect")}
          </Button>
          <Button variant="ghost" onClick={cancel}>
            {t("common.cancel")}
          </Button>
        </>
      );
      break;
    }
    case "proxySecret":
      title = t("connect.proxySecret.title");
      body = <ProxySecretForm id={`secret-${c.sessionId}`} proxyId={step.proxyId} message={step.message} onSaved={again} />;
      actions = (
        <>
          <Button variant="primary" type="submit" form={`secret-${c.sessionId}`}>
            {t("connect.connect")}
          </Button>
          {edit}
          <Button variant="ghost" onClick={cancel}>
            {t("common.cancel")}
          </Button>
        </>
      );
      break;
    case "kerberos":
    case "proxyHeld":
    case "proxyMissing":
    case "vpnHeld":
    case "vpnChoose": {
      // None of these offers to connect around the proxy or VPN (KTD22).
      const [titleKey, hintKey] = (
        {
          kerberos: ["connect.kerberos.title", "connect.kerberos.hint"],
          proxyHeld: ["connect.proxyHeld.title", "connect.proxyHeld.hint"],
          proxyMissing: ["connect.proxyMissing.title", "connect.proxyMissing.hint"],
          vpnHeld: ["connect.vpnHeld.title", "connect.vpnHeld.hint"],
          vpnChoose: ["connect.vpnChoose.title", "connect.vpnChoose.hint"],
        } satisfies Record<string, [MessageKey, MessageKey]>
      )[step.kind];
      title = t(titleKey);
      body = (
        <>
          {body}
          <p className="text-sm text-muted">{t(hintKey)}</p>
        </>
      );
      actions = (
        <>
          {(step.kind === "proxyHeld" || step.kind === "vpnHeld") && (
            <Button variant="primary" onClick={() => showSettings("sync")}>
              {t("connect.proxyHeld.open")}
            </Button>
          )}
          {step.kind === "proxyMissing" || step.kind === "vpnChoose" ? (
            <Button variant="primary" onClick={() => manager.editHost(host)}>
              {t("connect.editHost")}
            </Button>
          ) : (
            <Button variant={step.kind === "kerberos" ? "primary" : "default"} icon={RefreshIcon} onClick={again}>
              {t("connect.retry")}
            </Button>
          )}
          <Button variant="ghost" onClick={close}>
            {t("common.close")}
          </Button>
        </>
      );
      break;
    }
    case "unlock":
      title = t("connect.unlock.title");
      actions = (
        <>
          <Button variant="primary" onClick={again}>
            {t("connect.unlock.retry")}
          </Button>
          <Button variant="ghost" onClick={close}>
            {t("common.close")}
          </Button>
        </>
      );
      break;
    case "wake":
      title = t("connect.wake.title", { name: host.alias });
      body = (
        <>
          {body}
          {wait !== null && <p className="text-sm text-muted tabular-nums">{t("connect.wake.waiting", { n: wait })}</p>}
        </>
      );
      actions =
        wait !== null ? (
          <Button onClick={() => setWait(null)}>{t("common.cancel")}</Button>
        ) : (
          <>
            <Button
              variant="primary"
              onClick={async () => {
                await wakeHost(host);
                setWait(WAKE_WAIT);
              }}
            >
              {t("connect.wake.send")}
            </Button>
            <Button onClick={again}>{t("connect.retry")}</Button>
            {edit}
          </>
        );
      break;
    case "verify":
    case "helperMissing":
    case "failure":
    case "reconnect":
      title =
        step.kind === "verify"
          ? t("connect.verify.title")
          : step.kind === "helperMissing"
            ? t("connect.helper.title")
            : step.kind === "failure" && step.proxy
              ? t("connect.proxyFailed.title")
              : t("connect.failed.title");
      actions = (
        <>
          {step.kind !== "helperMissing" && (
            <Button variant="primary" icon={RefreshIcon} onClick={again}>
              {t("connect.retry")}
            </Button>
          )}
          {edit}
          <Button variant="ghost" onClick={close}>
            {t("common.close")}
          </Button>
        </>
      );
      break;
  }

  return (
    <div className="flex-1 min-h-0 overflow-auto flex items-center justify-center p-6">
      <section aria-live="polite" aria-label={title} className="w-full max-w-lg flex flex-col gap-4">
        <div className="flex items-center gap-2.5">
          {step ? <StatusDot status="error" /> : <Spinner />}
          <h2 className="text-base font-semibold">{title}</h2>
        </div>
        <p className="text-sm text-muted font-mono truncate allow-select -mt-2">{sessionAddress(c)}</p>
        {body}
        {error && step?.kind !== "secret" && <p className="text-sm text-danger">{error}</p>}
        <div className="flex flex-wrap gap-2">{actions}</div>
      </section>
    </div>
  );
}

/** Over a live session's views after its connection dropped: one Reconnect
 *  rebuilds the shared connection, and each view reopens its channel. */
export function ReconnectBanner({ c }: { c: Connection }) {
  const { reconnect } = useSessions();
  const [busy, setBusy] = useState(false);
  return (
    <div role="alert" className="shrink-0 flex items-center gap-3 px-3 py-2 bg-warning-soft text-sm">
      <StatusDot status="error" />
      <span className="flex-1 min-w-0 truncate allow-select" title={c.step?.message}>
        {t("connect.dropped.banner", { name: c.host.alias })} {c.step?.message}
      </span>
      <Button
        size="sm"
        variant="primary"
        icon={busy ? undefined : RefreshIcon}
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          await reconnect(c.sessionId);
          setBusy(false);
        }}
      >
        {busy ? <Spinner /> : t("connect.reconnect")}
      </Button>
    </div>
  );
}

/** Asks for a proxy's password and saves it sealed to the proxy, then runs
 *  `onSaved` (the connect again). The saved one is never shown. */
function ProxySecretForm({ id, proxyId, message, onSaved }: { id: string; proxyId: string; message: string; onSaved: () => void }) {
  // undefined while loading; null when the proxy no longer exists.
  const [proxy, setProxy] = useState<api.Proxy | null | undefined>(undefined);
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  useEffect(() => {
    api
      .listProxies()
      .then((ps) => setProxy(ps.find((p) => p.id === proxyId) ?? null))
      .catch((e) => setError(api.errorInfo(e).message));
  }, [proxyId]);
  return (
    <form
      id={id}
      onSubmit={async (e) => {
        e.preventDefault();
        if (!proxy || !password) return;
        try {
          await api.saveProxy({ ...proxy, password });
          onSaved();
        } catch (err) {
          setError(api.errorInfo(err).message);
        }
      }}
    >
      <p className="text-sm allow-select break-words mb-3">{message}</p>
      {proxy === null ? (
        <p className="text-sm text-danger">{t("connect.proxySecret.gone")}</p>
      ) : (
        <Field
          label={t("connect.proxySecret.label", { name: proxy?.name ?? "", address: proxy ? `${proxy.host}:${proxy.port}` : "" })}
          error={error}
        >
          <Input
            type="password"
            className="w-full"
            autoFocus
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
      )}
    </form>
  );
}
