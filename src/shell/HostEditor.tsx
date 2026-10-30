import { useEffect, useRef, useState, type ReactNode } from "react";
import * as api from "../api";
import { hostKind, isDatabaseHost, isFileHost, type Folder, type Host, type HostKeyPrompt, type HostKind, type Proxy, type SshAlgorithms } from "../api";
import { TagEditor } from "../components/TagEditor";
import { TrustDialog } from "../components/TrustDialog";
import { reloadVpns } from "../state/vpns";
import { answerTrust } from "../connect/useConnect";
import { open } from "../dialogs";
import { t, type MessageKey } from "../i18n";
import { useHosts } from "../state/hosts";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Field, Input } from "../ui/Field";
import { usePrompt } from "../ui/prompt";
import { SegmentedControl } from "../ui/SegmentedControl";
import { Select } from "../ui/Select";
import { useAsyncToast, useToast } from "../ui/toast";
import { asksPasswordAgain, formHost, passwordForKind, SQL_SERVERS, unencrypted, validateHost, type HostField } from "./hostForm";
import { SSH_ALGORITHMS } from "./sshAlgorithms";
import { PostLoginEditor } from "./PostLoginEditor";
import { VpnField } from "./VpnField";
import { TERMINAL_KINDS } from "./settings/Logging";

const KEY_FILTERS = [{ name: "Keys", extensions: ["pem", "ppk", "key", ""] }];

const KINDS: HostKind[] = [
  "ssh", "rdp", "vnc", "spice", "telnet", "mosh", "tcp", "ftp", "smb", "webdav", "s3",
  "postgres", "mysql", "mssql", "oracle", "rqlite", "sqlite", "kubernetes", "redis", "mongodb", "elasticsearch",
];
const KIND_LABEL = {
  ssh: t("hostForm.kind.ssh"),
  rdp: t("hostForm.kind.rdp"),
  vnc: t("hostForm.kind.vnc"),
  spice: t("hostForm.kind.spice"),
  telnet: t("hostForm.kind.telnet"),
  mosh: t("hostForm.kind.mosh"),
  tcp: t("hostForm.kind.tcp"),
  ftp: t("hostForm.kind.ftp"),
  smb: t("hostForm.kind.smb"),
  webdav: t("hostForm.kind.webdav"),
  s3: t("hostForm.kind.s3"),
  postgres: t("hostForm.kind.postgres"),
  mysql: t("hostForm.kind.mysql"),
  mssql: t("hostForm.kind.mssql"),
  oracle: t("hostForm.kind.oracle"),
  rqlite: t("hostForm.kind.rqlite"),
  sqlite: t("hostForm.kind.sqlite"),
  kubernetes: t("hostForm.kind.kubernetes"),
  redis: t("hostForm.kind.redis"),
  mongodb: t("hostForm.kind.mongodb"),
  elasticsearch: t("hostForm.kind.elasticsearch"),
};

/** The port a host gets when its type changes to `k`. FTP, SMB, VNC,
 *  SPICE and the database servers take 0, their protocol's default (SQLite and rqlite have none); WebDAV and S3 keep theirs untouched, because
 *  the saved password is sealed to it (store.Host.SecretAAD). */
function portFor(k: HostKind, h: Host): number {
  if (k === h.kind) return h.port;
  if (k === "telnet") return 23;
  // A raw TCP service has no usual port: a new host asks for one.
  if (k === "tcp") return h.id ? h.port : Number.NaN;
  if (k === "ftp" || k === "smb" || k === "vnc" || k === "spice" || isDatabaseHost({ kind: k })) return 0;
  if (k === "webdav" || k === "s3") return h.port;
  return h.port === 23 || !h.port || !h.id ? 22 : h.port;
}

// What Test connection says for each step it can stop at (ConnTest.step).
const TEST_TEXT: Record<api.ConnTest["step"], MessageKey> = {
  ok: "hostForm.test.ok",
  proxy: "hostForm.test.proxy",
  vpn: "hostForm.test.vpn",
  tcp: "hostForm.test.tcp",
  ssh: "hostForm.test.ssh",
  hostKey: "hostForm.test.hostKey",
  auth: "hostForm.test.auth",
  database: "hostForm.test.database",
};
const ALGO_LABEL: Record<keyof SshAlgorithms, MessageKey> = {
  kex: "hostForm.algo.kex",
  ciphers: "hostForm.algo.ciphers",
  macs: "hostForm.algo.macs",
  hostKeys: "hostForm.algo.hostKeys",
};
// The port a database server's empty port field stands for.
const DB_PORT: Partial<Record<HostKind, string>> = { postgres: "5432", mysql: "3306", mssql: "1433", oracle: "1521", redis: "6379", mongodb: "27017" };
// The kinds TestConnection (or, for a database server, databaseTest) can check.
const TESTABLE: HostKind[] = ["ssh", "mosh", "telnet", "tcp", "postgres", "mysql", "mssql", "oracle", "rqlite"];

