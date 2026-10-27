import { useCallback, useEffect, useMemo, useState, type TextareaHTMLAttributes } from "react";
import * as api from "../api";
import { useSessions, type Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { controlCls, Field, Input, useFieldControl } from "../ui/Field";
import { Dialog } from "../ui/Dialog";
import { IconButton } from "../ui/IconButton";
import { AddIcon, DeleteIcon, DockerIcon, FolderIcon, KeyIcon, LogsIcon, RefreshIcon, StartIcon, StopIcon, TerminalIcon } from "../ui/icons";
import { Panel, Spinner, Badge } from "../ui/kit";
import { SegmentedControl } from "../ui/SegmentedControl";
import { Select } from "../ui/Select";
import { Tabs } from "../ui/Tabs";
import { useToast, useAsyncToast } from "../ui/toast";
import { usePrompt } from "../ui/prompt";
import { ContainerFiles } from "./ContainerFiles";
import { open, save } from "../dialogs";
import { listenAddress, loopback, noEngine, toCreateOptions, type CreateForm } from "./containerForm";
import { t, type MessageKey } from "../i18n";

type Sub = "containers" | "images" | "compose" | "stats" | "settings";

function isPermissionError(e: unknown): boolean {
  const m = (e instanceof Error ? e.message : String(e)).toLowerCase();
  return m.includes("permission") || m.includes("denied") || m.includes("docker.sock");
}

function Output({ text }: { text: string }) {
  return (
    <pre className="text-xs bg-term-bg text-term-fg rounded-control p-3 overflow-auto flex-1 min-h-0 whitespace-pre-wrap break-words">
      {text || t("containers.noOutput")}
    </pre>
  );
}

const LOG_TAIL = 200; // default lines pulled per `docker logs` fetch
const LOG_TAIL_OPTIONS = ["200", "500", "2000", "10000", "all"];
const LOG_PAGE = 200; // filtered lines shown per page

/** Log viewer: timestamps (UTC↔local), text filter, pagination. */
function LogViewer({
  raw,
  name,
  onFetch,
  onClear,
  onFollow,
}: {
  raw: string;
  name: string;
  /** Re-pull logs with the given tail size ("200".."all"). */
  onFetch: (tail: string) => Promise<void>;
  onClear: () => void;
  /** Follows the logs as they come, in a terminal tab. */
  onFollow: () => void;
}) {
  const [query, setQuery] = useState("");
  const [tz, setTz] = useState<"local" | "utc">("local");
  const [page, setPage] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const [tail, setTail] = useState(String(LOG_TAIL));

  const { push } = useToast();
  const pull = async (tail: string) => {
    setRefreshing(true);
    try {
      await onFetch(tail);
    } finally {
      setRefreshing(false);
    }
  };

  const lines = useMemo(() => raw.split(/\r?\n/), [raw]);

  // Convert leading RFC3339 timestamp (from `docker logs -t`, always UTC) to
  // local time when requested. Lines without a timestamp pass through.
  const tsRe = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z)\s(.*)$/;
  const rendered = useMemo(() => {
    const out = lines.map((line) => {
      const m = line.match(tsRe);
      if (!m) return line;
      if (tz === "utc") return line;
      const d = new Date(m[1]);
      if (isNaN(d.getTime())) return line;
      return `${d.toLocaleString()} ${m[2]}`;
    });
    const q = query.trim().toLowerCase();
    return q ? out.filter((l) => l.toLowerCase().includes(q)) : out;
  }, [lines, tz, query]);

  const pages = Math.max(1, Math.ceil(rendered.length / LOG_PAGE));
  // Recent-first: jump to the last page whenever a fresh log blob arrives
  // (docker logs are oldest→newest, so newest lines live on the last page).
  useEffect(() => {
    setPage(Math.max(0, Math.ceil(lines.length / LOG_PAGE) - 1));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [raw]);
  const clamped = Math.min(page, pages - 1);
  const slice = rendered.slice(clamped * LOG_PAGE, clamped * LOG_PAGE + LOG_PAGE);

  return (
    <div className="flex-1 min-h-0 flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 shrink-0 text-sm">
        <Input
          placeholder={t("containers.logs.filter")}
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setPage(0);
          }}
          className="flex-1 min-w-48"
        />
        <SegmentedControl
          label={t("containers.logs.timeZone")}
          value={tz}
          onChange={setTz}
          options={[
            { value: "local", label: t("containers.logs.local") },
            { value: "utc", label: t("containers.logs.utc") },
          ]}
        />
        <span className="opacity-60 text-xs">{t(rendered.length === 1 ? "containers.logs.line" : "containers.logs.lines", { n: rendered.length })}</span>
        <Select
          compact
          value={tail}
          onChange={(e) => {
            setTail(e.target.value);
            pull(e.target.value);
          }}
          title={t("containers.logs.tail")}
        >
          {LOG_TAIL_OPTIONS.map((o) => (
            <option key={o} value={o}>
              {t("containers.logs.tailOption", { n: o })}
            </option>
          ))}
        </Select>
        <Button variant="ghost" onClick={() => pull(tail)} disabled={refreshing}>
          {refreshing ? <Spinner /> : t("containers.refresh")}
        </Button>
        <Button
          variant="ghost"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(rendered.join("\n"));
              push("success", t("containers.logs.copied"));
            } catch (e) {
              push("error", e instanceof Error ? e.message : t("containers.logs.copyFailed"));
            }
          }}
        >
          {t("containers.logs.copy")}
        </Button>
        <Button
          variant="ghost"
          onClick={async () => {
            const path = await save({ defaultPath: `${name}-logs.txt` });
            if (!path) return;
            try {
              await api.writeTextFile(path, rendered.join("\n"));
              push("success", t("containers.logs.downloaded"));
            } catch (e) {
              push("error", e instanceof Error ? e.message : t("containers.logs.downloadFailed"));
            }
          }}
        >
          {t("containers.logs.download")}
        </Button>
        <Button variant="ghost" onClick={onClear}>
          {t("containers.logs.clear")}
        </Button>
        <Button variant="ghost" icon={TerminalIcon} onClick={onFollow}>
          {t("containers.logs.follow")}
        </Button>
      </div>

      <pre className="text-xs bg-term-bg text-term-fg rounded-control p-3 overflow-auto flex-1 min-h-0 whitespace-pre-wrap wrap-break-word">
        {slice.join("\n") || t("containers.logs.noMatch")}
      </pre>

      {pages > 1 && (
        <div className="flex items-center justify-center gap-2 shrink-0 text-sm">
          <Button variant="ghost" onClick={() => setPage(Math.max(0, clamped - 1))} disabled={clamped === 0}>
            {t("containers.logs.prev")}
          </Button>
          <span className="opacity-60">{t("containers.logs.page", { page: clamped + 1, pages })}</span>
          <Button
            variant="ghost"
            onClick={() => setPage(Math.min(pages - 1, clamped + 1))}
            disabled={clamped >= pages - 1}
          >
            {t("containers.logs.next")}
          </Button>
        </div>
      )}
    </div>
  );
}


