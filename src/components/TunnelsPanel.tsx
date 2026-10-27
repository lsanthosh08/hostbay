import { useEffect, useState } from "react";
import * as api from "../api";
import type { Host, Tunnel, TunnelKind, TunnelState } from "../api";
import { useConnect } from "../connect/useConnect";
import { t, type MessageKey } from "../i18n";
import { PanelHeader } from "../shell/SidePanel";
import { useHosts } from "../state/hosts";
import { EmptyState } from "../ui/EmptyState";
import { StatusDot, type ConnStatus } from "../ui/StatusDot";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { AddIcon, DeleteIcon, StartIcon, StopIcon, TunnelIcon } from "../ui/icons";
import { Select } from "../ui/Select";
import { useToast } from "../ui/toast";
import { usePrompt } from "../ui/prompt";

const KIND_LABEL: Record<TunnelKind, MessageKey> = {
  local: "tunnels.kind.local",
  remote: "tunnels.kind.remote",
  dynamic: "tunnels.kind.dynamic",
};

const isLoopback = (h: string) =>
  h === "" || h === "localhost" || h === "::1" || /^127\.\d+\.\d+\.\d+$/.test(h);

const dot = (s?: TunnelState): ConnStatus | undefined =>
  s?.status === "listening"
    ? "live"
    : s?.status === "starting"
    ? "connecting"
    : s?.status === "failed" || s?.status === "dropped"
    ? "error"
    : undefined;

function describe(tn: Tunnel) {
  const bind = `${tn.bindHost || "127.0.0.1"}:${tn.bindPort}`;
  if (tn.kind === "dynamic") return t("tunnels.describe.dynamic", { bind });
  const target = `${tn.targetHost}:${tn.targetPort}`;
  return tn.kind === "local" ? `${bind} → ${target}` : t("tunnels.describe.remote", { bind, target });
}

const blank = (hostId: string): Tunnel => ({
  id: "",
  hostId,
  name: "",
  kind: "local",
  bindHost: "",
  bindPort: 0,
  targetHost: "127.0.0.1",
  targetPort: 0,
  allowExternal: false,
});

type TunnelStates = Record<string, TunnelState>;

const sameState = (a: TunnelState, b?: TunnelState) =>
  !!b && a.id === b.id && a.status === b.status && a.addr === b.addr && a.error === b.error;

// The same states keep the old map, so a repeated event re-renders nothing.
const keep = (m: TunnelStates, next: TunnelStates) => {
  const ids = Object.keys(next);
  return ids.length === Object.keys(m).length && ids.every((id) => sameState(next[id], m[id])) ? m : next;
};