/** One titled group of the editor's fields. */
function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <h3 className="text-sm font-semibold">{title}</h3>
      {children}
    </section>
  );
}

/**
 * The host editor, a wide drawer with sections: Connection, Authentication,
 * Jump host, Proxy, VPN, Advanced, Wake-on-LAN and Test connection. Errors show
 * inline once Save is tried, and closing with unsaved changes asks first
 * (R74). File hosts (FTP, SMB, WebDAV, S3) show their own connection fields
 * and an optional password (S3: the secret key), and no jump host. Options
 * that weaken a connection (agent forwarding, legacy algorithms, raw TCP)
 * always show their warning.
 */
export function HostEditor({
  initial,
  onClose,
  onSaved,
  onImportSshConfig,
}: {
  initial: Host;
  onClose: () => void;
  onSaved: () => void;
  onImportSshConfig: () => void;
}) {
  const { hosts, folders } = useHosts();
  const { push } = useToast();
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [h, setH] = useState(initial);
  const set = (patch: Partial<Host>) => setH((x) => ({ ...x, ...patch }));
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [savedKeys, setSavedKeys] = useState<string[]>([]);
  const [sshKeys, setSshKeys] = useState<string[]>([]);
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const reloadSavedKeys = () => api.listIdentityKeys().then(setSavedKeys).catch(() => {});
  useEffect(() => {
    reloadSavedKeys();
    api.listSshKeys().then(setSshKeys).catch(() => {});
    api.listProxies().then(setProxies).catch(() => {});
    // Sync may have brought a profile since the list was read.
    reloadVpns();
  }, []);
  // Test connection's outcome, and an unknown host key it stopped at.
  const [test, setTest] = useState<{ busy?: boolean; ok?: boolean; text?: string; detail?: string } | null>(null);
  const [trust, setTrust] = useState<HostKeyPrompt | null>(null);
  // A result describes the form it tested; an edit makes it stale, and so
  // does a newer test. Each test and each edit takes the next number.
  const testSeq = useRef(0);
  useEffect(() => {
    testSeq.current++;
    setTest(null);
  }, [h]);

  const kind = hostKind(h);
  const file = isFileHost(h);
  // A VNC host takes a password and an optional user, like a file host,
  // and its port may be left empty (5900).
  const vnc = kind === "vnc";
  // A SPICE host takes an optional password and no user; its port may be
  // left empty (5900).
  const spice = kind === "spice";
  // A PostgreSQL, MySQL, SQL Server or Oracle host signs in with a user and
  // password, and its port may be left empty (the engine's default). An
  // rqlite node is a URL, signed in to only with a user. A SQLite host is a
  // file here, chosen in Go's dialog: no address, sign-in, jump host or proxy.
  const db = isDatabaseHost(h);
  const pgLike = SQL_SERVERS.includes(kind);
  const rq = kind === "rqlite";
  const sqlite = kind === "sqlite";
  // A Kubernetes cluster signs in with a token, or the client certificate
  // its kubeconfig import stored; its CA stays in Go.
  const k8s = kind === "kubernetes";
  // Redis and MongoDB sign in optionally (Redis with a password alone),
  // and choose TLS as PostgreSQL does; an Elasticsearch node is its URL.
  const netDb = db && !sqlite;
  const tlsDb = pgLike || kind === "redis" || kind === "mongodb";
  // WebDAV, S3, Kubernetes, Elasticsearch and rqlite connect to a URL; FTP
  // and SMB to a host name and port.
  const byUrl = kind === "webdav" || kind === "s3" || k8s || kind === "elasticsearch" || rq;
  const authSection = kind !== "telnet" && kind !== "tcp" && !vnc && !spice && !file && !db && !k8s;
  const dirty = JSON.stringify(h) !== JSON.stringify(initial);
  const errors = validateHost(h, hosts);
  const err = (f: HostField) => (tried && errors[f] ? t(errors[f]!) : undefined);
  const close = async () => {
    if (!dirty || (await confirm(t("hostForm.discard")))) onClose();
  };


  const runTest = async () => {
    const seq = ++testSeq.current;
    setTest({ busy: true });
    try {
      const r = db ? await api.databaseTest(formHost(h)) : await api.testConnection(formHost(h));
      if (seq !== testSeq.current) return;
      const e = r.error;
      // An unknown key asks to trust it, then tests again.
      if (e?.code === "HostKeyUnknown" && e.details?.requestId) {
        setTest(null);
        setTrust(e.details as unknown as HostKeyPrompt);
        return;
      }
      let text = t(TEST_TEXT[r.step] ?? "hostForm.test.ssh");
      if (e?.code === "SecretNeeded" && e.details?.kind === "kerberos") text = t("hostForm.test.kerberos");
      else if (e?.code === "SecretNeeded" && !e.details?.reason && !e.details?.proxyId) text = t("hostForm.test.password");
      setTest({ ok: r.step === "ok", text, detail: r.step === "ok" ? undefined : r.message });
    } catch (err) {
      if (seq !== testSeq.current) return;
      setTest({ ok: false, text: api.errorInfo(err).message });
    }
  };

  const save = async () => {
    setTried(true);
    if (Object.keys(errors).length) return;
    setBusy(true);
    const ok = await run(() => api.saveHost(formHost(h)), t("hostForm.saved"));
    setBusy(false);
    if (ok === undefined) return;
    onSaved();
    onClose();
  };

  // Import a Windows .rdp file into the form. .rdp is plain `key:type:value`
  // text; the password is not stored in the file.
  const importRdp = async () => {
    const path = await open({ filters: [{ name: "RDP", extensions: ["rdp"] }] });
    if (typeof path !== "string") return;
    const text = await run(() => api.readTextFile(path));
    if (text === undefined) return;
    const get = (key: string) => text.match(new RegExp(`^${key}:[si]:(.*)$`, "im"))?.[1].trim() ?? "";
    const [hostname, portStr] = get("full address").split(":"); // host or host:port
    // username may be "DOMAIN\user"; the domain is not a host field yet.
    const user = get("username").split("\\").pop() ?? "";
    const fileName = path.split(/[\\/]/).pop()?.replace(/\.rdp$/i, "") || "";
    set({
      kind: "rdp",
      authType: "password",
      alias: h.alias || fileName,
      hostname: hostname || h.hostname,
      port: 22, // SSH port for the workspace
      rdpPort: portStr ? Number(portStr) : 3389,
      user: user || h.user,
    });
    push("success", t("hostForm.rdpImported"));
  };

  // A saved (copied) key stores its basename; a ~/.ssh key its absolute
  // path. Match either back to the list so it stays selected on re-edit.
  const keyName = h.identityFile.split("/").pop() ?? "";
  const keyValue = savedKeys.includes(keyName) ? keyName : sshKeys.includes(h.identityFile) ? h.identityFile : "";
  const jumpHosts = hosts.filter((x) => x.id !== h.id && hostKind(x) === "ssh");
  const num = (v: string) => (v.trim() === "" ? Number.NaN : Number(v));
  const userLabel = {
    telnet: t("hostForm.userOptional"),
    ftp: t("hostForm.userAnonymous"),
    smb: t("hostForm.userGuest"),
    webdav: t("hostForm.userOptional"),
    vnc: t("hostForm.userOptional"),
    rqlite: t("hostForm.userOptional"),
    s3: t("hostForm.accessKey"),
    redis: t("hostForm.userOptional"),
    mongodb: t("hostForm.userOptional"),
    elasticsearch: t("hostForm.userOptional"),
  }[kind as string] ?? t("hostForm.user");

  const passwordField = (label: string) => (
    <Field label={label} error={err("password")}>
      <div className="flex gap-2">
        <Input
          type="password"
          className="flex-1 min-w-0"
          autoComplete="new-password"
          placeholder={h.hasPassword ? t("hostForm.passwordKeep") : ""}
          value={h.password ?? ""}
          aria-invalid={!!err("password")}
          onChange={(e) => set({ password: e.target.value })}
        />
        {h.hasPassword && h.id && (
          <Button
            variant="ghost"
            type="button"
            className="shrink-0"
            title={t("hostForm.revealHint")}
            onClick={async () => {
              // OS verification first (R15); nothing is revealed if it fails.
              const pw = await run(() => api.revealPassword(h.id));
              if (pw !== undefined) set({ password: pw });
            }}
          >
            {t("hostForm.reveal")}
          </Button>
        )}
      </div>
    </Field>
  );

  return (
    <Dialog
      title={h.id ? t("hostForm.editTitle", { name: initial.alias }) : t("hostForm.newTitle")}
      placement="drawer"
      size="xl"
      onClose={close}
      bodyClassName="px-5 py-4 overflow-auto flex flex-col gap-7"
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" form="host-editor" disabled={busy}>
            {t("hostForm.save")}
          </Button>
        </>
      }
    >
      <form
        id="host-editor"
        noValidate
        className="contents"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <Section title={t("hostForm.connection")}>
          <div className="flex flex-wrap items-end justify-between gap-3">
            {/* The radio group names itself; this caption only shows the name. */}
            <div>
              <p className="text-xs text-muted mb-1" aria-hidden>
                {t("hostForm.type")}
              </p>
              <SegmentedControl
                label={t("hostForm.type")}
                value={kind}
                className="flex-wrap"
                options={KINDS.map((k) => ({
                  value: k,
                  label: KIND_LABEL[k],
                  // Mosh's UDP cannot pass through SSH jump hosts (R19) or a proxy.
                  disabled: k === "mosh" && (!!h.jumpHostId || !!h.proxyId),
                  title:
                    k === "mosh" && h.jumpHostId
                      ? t("hostForm.moshJump")
                      : k === "mosh" && h.proxyId
                        ? t("hostForm.error.proxyMosh")
                        : undefined,
                }))}
                onChange={(k) =>
                  set({
                    kind: k,
                    // A sensible default port and auth per type.
                    port: portFor(k, h),
                    rdpPort: k === "rdp" ? h.rdpPort ?? 3389 : h.rdpPort,
                    authType: k === "rdp" ? "password" : h.authType ?? "key",
                    // Each kind has its own security modes; start at its default.
                    tls: k === kind ? h.tls : null,
                    password: passwordForKind(h, k),
                  })
                }
              />
            </div>
            {kind === "rdp" ? (
              <Button variant="ghost" type="button" onClick={importRdp}>
                {t("hostForm.importRdp")}
              </Button>
            ) : authSection ? (
              <Button variant="ghost" type="button" onClick={onImportSshConfig}>
                {t("action.importSshConfig")}
              </Button>
            ) : null}
          </div>
          {kind === "telnet" && <p className="text-xs text-warning">{t("hostForm.telnetWarning")}</p>}
          {kind === "tcp" && <p className="text-xs text-warning">{t("hostForm.tcpWarning")}</p>}
          {file && unencrypted(h) && <p className="text-xs text-warning">{t("hostForm.unencryptedWarning")}</p>}
          {vnc && unencrypted(h) && <p className="text-xs text-warning">{t("hostForm.vncUnencrypted")}</p>}
          {spice && unencrypted(h) && <p className="text-xs text-warning">{t("hostForm.spiceUnencrypted")}</p>}
          {netDb && unencrypted(h) && <p className="text-xs text-warning">{t("hostForm.dbUnencrypted")}</p>}
          {sqlite ? (
            <Field label={t("hostForm.dbFile")} hint={t("hostForm.dbFileHint")} error={err("file")}>
              <div className="flex gap-2">
                <Input
                  className="flex-1 min-w-0"
                  readOnly
                  spellCheck={false}
                  placeholder={t("hostForm.dbFileNone")}
                  value={h.file ?? ""}
                  aria-invalid={!!err("file")}
                />
                <Button
                  variant="ghost"
                  type="button"
                  onClick={async () => {
                    // Go opens the dialog and keeps the path; the token names it.
                    const p = await run(() => api.databasePickFile());
                    if (p?.token) set({ file: p.path, filePick: p.token });
                  }}
                >
                  {t("hostForm.browse")}
                </Button>
              </div>
            </Field>
          ) : byUrl ? (
            <Field
              label={kind === "s3" ? t("hostForm.endpoint") : k8s ? t("hostForm.clusterUrl") : kind === "elasticsearch" || rq ? t("hostForm.nodeUrl") : t("hostForm.url")}
              hint={k8s ? t("hostForm.clusterUrlHint") : t("hostForm.urlHint")}
              error={err("url")}
            >
              <Input
                className="w-full"
                autoFocus
                spellCheck={false}
                placeholder={
                  kind === "s3"
                    ? "https://s3.eu-west-1.amazonaws.com"
                    : k8s
                      ? "https://k8s.example.com:6443"
                      : kind === "elasticsearch"
                        ? "https://search.example.com:9200"
                        : rq
                          ? "https://rqlite.example.com:4001"
                          : "https://files.example.com/remote.php/dav/"
                }
                value={h.url ?? ""}
                aria-invalid={!!err("url")}
                onChange={(e) => set({ url: e.target.value })}
              />
            </Field>
          ) : (
            <div className="grid grid-cols-1 sm:grid-cols-[1fr_7rem] gap-3">
              <Field label={t("hostForm.hostname")} error={err("hostname")}>
                <Input
                  className="w-full"
                  autoFocus
                  spellCheck={false}
                  value={h.hostname}
                  aria-invalid={!!err("hostname")}
                  onChange={(e) => set({ hostname: e.target.value })}
                />
              </Field>
              <Field label={kind === "rdp" ? t("hostForm.sshPort") : t("hostForm.port")} error={err("port")}>
                <Input
                  className="w-full"
                  type="number"
                  min={1}
                  max={65535}
                  // 0 is the protocol's default, shown as an empty field.
                  placeholder={vnc || spice ? "5900" : tlsDb ? DB_PORT[kind] : file ? String(kind === "smb" ? 445 : h.tls === "implicit" ? 990 : 21) : undefined}
                  value={Number.isNaN(h.port) || ((file || vnc || spice || tlsDb) && !h.port) ? "" : h.port}
                  aria-invalid={!!err("port")}
                  onChange={(e) => set({ port: num(e.target.value) })}
                />
              </Field>
            </div>
          )}
          {kind === "ftp" && (
            <Field label={t("hostForm.ftpTls")} error={err("tls")}>
              <Select
                className="w-full"
                value={h.tls ?? "explicit"}
                aria-invalid={!!err("tls")}
                onChange={(e) => set({ tls: e.target.value as Host["tls"] })}
              >
                <option value="explicit">{t("hostForm.ftpTls.explicit")}</option>
                <option value="implicit">{t("hostForm.ftpTls.implicit")}</option>
                <option value="none">{t("hostForm.ftpTls.none")}</option>
              </Select>
            </Field>
          )}
          {vnc && (
            <Field label={t("hostForm.vncSecurity")} hint={err("tls") ? undefined : t("hostForm.vncSecurityHint")} error={err("tls")}>
              <Select
                className="w-full"
                value={h.tls ?? "vencrypt"}
                aria-invalid={!!err("tls")}
                onChange={(e) => set({ tls: e.target.value as Host["tls"] })}
              >
                <option value="vencrypt">{t("hostForm.vncSecurity.vencrypt")}</option>
                <option value="none">{t("hostForm.vncSecurity.none")}</option>
              </Select>
            </Field>
          )}
          {spice && (
            <Field label={t("hostForm.vncSecurity")} hint={err("tls") ? undefined : t("hostForm.vncSecurityHint")} error={err("tls")}>
              <Select
                className="w-full"
                value={h.tls ?? "tls"}
                aria-invalid={!!err("tls")}
                onChange={(e) => set({ tls: e.target.value as Host["tls"] })}
              >
                <option value="tls">{t("hostForm.spiceSecurity.tls")}</option>
                <option value="none">{t("hostForm.spiceSecurity.none")}</option>
              </Select>
            </Field>
          )}
          {tlsDb && (
            <Field label={t("hostForm.dbTls")} error={err("tls")}>
              <Select
                className="w-full"
                value={h.tls ?? "verify"}
                aria-invalid={!!err("tls")}
                onChange={(e) => set({ tls: e.target.value as Host["tls"] })}
              >
                <option value="verify">{t("hostForm.dbTls.verify")}</option>
                <option value="none">{t("hostForm.dbTls.none")}</option>
              </Select>
            </Field>
          )}
          {asksPasswordAgain(initial, h) && <p className="text-xs text-warning">{t("hostForm.passwordAgain")}</p>}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <Field label={t("hostForm.alias")} error={err("alias")}>
              <Input className="w-full" value={h.alias} aria-invalid={!!err("alias")} onChange={(e) => set({ alias: e.target.value })} />
            </Field>
            {/* Raw TCP signs in to nothing, SPICE with no user, and SQLite at all. */}
            {kind !== "tcp" && !spice && !sqlite && !k8s && (
              <Field label={userLabel} hint={vnc ? t("hostForm.vncUserHint") : undefined} error={err("user")}>
                <Input
                  className="w-full"
                  spellCheck={false}
                  value={h.user}
                  aria-invalid={!!err("user")}
                  onChange={(e) => set({ user: e.target.value })}
                />
              </Field>
            )}
            {tlsDb && (
              <Field
                label={
                  kind === "redis"
                    ? t("hostForm.redisDatabase")
                    : kind === "mongodb"
                      ? t("hostForm.mongoAuthDb")
                      : kind === "oracle"
                        ? t("hostForm.serviceName")
                        : t("hostForm.database")
                }
                error={err("database")}
              >
                <Input
                  className="w-full"
                  spellCheck={false}
                  inputMode={kind === "redis" ? "numeric" : undefined}
                  placeholder={kind === "redis" ? "0" : kind === "mongodb" ? "admin" : undefined}
                  value={h.database ?? ""}
                  aria-invalid={!!err("database")}
                  onChange={(e) => set({ database: e.target.value })}
                />
              </Field>
            )}
            {k8s && (
              <Field label={t("hostForm.namespace")} error={err("namespace")}>
                <Input
                  className="w-full"
                  spellCheck={false}
                  placeholder="default"
                  value={h.namespace ?? ""}
                  aria-invalid={!!err("namespace")}
                  onChange={(e) => set({ namespace: e.target.value || null })}
                />
              </Field>
            )}
            {kind === "smb" && (
              <Field label={t("hostForm.domain")}>
                <Input className="w-full" spellCheck={false} value={h.domain ?? ""} onChange={(e) => set({ domain: e.target.value })} />
              </Field>
            )}
            {kind === "s3" && (
              <>
                <Field label={t("hostForm.bucket")} error={err("bucket")}>
                  <Input
                    className="w-full"
                    spellCheck={false}
                    value={h.bucket ?? ""}
                    aria-invalid={!!err("bucket")}
                    onChange={(e) => set({ bucket: e.target.value })}
                  />
                </Field>
                <Field label={t("hostForm.region")}>
                  <Input
                    className="w-full"
                    spellCheck={false}
                    placeholder="us-east-1"
                    value={h.region ?? ""}
                    onChange={(e) => set({ region: e.target.value })}
                  />
                </Field>
              </>
            )}
            {kind === "rdp" && (
              <Field label={t("hostForm.rdpPort")} error={err("rdpPort")}>
                <Input
                  className="w-full"
                  type="number"
                  min={1}
                  max={65535}
                  value={h.rdpPort == null || Number.isNaN(h.rdpPort) ? "" : h.rdpPort}
                  aria-invalid={!!err("rdpPort")}
                  onChange={(e) => set({ rdpPort: num(e.target.value) })}
                />
              </Field>
            )}
            <Field label={t("hostForm.folder")}>
              <Select className="w-full" value={h.folderId ?? ""} onChange={(e) => set({ folderId: e.target.value || null })}>
                <option value="">{t("hostForm.folderRoot")}</option>
                {folders.map((f) => (
                  <option key={f.id} value={f.id}>
                    {folderPath(f, folders)}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          <Field label={t("hostForm.tags")} hint={t("hostForm.tagsHint")}>
            <TagEditor tags={h.tags ?? []} onChange={(tags) => set({ tags })} />
          </Field>
          {kind === "tcp" && (
            <label className="flex items-center gap-2 text-sm w-fit">
              <input
                type="checkbox"
                className="accent-accent size-4"
                checked={!!h.localEcho}
                onChange={(e) => set({ localEcho: e.target.checked })}
              />
              {t("hostForm.localEcho")}
            </label>
          )}
          {kind === "s3" && (
            <label className="flex items-center gap-2 text-sm w-fit">
              <input
                type="checkbox"
                className="accent-accent size-4"
                checked={!!h.pathStyle}
                onChange={(e) => set({ pathStyle: e.target.checked })}
              />
              {t("hostForm.pathStyle")}
            </label>
          )}
        </Section>

        {(file || vnc || spice || netDb || k8s) && (
          <Section title={t("hostForm.authentication")}>
            {passwordField(
              vnc || pgLike ? t("hostForm.password") : kind === "s3" ? t("hostForm.secretKey") : k8s ? t("hostForm.token") : t("hostForm.passwordOptional")
            )}
            {k8s && <p className="text-xs text-muted">{t("hostForm.tokenHint")}</p>}
          </Section>
        )}

        {authSection && (
          <Section title={t("hostForm.authentication")}>
            {kind !== "rdp" && (
              <SegmentedControl
                label={t("hostForm.authentication")}
                value={h.authType ?? "key"}
                options={[
                  { value: "key", label: t("hostForm.auth.key") },
                  { value: "password", label: t("hostForm.auth.password") },
                  { value: "agent", label: t("hostForm.auth.agent") },
                  { value: "kerberos", label: t("hostForm.auth.kerberos") },
                ]}
                onChange={(a) => set({ authType: a })}
              />
            )}
            {kind !== "rdp" && h.authType === "agent" ? (
              <p className="text-xs text-muted">{t("hostForm.agentHint")}</p>
            ) : kind !== "rdp" && h.authType === "kerberos" ? (
              <p className="text-xs text-muted">{t("hostForm.kerberosHint")}</p>
            ) : kind !== "rdp" && (h.authType ?? "key") === "key" ? (
              <Field label={t("hostForm.identityKey")} error={err("identityFile")}>
                <Select
                  className="w-full"
                  value={keyValue}
                  aria-invalid={!!err("identityFile")}
                  onChange={async (e) => {
                    if (e.target.value !== "__import__") return set({ identityFile: e.target.value });
                    const p = await open({ filters: KEY_FILTERS });
                    if (typeof p !== "string") return;
                    const name = await run(() => api.saveIdentityKey(p));
                    if (name) {
                      await reloadSavedKeys();
                      set({ identityFile: name });
                    }
                  }}
                >
                  <option value="">{t("hostForm.selectKey")}</option>
                  {sshKeys.length > 0 && (
                    <optgroup label="~/.ssh">
                      {sshKeys.map((k) => (
                        <option key={k} value={k}>
                          {k.split("/").pop()}
                        </option>
                      ))}
                    </optgroup>
                  )}
                  {savedKeys.length > 0 && (
                    <optgroup label={t("hostForm.importedKeys")}>
                      {savedKeys.map((k) => (
                        <option key={k} value={k}>
                          {k}
                        </option>
                      ))}
                    </optgroup>
                  )}
                  <option value="__import__">{t("hostForm.importKey")}</option>
                </Select>
                <div className="flex gap-2 mt-2">
                  <Input
                    className="flex-1 min-w-0"
                    spellCheck={false}
                    placeholder={t("hostForm.keyPath")}
                    aria-label={t("hostForm.keyPath")}
                    value={h.identityFile}
                    onChange={(e) => set({ identityFile: e.target.value })}
                  />
                  <Button
                    variant="ghost"
                    type="button"
                    onClick={async () => {
                      const p = await open({ filters: KEY_FILTERS });
                      if (typeof p === "string") set({ identityFile: p });
                    }}
                  >
                    {t("hostForm.browse")}
                  </Button>
                </div>
              </Field>
            ) : (
              passwordField(t("hostForm.password"))
            )}
          </Section>
        )}

        {kind !== "telnet" && !file && !sqlite && (
          <Section title={t("hostForm.jumpHost")}>
            <Field label={t("hostForm.jumpVia")} hint={t("hostForm.jumpHint")} error={err("jumpHostId")}>
              <Select
                className="w-full"
                value={h.jumpHostId ?? ""}
                aria-invalid={!!err("jumpHostId")}
                onChange={(e) => set({ jumpHostId: e.target.value || null })}
              >
                <option value="">{t("hostForm.jumpNone")}</option>
                {jumpHosts.map((x) => (
                  <option key={x.id} value={x.id}>
                    {x.alias} ({x.user}@{x.hostname})
                  </option>
                ))}
              </Select>
            </Field>
            {h.jumpHostId && kind !== "rdp" && kind !== "tcp" && !vnc && !spice && !db && <p className="text-xs text-muted">{t("hostForm.moshJump")}</p>}
          </Section>
        )}

        {!sqlite && (
        <Section title={t("hostForm.proxy")}>
          {/* The conflict shows at once: it names what to do instead. */}
          <Field label={t("hostForm.proxyVia")} hint={t("hostForm.proxyHint")} error={errors.proxyId && t(errors.proxyId)}>
            <Select
              className="w-full"
              value={h.proxyId ?? ""}
              aria-invalid={!!errors.proxyId}
              onChange={(e) => set({ proxyId: e.target.value || null })}
            >
              <option value="">{t("hostForm.proxyNone")}</option>
              {proxies.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name} ({p.type} {p.host}:{p.port})
                </option>
              ))}
              {h.proxyId && !proxies.some((p) => p.id === h.proxyId) && <option value={h.proxyId}>{t("hostForm.proxyMissing")}</option>}
            </Select>
          </Field>
        </Section>
        )}

        {!sqlite && (
          <Section title={t("hostForm.vpn")}>
            {/* Like the proxy, the conflict shows at once (R15, KTD5). */}
            <VpnField h={h} set={set} error={errors.vpnId} />
          </Section>
        )}

        {(kind === "rdp" || kind === "ssh") && (
          <Section title={t("hostForm.advanced")}>
            {kind === "ssh" && <SshAdvanced h={h} set={set} keepaliveError={err("keepalive")} />}
            {kind === "rdp" && (
              <Field label={t("hostForm.rdpSecurity")}>
                <Select
                  className="w-full"
                  value={h.rdpSecurity ?? "auto"}
                  onChange={(e) => set({ rdpSecurity: e.target.value as "auto" | "tls" | "nla" })}
                >
                  <option value="auto">{t("hostForm.rdpSecurity.auto")}</option>
                  <option value="tls">{t("hostForm.rdpSecurity.tls")}</option>
                  <option value="nla">{t("hostForm.rdpSecurity.nla")}</option>
                </Select>
              </Field>
            )}
          </Section>
        )}

        {initial.id && TERMINAL_KINDS.includes(kind) && (
          <Section title={t("postLogin.title")}>
            <PostLoginEditor hostId={initial.id} />
          </Section>
        )}

        {!byUrl && !sqlite && (
          <Section title={t("hostForm.wol")}>
            <Field label={t("hostForm.mac")} hint={t("hostForm.macHint")}>
              <Input
                className="w-full font-mono"
                spellCheck={false}
                placeholder="aa:bb:cc:dd:ee:ff"
                value={h.mac ?? ""}
                onChange={(e) => set({ mac: e.target.value })}
              />
            </Field>
          </Section>
        )}

        {TESTABLE.includes(kind) && (
          <Section title={t("hostForm.test")}>
            <div className="flex flex-wrap items-center gap-3">
              <Button type="button" className="w-fit" disabled={test?.busy} onClick={runTest}>
                {test?.busy ? t("hostForm.testing") : t("hostForm.test")}
              </Button>
              <div role="status" aria-live="polite" className="text-sm min-w-0 flex-1">
                {test && !test.busy && (
                  <>
                    <p className={test.ok ? "text-success" : "text-danger"}>{test.text}</p>
                    {test.detail && <p className="text-xs text-muted allow-select break-words">{test.detail}</p>}
                  </>
                )}
              </div>
            </div>
          </Section>
        )}
      </form>
      {trust && (
        <TrustDialog
          prompt={trust}
          onConfirm={() => {
            const prompt = trust;
            const seq = testSeq.current;
            setTrust(null);
            // An edit while the answer is on its way makes the test again stale.
            answerTrust(prompt, true).then(
              () => {
                if (seq === testSeq.current) runTest();
              },
              (e) => {
                if (seq === testSeq.current) setTest({ ok: false, text: api.errorInfo(e).message });
              }
            );
          }}
          onCancel={() => {
            answerTrust(trust, false).catch(() => {});
            setTrust(null);
            setTest({ ok: false, text: t("hostForm.test.trustDeclined") });
          }}
        />
      )}
    </Dialog>
  );
}