/** Docker daemon log-rotation settings (/etc/docker/daemon.json log-opts). */
function LogSettings({ sessionId }: { sessionId: string }) {
  const { push } = useToast();
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [maxSize, setMaxSize] = useState("50m");
  const [maxFile, setMaxFile] = useState("3");
  const [current, setCurrent] = useState<string>("");
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    const r = await run(() => api.exec(sessionId, "cat /etc/docker/daemon.json 2>/dev/null || echo '{}'"));
    setLoading(false);
    if (!r) return;
    setCurrent(r.stdout.trim() || "{}");
    try {
      const j = JSON.parse(r.stdout || "{}");
      const opts = j["log-opts"] || {};
      // Pre-fill with the requested defaults when not already set.
      setMaxSize(opts["max-size"] || "50m");
      setMaxFile(opts["max-file"] || "3");
    } catch {
      setMaxSize("50m");
      setMaxFile("3");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId]);

  useEffect(() => {
    load();
  }, [load]);

  const apply = async () => {
    if (!/^\d+[kmg]?$/i.test(maxSize.trim())) {
      push("error", t("containers.settings.badSize"));
      return;
    }
    if (!/^\d+$/.test(maxFile.trim())) {
      push("error", t("containers.settings.badFiles"));
      return;
    }
    if (
      !(await confirm(t("containers.settings.confirm")))
    )
      return;
    // Merge in JS against the daemon.json we already fetched into `current`,
    // then write the COMPLETE file. No remote python/jq — avoids the silent
    // pipe failures that left daemon.json as {}.
    let base: Record<string, unknown> = {};
    try {
      base = current.trim() ? JSON.parse(current) : {};
    } catch {
      base = {};
    }
    const merged = {
      ...base,
      "log-driver": "json-file",
      "log-opts": {
        ...(typeof base["log-opts"] === "object" && base["log-opts"] ? base["log-opts"] : {}),
        "max-size": maxSize.trim(),
        "max-file": maxFile.trim(),
      },
      "live-restore": true,
    };
    const b64 = btoa(JSON.stringify(merged, null, 2));

    // Write via base64->tee, then read back to CONFIRM it persisted, then
    // reload (SIGHUP keeps containers).
    const cmd =
      "sudo -n mkdir -p /etc/docker && " +
      `printf %s '${b64}' | base64 -d | sudo -n tee /etc/docker/daemon.json >/dev/null && ` +
      "echo '---WROTE---' && cat /etc/docker/daemon.json && echo '---RELOAD---' && " +
      "( sudo -n systemctl reload docker 2>&1 || sudo -n kill -s HUP $(cat /var/run/docker.pid 2>/dev/null) 2>&1 || echo 'reload-skipped' )";
    const r = await run(() => api.exec(sessionId, cmd));
    if (r) {
      const wrote = r.stdout.includes("---WROTE---") && r.stdout.includes('"max-size"');
      if (wrote && r.code === 0) {
        push("success", t("containers.settings.applied"));
      } else {
        push("error", (r.stderr || r.stdout || t("containers.settings.failed")).trim());
      }
      load();
    }
  };

  return (
    <Panel
      title={t("containers.settings.title")}
      className="flex-1 min-h-0"
      bodyClassName="flex-1 min-h-0 overflow-auto p-4"
      actions={
        <Button variant="ghost" onClick={load} disabled={loading}>
          {loading ? <Spinner /> : t("containers.settings.reload")}
        </Button>
      }
    >
      <p className="text-sm opacity-70 mb-4">{t("containers.settings.hint")}</p>
      <div className="flex flex-wrap items-end gap-4 mb-4">
        <label className="text-sm">
          <div className="opacity-60 mb-1">{t("containers.settings.maxSize")}</div>
          <Input value={maxSize} onChange={(e) => setMaxSize(e.target.value)} className="w-32" placeholder="50m" />
        </label>
        <label className="text-sm">
          <div className="opacity-60 mb-1">{t("containers.settings.maxFiles")}</div>
          <Input value={maxFile} onChange={(e) => setMaxFile(e.target.value)} className="w-24" placeholder="3" />
        </label>
        <Button variant="primary" onClick={apply}>
          {t("containers.settings.apply")}
        </Button>
      </div>
      <div className="text-xs opacity-50 mb-1">{t("containers.settings.current")}</div>
      <pre className="text-xs bg-term-bg text-term-fg rounded-control p-3 overflow-auto max-h-60 whitespace-pre-wrap">
        {current || t("containers.empty")}
      </pre>
    </Panel>
  );
}