/** Every tunnel's live state by ID, kept current by the backend's events. */
export function useTunnelStates(onError: (e: unknown) => void = () => {}) {
  const [states, setStates] = useState<TunnelStates>({});
  useEffect(() => {
    api
      .tunnelStates()
      .then((all) => setStates((m) => keep(m, Object.fromEntries(all.map((s) => [s.id, s])))))
      .catch(onError);
    const sub = api.onTunnelState((s) => setStates((m) => keep(m, { ...m, [s.id]: s })));
    return () => {
      sub.then((off) => off());
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return states;
}

/** A tunnel that is listening or on its way there. */
export const isRunning = (s?: TunnelState) => s?.status === "listening" || s?.status === "starting";

/** Saved tunnels for every host, with their live state (R23, R33). */
export function TunnelsPanel() {
  const { hosts } = useHosts();
  const { push } = useToast();
  const { confirm } = usePrompt();
  const [tunnels, setTunnels] = useState<Tunnel[]>([]);
  const [editing, setEditing] = useState<Tunnel | null>(null);
  const { fail: connectFailed, dialog } = useConnect();
  const sshHosts = hosts.filter((h) => h.kind !== "rdp");

  const fail = (e: unknown) => push("error", api.errorInfo(e).message);
  const reload = () => api.listTunnels().then(setTunnels).catch(fail);
  const states = useTunnelStates(fail);

  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const start = async (id: string) => {
    try {
      await api.startTunnel(id);
    } catch (e) {
      // Other failures show on the row through the failed state.
      connectFailed(e, () => start(id), { onStep: () => {} });
    }
  };

  const remove = async (t: Tunnel) => {
    if (!(await confirm(`Delete tunnel "${t.name || describe(t)}"?`))) return;
    api.deleteTunnel(t.id).then(reload).catch(fail);
  };

  const save = async (t: Tunnel) => {
    try {
      await api.saveTunnel(t);
      setEditing(null);
      reload();
    } catch (e) {
      fail(e);
    }
  };

  return (
    <>
      <PanelHeader title={t("nav.tunnels")}>
        <IconButton
          label={t("tunnels.add")}
          icon={AddIcon}
          onClick={() => sshHosts[0] && setEditing(blank(sshHosts[0].id))}
          disabled={sshHosts.length === 0}
        />
      </PanelHeader>
      <ul className="flex-1 min-h-0 overflow-auto px-2 pb-2">
        {tunnels.map((t) => {
          const s = states[t.id];
          const running = isRunning(s);
          const host = hosts.find((h) => h.id === t.hostId);
          return (
            <li key={t.id} className="group px-2 py-1 rounded text-sm hover:bg-hover">
              <div className="flex items-center gap-2">
                <StatusDot status={dot(s)} />
                <button
                  className="truncate text-left flex-1"
                  onClick={() => setEditing(t)}
                  title={`${host?.alias ?? "deleted host"}: ${describe(t)} (${s?.status ?? "stopped"})`}
                >
                  {t.name || describe(t)}
                </button>
                <IconButton
                  size="sm"
                  label={running ? "Stop" : "Start"}
                  icon={running ? StopIcon : StartIcon}
                  onClick={() => (running ? api.stopTunnel(t.id).catch(fail) : start(t.id))}
                />
                <IconButton
                  size="sm"
                  label="Delete"
                  icon={DeleteIcon}
                  tone="danger"
                  className="opacity-0 group-hover:opacity-70 hover:!opacity-100 focus-visible:opacity-100"
                  onClick={() => remove(t)}
                />
              </div>
              <div className="pl-4 text-xs text-muted truncate">
                {s?.status === "listening" ? `listening on ${s.addr}` : s?.status ?? "stopped"}
              </div>
              {s?.error && (
                <div className="pl-4 text-xs text-danger break-words" title={s.error}>
                  {s.error}
                </div>
              )}
            </li>
          );
        })}
        {tunnels.length === 0 && (
          <li>
            <EmptyState icon={TunnelIcon} title={t("tunnels.empty.title")}>
              {t("tunnels.empty.description")}
            </EmptyState>
          </li>
        )}
      </ul>

      {editing && (
        <TunnelForm
          tunnel={editing}
          hosts={sshHosts}
          onSave={save}
          onCancel={() => setEditing(null)}
        />
      )}
      {dialog}
    </>
  );
}

function TunnelForm({
  tunnel,
  hosts,
  onSave,
  onCancel,
}: {
  tunnel: Tunnel;
  hosts: Host[];
  onSave: (t: Tunnel) => void;
  onCancel: () => void;
}) {
  const [draft, setDraft] = useState(tunnel);
  const set = (patch: Partial<Tunnel>) => setDraft((x) => ({ ...x, ...patch }));
  const external = !isLoopback(draft.bindHost.trim());
  const where = t(draft.kind === "remote" ? "tunnels.form.onHost" : "tunnels.form.onComputer");
  const label = "text-xs text-muted flex flex-col gap-1";
  const dirty = JSON.stringify(draft) !== JSON.stringify(tunnel);

  return (
    <Dialog title={t(tunnel.id ? "tunnels.form.edit" : "tunnels.form.new")} onClose={onCancel} dismissible={!dirty}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          onSave({ ...draft, bindHost: draft.bindHost.trim(), allowExternal: external && draft.allowExternal });
        }}
      >
        <label className={label}>
          {t("tunnels.form.name")}
          <Input value={draft.name} onChange={(e) => set({ name: e.target.value })} placeholder={t("tunnels.form.namePlaceholder")} />
        </label>
        <div className="grid grid-cols-2 gap-3">
          <label className={label}>
            {t("tunnels.form.host")}
            <Select value={draft.hostId} onChange={(e) => set({ hostId: e.target.value })}>
              {hosts.map((h) => (
                <option key={h.id} value={h.id}>
                  {h.alias}
                </option>
              ))}
            </Select>
          </label>
          <label className={label}>
            {t("tunnels.form.type")}
            <Select value={draft.kind} onChange={(e) => set({ kind: e.target.value as TunnelKind })}>
              {(Object.keys(KIND_LABEL) as TunnelKind[]).map((k) => (
                <option key={k} value={k}>
                  {t(KIND_LABEL[k])}
                </option>
              ))}
            </Select>
          </label>
        </div>
        <div className="grid grid-cols-[1fr_7rem] gap-3">
          <label className={label}>
            {t("tunnels.form.listen", { where })}
            <Input value={draft.bindHost} onChange={(e) => set({ bindHost: e.target.value })} placeholder="127.0.0.1" />
          </label>
          <label className={label}>
            {t("tunnels.form.port")}
            <Input
              type="number"
              min={1}
              max={65535}
              required
              value={draft.bindPort || ""}
              onChange={(e) => set({ bindPort: Number(e.target.value) })}
            />
          </label>
        </div>
        {draft.kind !== "dynamic" && (
          <div className="grid grid-cols-[1fr_7rem] gap-3">
            <label className={label}>
              {t(draft.kind === "local" ? "tunnels.form.destFromHost" : "tunnels.form.destFromComputer")}
              <Input required value={draft.targetHost} onChange={(e) => set({ targetHost: e.target.value })} />
            </label>
            <label className={label}>
              {t("tunnels.form.port")}
              <Input
                type="number"
                min={1}
                max={65535}
                required
                value={draft.targetPort || ""}
                onChange={(e) => set({ targetPort: Number(e.target.value) })}
              />
            </label>
          </div>
        )}
        {external && (
          <div className="rounded-control border border-warning/60 bg-warning/10 p-3 text-xs flex flex-col gap-2">
            <p>
              <b>{draft.bindHost}</b>{" "}
              {t("tunnels.form.notLoopback", {
                to: t(draft.kind === "remote" ? "tunnels.form.reachComputer" : "tunnels.form.reachHostNetwork"),
              })}
            </p>
            <label className="flex items-center gap-2">
              <input
                type="checkbox"
                checked={draft.allowExternal}
                onChange={(e) => set({ allowExternal: e.target.checked })}
              />
              {t("tunnels.form.allowExternal")}
            </label>
          </div>
        )}
        <p className="text-xs text-muted">
          {t("tunnels.form.loopbackNote")}
        </p>
        <div className="flex gap-2 justify-end">
          <Button type="button" variant="ghost" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" disabled={external && !draft.allowExternal}>
            {t("tunnels.form.save")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