/** SSH's advanced options: agent forwarding, X11 forwarding and its
 *  desktop command, keepalive and legacy algorithms, each weakening option
 *  with its warning always shown. */
function SshAdvanced({ h, set, keepaliveError }: { h: Host; set: (patch: Partial<Host>) => void; keepaliveError?: string }) {
  // With X11 on, say which X server to install when this computer has none.
  const [xProblem, setXProblem] = useState("");
  useEffect(() => {
    if (!h.x11) return setXProblem("");
    let gone = false;
    api.localXServer().then(
      (s) => !gone && setXProblem(s.problem ?? ""),
      (e) => !gone && setXProblem(api.errorInfo(e).message)
    );
    return () => {
      gone = true;
    };
  }, [h.x11]);
  const algos = h.algorithms ?? {};
  const chosen = Object.values(algos).reduce((n, l) => n + (l?.length ?? 0), 0);
  const toggle = (list: keyof SshAlgorithms, name: string, on: boolean) => {
    const next = { ...algos, [list]: on ? [...(algos[list] ?? []), name] : (algos[list] ?? []).filter((x) => x !== name) };
    // No names left is no opt-in at all: the library's defaults.
    const any = Object.values(next).some((l) => l?.length);
    set({ algorithms: any ? next : null });
  };
  return (
    <>
      <div className="flex flex-col gap-1">
        <label className="flex items-center gap-2 text-sm w-fit">
          <input
            type="checkbox"
            className="accent-accent size-4"
            checked={!!h.forwardAgent}
            aria-describedby="forward-agent-warning"
            onChange={(e) => set({ forwardAgent: e.target.checked || null })}
          />
          {t("hostForm.forwardAgent")}
        </label>
        <p id="forward-agent-warning" className={`text-xs ${h.forwardAgent ? "text-warning" : "text-muted"}`}>
          {t("hostForm.forwardAgentWarning")}
        </p>
      </div>
      <div className="flex flex-col gap-1">
        <label className="flex items-center gap-2 text-sm w-fit">
          <input
            type="checkbox"
            className="accent-accent size-4"
            checked={!!h.x11}
            aria-describedby="x11-warning"
            onChange={(e) => set({ x11: e.target.checked || null })}
          />
          {t("hostForm.x11")}
        </label>
        <p id="x11-warning" className={`text-xs ${h.x11 ? "text-warning" : "text-muted"}`}>
          {t("hostForm.x11Warning")}
        </p>
        <p role="status" aria-live="polite" className="text-xs text-warning allow-select break-words empty:hidden">
          {xProblem}
        </p>
      </div>
      <Field label={t("hostForm.x11Desktop")} hint={t("hostForm.x11DesktopHint")}>
        <Input
          className="w-full font-mono"
          spellCheck={false}
          placeholder={t("hostForm.x11DesktopPlaceholder")}
          value={h.x11Desktop ?? ""}
          onChange={(e) => set({ x11Desktop: e.target.value })}
        />
      </Field>
      <Field label={t("hostForm.keepalive")} hint={t("hostForm.keepaliveHint")} error={keepaliveError}>
        <Input
          className="w-32"
          type="number"
          min={0}
          max={86400}
          value={h.keepalive == null || Number.isNaN(h.keepalive) ? "" : h.keepalive}
          aria-invalid={!!keepaliveError}
          onChange={(e) => set({ keepalive: e.target.value.trim() === "" ? null : Number(e.target.value) })}
        />
      </Field>
      <details className="group" open={chosen > 0}>
        <summary className="text-sm cursor-pointer w-fit">
          {t("hostForm.algorithms")}
          {chosen > 0 && <span className="text-muted"> · {t("hostForm.algorithmsCount", { n: chosen })}</span>}
        </summary>
        <div className="mt-2 flex flex-col gap-3">
          <p className={`text-xs ${chosen ? "text-warning" : "text-muted"}`}>{t("hostForm.algorithmsWarning")}</p>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            {(Object.keys(SSH_ALGORITHMS) as (keyof SshAlgorithms)[]).map((list) => {
              const [supported, insecure] = SSH_ALGORITHMS[list];
              return (
                <fieldset key={list} className="flex flex-col gap-1 min-w-0">
                  <legend className="text-xs text-muted mb-1">{t(ALGO_LABEL[list])}</legend>
                  {[...insecure, ...supported].map((name) => (
                    <label key={name} className="flex items-center gap-2 text-xs min-w-0">
                      <input
                        type="checkbox"
                        className="accent-accent size-3.5 shrink-0"
                        checked={!!algos[list]?.includes(name)}
                        onChange={(e) => toggle(list, name, e.target.checked)}
                      />
                      <span className={`truncate ${insecure.includes(name) ? "text-warning" : ""}`} title={name}>
                        {insecure.includes(name) ? t("hostForm.algorithmsWeak", { name }) : name}
                      </span>
                    </label>
                  ))}
                </fieldset>
              );
            })}
          </div>
        </div>
      </details>
    </>
  );
}

/** "Parent / Child" breadcrumb label for the folder dropdown. */
function folderPath(f: Folder, all: Folder[]): string {
  const parts: string[] = [f.name];
  let cur = f.parentId;
  const guard = new Set<string>([f.id]);
  while (cur && !guard.has(cur)) {
    guard.add(cur);
    const parent = all.find((x) => x.id === cur);
    if (!parent) break;
    parts.unshift(parent.name);
    cur = parent.parentId;
  }
  return parts.join(" / ");
}
