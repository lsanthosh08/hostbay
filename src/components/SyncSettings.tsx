import { useCallback, useEffect, useState } from "react";
import {
  errorInfo,
  listProxies,
  syncChangePassphrase,
  syncConfigure,
  syncConfirmHold,
  syncDisable,
  syncNow,
  syncStatus,
  type Proxy,
  type SyncHold,
  type SyncKind,
  type SyncStatus,
  type SyncTarget,
} from "../api";
import { useConnect } from "../connect/useConnect";
import { t, type MessageKey } from "../i18n";
import { Button } from "../ui/Button";
import { Input } from "../ui/Field";
import { Badge, Spinner } from "../ui/kit";
import { Select } from "../ui/Select";
import { Tabs } from "../ui/Tabs";
import { SyncConflicts } from "./SyncConflicts";

const kinds: { kind: SyncKind; label: MessageKey; secret?: MessageKey }[] = [
  { kind: "git", label: "sync.kind.git", secret: "sync.secret.token" },
  { kind: "webdav", label: "sync.kind.webdav", secret: "sync.secret.password" },
  { kind: "folder", label: "sync.kind.folder" },
  { kind: "s3", label: "sync.kind.s3", secret: "sync.secret.s3" },
  { kind: "sftp", label: "sync.kind.sftp", secret: "sync.secret.sftp" },
  { kind: "smb", label: "sync.kind.smb", secret: "sync.secret.password" },
];

// Which target fields each kind uses, with their labels.
const fields: Record<SyncKind, [keyof SyncTarget, MessageKey][]> = {
  git: [["url", "sync.field.repoUrl"], ["branch", "sync.field.branch"], ["user", "sync.field.user"]],
  webdav: [["url", "sync.field.folderUrl"], ["user", "sync.field.user"]],
  folder: [["path", "sync.field.folderPath"]],
  s3: [["url", "sync.field.endpoint"], ["region", "sync.field.region"], ["bucket", "sync.field.bucket"], ["path", "sync.field.keyPrefix"], ["user", "sync.field.accessKey"]],
  sftp: [["host", "sync.field.host"], ["port", "sync.field.port"], ["user", "sync.field.user"], ["path", "sync.field.serverFolder"]],
  smb: [["host", "sync.field.server"], ["share", "sync.field.share"], ["path", "sync.field.shareFolder"], ["user", "sync.field.user"], ["domain", "sync.field.domain"]],
};

// Why sync paused, by the pause code in internal/sync/state.go.
const pausedText: Record<string, MessageKey> = {
  decrypt: "sync.pause.decrypt",
  updateNeeded: "sync.pause.updateNeeded",
  rollback: "sync.pause.rollback",
  passphraseChanged: "sync.pause.passphraseChanged",
  hostKeyChanged: "sync.pause.hostKeyChanged",
  auth: "sync.pause.auth",
  clock: "sync.pause.clock",
};

/** A host hold naming no setting but plaintext, so it keeps the wording
 *  holds had before they named their settings. */
export const plainOnly = (h: SyncHold) => !h.forwardAgent && !h.x11 && !h.algorithms && h.proxyId == null && !h.cluster;

/** What a held host change would do here, one plain sentence per setting
 *  (internal/sync/holds.go). A hold naming nothing is a plaintext one, as
 *  holds were before they named their settings. */
// A cancelled Go dialog or OS verification changes nothing and is no error.
export function ignoreCancel(e: unknown) {
  if (errorInfo(e).code !== "VerifyFailed") throw e;
}

export function holdReasons(h: SyncHold, proxies: Proxy[]): string[] {
  const out: string[] = [];
  const names = h.algorithms ? Object.values(h.algorithms).flatMap((l) => l ?? []) : [];
  if (h.plaintext || plainOnly(h)) out.push(t("sync.hold.plaintext"));
  if (h.cluster) out.push(t("sync.hold.cluster", { url: h.cluster.url }));
  if (h.forwardAgent) out.push(t("sync.hold.forwardAgent"));
  if (h.x11) out.push(t("sync.hold.x11"));
  if (h.algorithms) out.push(t("sync.hold.algorithms", { names: names.join(", ") }));
  // A cluster's "" may be a new cluster that never had a proxy, so its
  // line holds either way.
  if (h.proxyId === "") out.push(t(h.cluster ? "sync.hold.proxyNone" : "sync.hold.proxyRemoved"));
  else if (h.proxyId != null) {
    const p = proxies.find((x) => x.id === h.proxyId);
    out.push(t("sync.hold.proxySet", { proxy: p ? `${p.name} (${p.type} ${p.host}:${p.port})` : t("sync.hold.proxyUnknown") }));
  }
  return out;
}