const th = "text-left font-medium px-3 py-2 opacity-70";
const td = "px-3 py-2 border-t border-line align-top";

// ---- Containers ----

function Containers({ sessionId, info, local }: { sessionId: string; info: api.ContainerInfo | null; local: boolean }) {
  const { push } = useToast();
  const { openContainerTerm } = useSessions();
  const [creating, setCreating] = useState(false);
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [rows, setRows] = useState<api.DockerContainer[]>([]);
  const [loading, setLoading] = useState(false);
  const [permError, setPermError] = useState(false);
  const [logs, setLogs] = useState<{ id: string; name: string; text: string } | null>(null);
  const [files, setFiles] = useState<{ id: string; name: string } | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setPermError(false);
    try {
      setRows(await api.dockerPs(sessionId));
    } catch (e) {
      // The socket fix runs over SSH; this computer's engine is not Hostbay's to fix.
      if (isPermissionError(e) && !local) setPermError(true);
      else push("error", e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [sessionId, push, local]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const act = async (id: string, action: "start" | "stop" | "restart" | "rm") => {
    if (action === "rm" && !(await confirm(t("containers.confirmRemove")))) return;
    const r = await run(() => api.dockerAction(sessionId, id, action), t("containers.actionDone", { action }));
    if (r !== undefined) refresh();
  };

  const fetchLogs = async (id: string, tail: string) => {
    // Each line carries an RFC3339 UTC timestamp the viewer can convert to local time.
    const r = await run(() => api.dockerLogs(sessionId, id, tail));
    return r === undefined ? "" : r || t("containers.empty");
  };
  const showLogs = async (c: api.DockerContainer) => {
    const text = await fetchLogs(c.id, String(LOG_TAIL));
    setLogs({ id: c.id, name: c.name, text });
  };

  return (
    <Panel
      title={t("containers.title")}
      className="flex-1 min-h-0"
      bodyClassName="flex-1 min-h-0 overflow-auto p-4"
      actions={
        <>
          <Button variant="ghost" icon={AddIcon} onClick={() => setCreating(true)}>
            {t("containers.create")}
          </Button>
          <Button variant="ghost" onClick={refresh} disabled={loading}>
            {loading ? <Spinner /> : t("containers.refresh")}
          </Button>
        </>
      }
    >
      {permError && (
        <div className="mb-3 rounded-control border border-warning/40 bg-warning-soft p-3 text-sm">
          <p className="mb-2">{t("containers.permission.text")}</p>
          <Button
            variant="primary"
            onClick={() => run(() => api.dockerFixPermission(sessionId), t("containers.permission.fixed"))}
          >
            {t("containers.permission.fix")}
          </Button>
        </div>
      )}

      {!permError && rows.length === 0 && !loading && <EmptyState>{t("containers.noContainers")}</EmptyState>}

      {rows.length > 0 && (
        <div className="overflow-auto">
          <table className="w-full text-sm">
            <thead>
              <tr>
                <th className={th}>{t("containers.col.name")}</th>
                <th className={th}>{t("containers.col.image")}</th>
                <th className={th}>{t("containers.col.status")}</th>
                <th className={th}>{t("containers.col.ports")}</th>
                <th className={th}>{t("containers.col.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((c) => {
                const running = c.state === "running";
                return (
                  <tr key={c.id}>
                    <td className={td}>{c.name}</td>
                    <td className={td}>{c.image}</td>
                    <td className={`${td} whitespace-nowrap`}>
                      <Badge tone={running ? "ok" : "neutral"}>{c.status}</Badge>
                    </td>
                    <td className={`${td} font-mono text-xs`}>
                      {c.ports
                        ? c.ports.split(",").map((p, i) => <div key={i} className="whitespace-nowrap">{p.trim()}</div>)
                        : "—"}
                    </td>
                    <td className={td}>
                      <div className="flex items-center gap-1">
                        {!running && <IconButton label={t("containers.start")} icon={StartIcon} tone="accent" onClick={() => act(c.id, "start")} />}
                        {running && (
                          <>
                            <IconButton label={t("containers.stop")} icon={StopIcon} onClick={() => act(c.id, "stop")} />
                            {info?.caps.restart !== false && (
                              <IconButton label={t("containers.restart")} icon={RefreshIcon} onClick={() => act(c.id, "restart")} />
                            )}
                            <IconButton
                              label={t("containers.shell")}
                              icon={TerminalIcon}
                              onClick={() => openContainerTerm(sessionId, c.id, c.name, false)}
                            />
                          </>
                        )}
                        <IconButton label={t("containers.logs")} icon={LogsIcon} onClick={() => showLogs(c)} />
                        {running && (
                          <IconButton label={t("containers.browse")} icon={FolderIcon} onClick={() => setFiles({ id: c.id, name: c.name })} />
                        )}
                        <IconButton label={t("containers.remove")} icon={DeleteIcon} tone="danger" onClick={() => act(c.id, "rm")} />
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {files && (
        <ContainerFiles
          sessionId={sessionId}
          containerId={files.id}
          name={files.name}
          onClose={() => setFiles(null)}
        />
      )}

      {logs && (
        <Dialog
          title={t("containers.logs.title", { name: logs.name })}
          size="full"
          onClose={() => setLogs(null)}
          bodyClassName="p-4 overflow-auto flex flex-col"
        >
          <LogViewer
            raw={logs.text}
            name={logs.name}
            onFetch={async (tail) => {
              const text = await fetchLogs(logs.id, tail);
              setLogs((l) => (l ? { ...l, text } : l));
            }}
            onClear={() => setLogs((l) => (l ? { ...l, text: "" } : l))}
            onFollow={() => {
              // The followed logs open in a tab of their own, shown at once.
              setLogs(null);
              openContainerTerm(sessionId, logs.id, logs.name, true);
            }}
          />
        </Dialog>
      )}

      {creating && (
        <CreateDialog
          sessionId={sessionId}
          info={info}
          onClose={() => setCreating(false)}
          onCreated={() => {
            setCreating(false);
            refresh();
          }}
        />
      )}
    </Panel>
  );
}

// A multi-line field, named by its Field label like Input.
function TextArea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...useFieldControl(props)} {...props} className={`w-full px-3 py-2 font-mono ${controlCls}`} />;
}

/** The create form (KTD27): image, name, ports, environment, named
 *  volumes, bind mounts and restart policy, and nothing that widens what
 *  the container can reach. Go asks before a bind mount, in its own dialog. */
function CreateDialog({
  sessionId,
  info,
  onClose,
  onCreated,
}: {
  sessionId: string;
  info: api.ContainerInfo | null;
  onClose: () => void;
  onCreated: () => void;
}) {
  const run = useAsyncToast();
  const [f, setF] = useState<CreateForm>({ image: "", name: "", ports: "", env: "", volumes: "", binds: "", restart: "" });
  const [busy, setBusy] = useState(false);
  const set = (p: Partial<CreateForm>) => setF((v) => ({ ...v, ...p }));
  let parsed: api.CreateOptions | null = null;
  let error = "";
  try {
    parsed = toCreateOptions(f);
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }
  const wide = parsed?.ports.filter((p) => !loopback(p)) ?? [];

  const create = async () => {
    if (!parsed?.image) return;
    setBusy(true);
    const id = await run(() => api.dockerCreate(sessionId, parsed!), t("containers.created"));
    setBusy(false);
    if (id !== undefined) onCreated();
  };

  return (
    <Dialog
      title={t("containers.createTitle")}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" onClick={create} disabled={busy || !parsed?.image}>
            {busy ? <Spinner /> : t("containers.createStart")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-sm">
        <div className="grid grid-cols-2 gap-3">
          <Field label={t("containers.form.image")}>
            <Input value={f.image} onChange={(e) => set({ image: e.target.value })} placeholder="nginx:1.27" className="w-full" />
          </Field>
          <Field label={t("containers.form.name")}>
            <Input value={f.name} onChange={(e) => set({ name: e.target.value })} className="w-full" />
          </Field>
        </div>
        <Field
          label={t("containers.form.ports")}
          hint={t("containers.form.portsHint")}
          error={
            wide.length > 0
              ? t("containers.form.portsWide", { ports: wide.map(listenAddress).join(", ") })
              : undefined
          }
        >
          <TextArea rows={2} value={f.ports} onChange={(e) => set({ ports: e.target.value })} placeholder="8080:80" />
        </Field>
        <Field label={t("containers.form.env")}>
          <TextArea rows={2} value={f.env} onChange={(e) => set({ env: e.target.value })} />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label={t("containers.form.volumes")} hint={t("containers.form.volumesHint")}>
            <TextArea rows={2} value={f.volumes} onChange={(e) => set({ volumes: e.target.value })} placeholder="data:/data" />
          </Field>
          <Field label={t("containers.form.binds")} hint={t("containers.form.bindsHint")}>
            <TextArea rows={2} value={f.binds} onChange={(e) => set({ binds: e.target.value })} placeholder="/srv/site:/usr/share/nginx/html:ro" />
          </Field>
        </div>
        {info?.caps.restartPolicy !== false && (
          <Field label={t("containers.form.restart")}>
            <Select value={f.restart} onChange={(e) => set({ restart: e.target.value })}>
              {["", "always", "unless-stopped", "on-failure"].map((r) => (
                <option key={r} value={r}>
                  {r || t("containers.form.restartNo")}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {error && <p className="text-xs text-danger">{error}</p>}
      </div>
    </Dialog>
  );
}

/** Registry sign-in. The password goes to the engine once, on its stdin. */
function LoginDialog({ sessionId, onClose }: { sessionId: string; onClose: () => void }) {
  const run = useAsyncToast();
  const [registry, setRegistry] = useState("");
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const login = async () => {
    setBusy(true);
    const r = await run(() => api.dockerLogin(sessionId, registry.trim(), user.trim(), password), t("containers.login.done"));
    setBusy(false);
    if (r !== undefined) onClose();
  };
  return (
    <Dialog
      title={t("containers.login.title")}
      size="sm"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" onClick={login} disabled={busy || !user.trim() || !password}>
            {busy ? <Spinner /> : t("containers.login.signIn")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Field label={t("containers.login.registry")} hint={t("containers.login.registryHint")}>
          <Input value={registry} onChange={(e) => setRegistry(e.target.value)} placeholder="ghcr.io" className="w-full" />
        </Field>
        <Field label={t("containers.login.user")}>
          <Input value={user} onChange={(e) => setUser(e.target.value)} className="w-full" />
        </Field>
        <Field label={t("containers.login.password")} hint={t("containers.login.passwordHint")}>
          <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} className="w-full" />
        </Field>
      </div>
    </Dialog>
  );
}

/** The engine the tab drives, its namespace on nerdctl, and on this
 *  computer the binary, context and socket it uses (KTD27). */
function EngineBar({
  sessionId,
  info,
  onUse,
}: {
  sessionId: string;
  info: api.ContainerInfo;
  onUse: (engine: string, namespace: string) => void;
}) {
  const run = useAsyncToast();
  const [namespaces, setNamespaces] = useState<string[]>([]);
  useEffect(() => {
    if (info.caps.namespaces) run(() => api.containerNamespaces(sessionId)).then((n) => n && setNamespaces(n));
  }, [info.caps.namespaces, info.engine, sessionId, run]);
  return (
    <div className="flex flex-wrap items-center gap-3 shrink-0 text-xs text-muted">
      {info.engines.length > 1 && (
        <Select compact value={info.engine} onChange={(e) => onUse(e.target.value, "")} title={t("containers.engine")}>
          {info.engines.map((e) => (
            <option key={e.id} value={e.id}>
              {e.label}
            </option>
          ))}
        </Select>
      )}
      {info.caps.namespaces && (
        <Select compact value={info.namespace} onChange={(e) => onUse(info.engine, e.target.value)} title={t("containers.namespace")}>
          {(namespaces.includes(info.namespace) ? namespaces : [info.namespace, ...namespaces]).map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </Select>
      )}
      {info.local && (
        <span className="font-mono truncate">
          {info.path}
          {info.context && ` · ${info.context}`}
          {info.endpoint && ` · ${info.endpoint}`}
        </span>
      )}
    </div>
  );
}

// ---- Images ----

function Images({ sessionId, info }: { sessionId: string; info: api.ContainerInfo | null }) {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [login, setLogin] = useState(false);
  const [rows, setRows] = useState<api.DockerImage[]>([]);
  const [loading, setLoading] = useState(false);
  const [pullName, setPullName] = useState("");
  const [pulling, setPulling] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    const r = await run(() => api.dockerImages(sessionId));
    if (r) setRows(r);
    setLoading(false);
  }, [sessionId, run]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const pull = async () => {
    const name = pullName.trim();
    if (!name) return;
    setPulling(true);
    const r = await run(() => api.dockerPull(sessionId, name));
    setPulling(false);
    if (r) {
      run(async () => {
        if (r.code && r.code !== 0) throw new Error(r.stderr || t("containers.pullFailed", { code: r.code }));
      }, t("containers.pulled", { name }));
      setPullName("");
      refresh();
    }
  };

  const remove = async (id: string) => {
    if (!(await confirm(t("containers.confirmRemoveImage")))) return;
    const r = await run(() => api.dockerRmi(sessionId, id), t("containers.imageRemoved"));
    if (r !== undefined) refresh();
  };

  const prune = async (what: "dangling" | "all") => {
    if (what === "all" && !(await confirm(t("containers.confirmPruneAll")))) return;
    const r = await run(() => api.dockerPrune(sessionId, what), t("containers.pruned"));
    if (r) refresh();
  };

  return (
    <Panel
      title={t("containers.images")}
      className="flex-1 min-h-0"
      bodyClassName="flex-1 min-h-0 flex flex-col p-4"
      actions={
        <Button variant="ghost" onClick={refresh} disabled={loading}>
          {loading ? <Spinner /> : t("containers.refresh")}
        </Button>
      }
    >
      <div className="flex flex-wrap items-center gap-2 mb-3 shrink-0">
        <Input
          placeholder="repo/image:tag"
          value={pullName}
          onChange={(e) => setPullName(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && pull()}
          className="flex-1 min-w-48"
        />
        <Button variant="primary" onClick={pull} disabled={pulling}>
          {pulling ? <Spinner /> : t("containers.pull")}
        </Button>
        <Button icon={KeyIcon} onClick={() => setLogin(true)}>
          {t("containers.login.title")}
        </Button>
        {info?.caps.prune !== false && (
          <>
            <Button onClick={() => prune("dangling")}>{t("containers.pruneDangling")}</Button>
            <Button variant="danger" onClick={() => prune("all")}>
              {t("containers.pruneAll")}
            </Button>
          </>
        )}
      </div>
      {login && <LoginDialog sessionId={sessionId} onClose={() => setLogin(false)} />}

      {rows.length === 0 && !loading ? (
        <EmptyState>{t("containers.noImages")}</EmptyState>
      ) : (
        <div className="flex-1 min-h-0 overflow-auto">
          <table className="w-full text-sm">
            <thead>
              <tr>
                <th className={th}>{t("containers.col.repoTag")}</th>
                <th className={th}>{t("containers.col.imageId")}</th>
                <th className={th}>{t("containers.col.size")}</th>
                <th className={th}>{t("containers.col.created")}</th>
                <th className={th}>{t("containers.col.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((img) => (
                <tr key={img.id}>
                  <td className={td}>
                    {img.repository}:{img.tag}
                  </td>
                  <td className={`${td} font-mono text-xs`}>{img.id.replace("sha256:", "").slice(0, 12)}</td>
                  <td className={td}>{img.size}</td>
                  <td className={td}>{img.created}</td>
                  <td className={td}>
                    <Button variant="danger" onClick={() => remove(img.id)}>
                      {t("containers.remove")}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}

// ---- Compose ----

const COMPOSE_LABEL: Record<"up" | "down" | "restart" | "ps" | "logs", MessageKey> = {
  up: "containers.compose.up",
  down: "containers.compose.down",
  restart: "containers.compose.restart",
  ps: "containers.compose.ps",
  logs: "containers.compose.logs",
};

function Compose({ sessionId, local }: { sessionId: string; local: boolean }) {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  // On this computer the user picks compose files rather than Hostbay
  // searching the home folder.
  const [picked, setPicked] = useState<string[]>([]);
  const [projects, setProjects] = useState<api.ComposeProject[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [output, setOutput] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true);
    const r = await run(() => api.composeProjects(sessionId, picked));
    if (r) setProjects(r);
    setLoading(false);
  }, [sessionId, run, picked]);

  const pick = async () => {
    const p = await open({ filters: [{ name: t("containers.composeFile"), extensions: ["yml", "yaml"] }] });
    if (typeof p === "string" && !picked.includes(p)) setPicked((l) => [...l, p]);
  };

  useEffect(() => {
    refresh();
  }, [refresh]);

  const act = async (p: api.ComposeProject, action: "up" | "down" | "restart" | "ps" | "logs") => {
    if (action === "down" && !(await confirm(t("containers.confirmDown", { name: p.name })))) return;
    setBusy(`${p.path}:${action}`);
    const r = await run(() => api.composeAction(sessionId, p.path, action), t("containers.composeDone", { action: t(COMPOSE_LABEL[action]), name: p.name }));
    setBusy(null);
    if (r) setOutput(`$ ${p.name} ${action}\n\n${r.stdout || ""}${r.stderr ? "\n[stderr]\n" + r.stderr : ""}`);
  };

  const actions: Array<"up" | "down" | "restart" | "ps" | "logs"> = ["up", "down", "restart", "ps", "logs"];

  return (
    <Panel
      title={t("containers.compose")}
      className="flex-1 min-h-0"
      bodyClassName="flex-1 min-h-0 overflow-auto p-4"
      actions={
        local ? (
          <Button variant="ghost" icon={AddIcon} onClick={pick}>
            {t("containers.openCompose")}
          </Button>
        ) : (
          <Button variant="ghost" onClick={refresh} disabled={loading}>
            {loading ? <Spinner /> : t("containers.rescan")}
          </Button>
        )
      }
    >
      <p className="text-xs opacity-60 mb-3">
        {local
          ? t("containers.composeLocalHint")
          : t("containers.composeRemoteHint")}
      </p>

      {projects.length === 0 && !loading ? (
        <EmptyState>{t("containers.noCompose")}</EmptyState>
      ) : (
        <div className="space-y-2">
          {projects.map((p) => (
            <div
              key={p.path}
              className="flex flex-wrap items-center justify-between gap-2 rounded-control border border-line p-3"
            >
              <div className="min-w-0">
                <div className="font-medium text-sm">{p.name}</div>
                <div className="font-mono text-xs opacity-60 truncate">{p.path}</div>
              </div>
              <div className="flex flex-wrap gap-1.5">
                {actions.map((a) => (
                  <Button
                    key={a}
                    variant={a === "up" ? "primary" : a === "down" ? "danger" : "default"}
                    disabled={busy === `${p.path}:${a}`}
                    onClick={() => act(p, a)}
                  >
                    {busy === `${p.path}:${a}` ? <Spinner /> : t(COMPOSE_LABEL[a])}
                  </Button>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      {output && (
        <div className="mt-4">
          <Output text={output} />
        </div>
      )}
    </Panel>
  );
}

// ---- Stats ----

function Stats({ sessionId, info }: { sessionId: string; info: api.ContainerInfo | null }) {
  const run = useAsyncToast();
  const [rows, setRows] = useState<api.DockerStat[]>([]);
  const [loading, setLoading] = useState(false);
  const [auto, setAuto] = useState(false);
  const [df, setDf] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true);
    const r = await run(() => api.dockerStats(sessionId));
    if (r) setRows(r);
    setLoading(false);
  }, [sessionId, run]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Auto-refresh while enabled. Cleared on unmount / toggle off.
  useEffect(() => {
    if (!auto) return;
    // A tick waits out a slow host's last answer, and the same stats keep
    // the old rows so the table does not re-render.
    let pending = false;
    const timer = setInterval(() => {
      if (pending) return;
      pending = true;
      api
        .dockerStats(sessionId)
        .then((next) => setRows((rows) => (JSON.stringify(rows) === JSON.stringify(next) ? rows : next)))
        .catch(() => {})
        .finally(() => {
          pending = false;
        });
    }, 3000);
    return () => clearInterval(timer);
  }, [auto, sessionId]);

  const loadDf = () => run(() => api.dockerSystemDf(sessionId)).then((r) => r !== undefined && setDf(r));

  return (
    <Panel
      title={t("containers.stats")}
      className="flex-1 min-h-0"
      bodyClassName="flex-1 min-h-0 overflow-auto p-4"
      actions={
        <>
          <label className="flex items-center gap-1.5 text-xs select-none">
            <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
            {t("containers.statsAuto")}
          </label>
          <Button variant="ghost" onClick={refresh} disabled={loading}>
            {loading ? <Spinner /> : t("containers.refresh")}
          </Button>
        </>
      }
    >
      {rows.length === 0 && !loading ? (
        <EmptyState>{t("containers.noRunning")}</EmptyState>
      ) : (
        <div className="overflow-auto">
          <table className="w-full text-sm">
            <thead>
              <tr>
                <th className={th}>{t("containers.col.name")}</th>
                <th className={th}>{t("containers.col.cpu")}</th>
                <th className={th}>{t("containers.col.mem")}</th>
                <th className={th}>{t("containers.col.memPerc")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((s) => (
                <tr key={s.id}>
                  <td className={td}>{s.name}</td>
                  <td className={td}>{s.cpu}</td>
                  <td className={td}>{s.mem}</td>
                  <td className={td}>{s.memPerc}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="mt-4">
        {info?.caps.diskUsage !== false && <Button onClick={loadDf}>{t("containers.diskUsage")}</Button>}
        {df && (
          <div className="mt-3">
            <Output text={df} />
          </div>
        )}
      </div>
    </Panel>
  );
}

export function DockerTab({ conn }: { conn: Connection }) {
  const [sub, setSub] = useState<Sub>("containers");
  const sid = conn.sessionId;
  const run = useAsyncToast();
  // The engine in use; the views remount (rev) when it or its namespace changes.
  const [info, setInfo] = useState<api.ContainerInfo | null>(null);
  const [rev, setRev] = useState(0);
  // A remote host is checked for an engine first, so one without any shows
  // that once rather than a toast per docker command. null: still checking.
  const [missing, setMissing] = useState<boolean | null>(conn.engine ? false : null);
  const load = useCallback(async () => {
    if (!conn.engine) {
      const d = await run(() => api.dashboard(sid));
      const none = !!d && noEngine(d);
      setMissing(none);
      if (none) return;
    }
    const i = await run(() => api.containerInfo(sid));
    if (i) setInfo(i);
  }, [sid, run, conn.engine]);
  useEffect(() => {
    load();
  }, [load]);
  const use = async (engine: string, namespace: string) => {
    if ((await run(() => api.containerUse(sid, engine, namespace))) === undefined) return;
    await load();
    setRev((r) => r + 1);
  };
  const local = !!conn.engine || !!info?.local;
  const items: { id: Sub; label: string }[] = [
    { id: "containers", label: t("containers.title") },
    { id: "images", label: t("containers.images") },
    ...(info?.caps.compose !== false ? [{ id: "compose" as const, label: t("containers.compose") }] : []),
    { id: "stats", label: t("containers.stats") },
    // daemon.json is a remote Docker host's file, edited over SSH.
    ...(!local && (info?.kind ?? "docker") === "docker" ? [{ id: "settings" as const, label: t("containers.settings") }] : []),
  ];
  const shown = items.some((i) => i.id === sub) ? sub : "containers";

  if (missing !== false)
    return (
      <div className="h-full flex flex-col overflow-hidden p-6">
        {missing ? (
          <EmptyState
            icon={DockerIcon}
            title={t("containers.noEngine.title")}
            actions={
              <Button icon={RefreshIcon} onClick={load}>
                {t("containers.noEngine.retry")}
              </Button>
            }
          >
            {t("containers.noEngine.hint")}
          </EmptyState>
        ) : (
          <Spinner />
        )}
      </div>
    );

  return (
    <div className="h-full flex flex-col overflow-hidden p-6 gap-4">
      {info && <EngineBar sessionId={sid} info={info} onUse={use} />}
      <Tabs label={t("view.docker")} value={shown} onChange={setSub} className="shrink-0 border-b border-line" items={items} />

      <div key={rev} className="flex-1 min-h-0 flex flex-col">
        {shown === "containers" && <Containers sessionId={sid} info={info} local={local} />}
        {shown === "images" && <Images sessionId={sid} info={info} />}
        {shown === "compose" && <Compose sessionId={sid} local={local} />}
        {shown === "stats" && <Stats sessionId={sid} info={info} />}
        {shown === "settings" && <LogSettings sessionId={sid} />}
      </div>
    </div>
  );
}