/** Sync settings, status and conflicts (R35–R37), in Settings > Sync. */
export function SyncSettings() {
  const [status, setStatus] = useState<SyncStatus | null>(null);
  const [target, setTarget] = useState<SyncTarget>({ kind: "git" });
  const [secret, setSecret] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  // An SFTP target's unknown host key asks to trust it, then reruns the action.
  const { fail, dialog } = useConnect();
  const [tab, setTab] = useState<"settings" | "conflicts">("settings");
  // To name the proxy a held host change points at.
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const held = !!status?.holds.length;
  useEffect(() => {
    if (held) listProxies().then(setProxies).catch(() => {});
  }, [held]);

  const refresh = useCallback(() => {
    syncStatus()
      .then((s) => {
        setStatus(s);
        if (s.target?.kind) setTarget(s.target);
      })
      .catch((e) => setError(errorInfo(e).message));
  }, []);
  useEffect(refresh, [refresh]);

  const run = async (fn: () => Promise<string | void>) => {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const msg = await fn();
      if (msg) setMessage(msg);
    } catch (e) {
      fail(e, () => run(fn), { onStep: (step) => setError(step.message) });
    } finally {
      setBusy(false);
      refresh();
    }
  };

  const sync = (choice: "" | "remote" | "merge" = "") =>
    run(async () => {
      const r = await syncNow(choice);
      const retries = r.retries ? [t("sync.retries", { retries: r.retries })] : [];
      return [t("sync.done", { received: r.received }), ...retries, ...r.notes].join(" ");
    });

  const save = () =>
    run(async () => {
      await syncConfigure({ ...target, port: target.port ? Number(target.port) : undefined }, secret, passphrase);
      setSecret("");
      setPassphrase("");
      const r = await syncNow();
      return t("sync.isOn", { received: r.received });
    });

  const set = (k: keyof SyncTarget, v: string) =>
    setTarget((t) => ({ ...t, [k]: k === "port" ? (v ? Number(v) : undefined) : v }));
  const kind = kinds.find((k) => k.kind === target.kind) ?? kinds[0];

  return (
    <div className="flex flex-col gap-4">
      <div>
        {status?.enabled ? (
          <Badge tone={status.paused ? "warn" : "ok"}>{t(status.paused ? "sync.badge.paused" : "sync.badge.on")}</Badge>
        ) : (
          <Badge>{t("sync.badge.off")}</Badge>
        )}
      </div>
      <Tabs
        label={t("sync.tabs")}
        value={tab}
        onChange={setTab}
        className="-mt-2 border-b border-line"
        items={[
          { id: "settings", label: t("sync.tab.settings") },
          { id: "conflicts", label: status?.conflicts ? t("sync.tab.conflictsCount", { count: status.conflicts }) : t("sync.tab.conflicts") },
        ]}
      />

      {tab === "conflicts" ? (
        <SyncConflicts onChanged={refresh} />
      ) : (
        <>
          {status?.enabled && (
            <div className="text-sm flex flex-col gap-2">
              <p className="opacity-80">
                {t("sync.lastSync", { when: status.lastSync ? new Date(status.lastSync).toLocaleString() : t("sync.never") })}
              </p>
              {status.paused && (
                <p className="text-warning">{pausedText[status.paused] ? t(pausedText[status.paused]) : status.lastError}</p>
              )}
              {!status.paused && status.lastError && !status.needsChoice && <p className="text-danger">{status.lastError}</p>}
              {status.needsChoice && (
                <div className="rounded border border-line p-3 flex flex-col gap-2">
                  <p>{t("sync.choice")}</p>
                  <div className="flex gap-2">
                    <Button variant="primary" disabled={busy} onClick={() => sync("merge")}>
                      {t("sync.choice.merge")}
                    </Button>
                    <Button variant="danger" disabled={busy} onClick={() => sync("remote")}>
                      {t("sync.choice.remote")}
                    </Button>
                  </div>
                </div>
              )}
              {status.holds.map((h) => {
                const reasons = h.kind === "host" ? holdReasons(h, proxies) : [];
                const plain = plainOnly(h);
                return (
                  <div key={h.id} className="rounded border border-warning/50 p-3 flex flex-col gap-2">
                    {h.kind === "proxy" && h.endpoint ? (
                      <p>
                        {t("sync.hold.proxy", {
                          name: h.name,
                          endpoint: `${h.endpoint.type} ${h.endpoint.host}:${h.endpoint.port}`,
                        })}
                      </p>
                    ) : h.kind === "host" && plain ? (
                      <p>{t("sync.hold.host", { name: h.name })}</p>
                    ) : h.kind === "host" ? (
                      <>
                        <p>{t("sync.hold.hostChanged", { name: h.name })}</p>
                        <ul className="list-disc pl-5">
                          {reasons.map((r) => (
                            <li key={r} className="allow-select break-words">
                              {r}
                            </li>
                          ))}
                        </ul>
                      </>
                    ) : h.kind === "aimode" ? (
                      <p>{t("sync.hold.aimode", { mode: h.mode ?? "" })}</p>
                    ) : h.kind === "postlogin" ? (
                      <p>{t("sync.hold.postlogin", { name: h.name })}</p>
                    ) : !h.kind ? (
                      <p>{t("sync.hold.tunnel", { name: h.name })}</p>
                    ) : (
                      <p>{t("sync.hold.unknown", { name: h.name })}</p>
                    )}
                    {/* Go cannot resolve a kind from a newer Hostbay; it stays held. */}
                    {(!h.kind || ["host", "proxy", "aimode", "postlogin"].includes(h.kind)) && (
                      <div className="flex gap-2">
                        <Button onClick={() => run(() => syncConfirmHold(h.id, true).catch(ignoreCancel))}>{t("sync.hold.allow")}</Button>
                        <Button variant="ghost" onClick={() => run(() => syncConfirmHold(h.id, false))}>
                          {h.kind === "proxy"
                            ? t("sync.hold.keepProxy")
                            : h.kind === "host"
                              ? plain
                                ? t("sync.hold.keepEncrypted")
                                : t("sync.hold.keepHost")
                              : h.kind === "aimode"
                                ? t("sync.hold.keepMode")
                                : h.kind === "postlogin"
                                  ? t("sync.hold.removeRule")
                                  : t("sync.hold.keepLoopback")}
                        </Button>
                      </div>
                    )}
                  </div>
                );
              })}
              <div className="flex gap-2">
                <Button variant="primary" disabled={busy} onClick={() => sync()}>
                  {busy ? <Spinner /> : t("sync.now")}
                </Button>
                <Button variant="ghost" disabled={busy} onClick={() => run(async () => (await syncDisable(), t("sync.isOff")))}>
                  {t("sync.turnOff")}
                </Button>
              </div>
            </div>
          )}

          <div className="flex flex-col gap-2 text-sm">
            <label className="flex flex-col gap-1">
              {t("sync.target")}
              <Select value={target.kind} onChange={(e) => setTarget({ kind: e.target.value as SyncKind })}>
                {kinds.map((k) => (
                  <option key={k.kind} value={k.kind}>
                    {t(k.label)}
                  </option>
                ))}
              </Select>
            </label>
            {fields[target.kind].map(([k, label]) => (
              <label key={k} className="flex flex-col gap-1">
                {t(label)}
                <Input value={target[k] ?? ""} onChange={(e) => set(k, e.target.value)} />
              </label>
            ))}
            {kind.secret && (
              <label className="flex flex-col gap-1">
                {t(kind.secret)}
                <Input
                  type="password"
                  value={secret}
                  placeholder={status?.enabled ? t("sync.keepSecret") : ""}
                  onChange={(e) => setSecret(e.target.value)}
                />
              </label>
            )}
            <label className="flex flex-col gap-1">
              {t("sync.passphrase")}
              <Input type="password" value={passphrase} onChange={(e) => setPassphrase(e.target.value)} />
              <span className="text-xs opacity-60">{t("sync.passphraseHint")}</span>
            </label>
            <div className="flex gap-2">
              <Button variant="primary" disabled={busy || !passphrase} onClick={save}>
                {status?.enabled ? t("sync.saveAndSync") : t("sync.turnOn")}
              </Button>
              {status?.enabled && (
                <Button
                  disabled={busy || !passphrase}
                  onClick={() =>
                    run(async () => {
                      await syncChangePassphrase(passphrase);
                      setPassphrase("");
                      return t("sync.passphraseChanged");
                    })
                  }
                >
                  {t("sync.changePassphrase")}
                </Button>
              )}
            </div>
          </div>
          {message && <p className="text-sm text-success">{message}</p>}
          {error && <p className="text-sm text-danger">{error}</p>}
        </>
      )}
      {dialog}
    </div>
  );
}
