import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { Browser } from "@wailsio/runtime";
import * as api from "../api";
import type { DetectedVpn, Host, Vpn, VpnState } from "../api";
import { useConnect } from "../connect/useConnect";
import { t, type MessageKey } from "../i18n";
import { PanelHeader } from "../shell/SidePanel";
import { VpnForm, NEW_VPN } from "../shell/settings/Vpns";
import { filterVpns, tagCounts, typeLabel } from "../shell/vpnForm";
import { useHosts } from "../state/hosts";
import { useSessions } from "../state/sessions";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { EmptyState } from "../ui/EmptyState";
import { Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { AddIcon, ImportIcon, RefreshIcon, SearchIcon, StartIcon, StopIcon, VpnIcon } from "../ui/icons";
import { StatusDot, type ConnStatus } from "../ui/StatusDot";
import { useToast } from "../ui/toast";
import { onFilesDropped } from "../importApi";
import { Fingerprint } from "./TrustDialog";
import { VpnImport } from "./VpnImport";

type VpnStates = Record<string, VpnState>;

// One copy of the profiles and their live states for every view that shows
// them (panel, tabs, host menus, palette). It subscribes on first use and
// stays subscribed for the app's life, as the backend's states do.
let snap: { vpns: Vpn[]; states: VpnStates } = { vpns: [], states: {} };
const subs = new Set<() => void>();
let started = false;
const emit = (next: typeof snap) => {
  snap = next;
  subs.forEach((f) => f());
};
/** Reads the saved profiles again, after a save or delete. */
export const reloadVpns = () =>
  api
    .listVpns()
    .then((vpns) => emit({ ...snap, vpns }))
    .catch(() => {});
function subscribe(cb: () => void) {
  subs.add(cb);
  if (!started) {
    started = true;
    reloadVpns();
    // An event that arrived first is newer than the list.
    api
      .vpnStates()
      .then((all) => emit({ ...snap, states: { ...Object.fromEntries(all.map((s) => [s.id, s])), ...snap.states } }))
      .catch(() => {});
    api.onVpnState((s) => emit({ ...snap, states: { ...snap.states, [s.id]: s } }));
  }
  return () => {
    subs.delete(cb);
  };
}
/** The saved profiles and every known live state (unknown is disconnected). */
export const useVpns = () => useSyncExternalStore(subscribe, () => snap);

/** On its way up, or up: Disconnect rather than Connect. */
export const isVpnActive = (s?: VpnState) => !!s && (s.status === "connecting" || s.status === "signing_in" || s.status === "connected");

const STATUS: Record<api.VpnStatus, MessageKey> = {
  disconnected: "vpn.status.disconnected",
  connecting: "vpn.status.connecting",
  signing_in: "vpn.status.signingIn",
  connected: "vpn.status.connected",
  failed: "vpn.status.failed",
  dropped: "vpn.status.dropped",
};
export const vpnStatusText = (s?: VpnState) => t(STATUS[s?.status ?? "disconnected"] ?? "vpn.status.disconnected");

const dot = (s?: VpnState): ConnStatus | undefined =>
  s?.status === "connected"
    ? "live"
    : s?.status === "connecting" || s?.status === "signing_in"
      ? "connecting"
      : s?.status === "failed" || s?.status === "dropped"
        ? "error"
        : undefined;

/** How long a profile has been up, from its since time. */
export function uptime(since: number, now: number) {
  const m = Math.max(0, Math.floor((now - since) / 60000));
  return m >= 60 ? t("vpn.duration.hm", { h: Math.floor(m / 60), m: m % 60 }) : t("vpn.duration.m", { m });
}

/** A gateway certificate a failed connect asks about (R21): the pin
 *  presented and, for a changed one, the saved pin it does not match (AE8),
 *  which is refused and can only be forgotten. */
export type VpnCert = { pin: string; pinned?: string };

/** The certificate a rejected connectVpn asks about, if it does. */
export function vpnCertPrompt(e: unknown): VpnCert | undefined {
  const { code, details } = api.errorInfo(e);
  const reason = details?.reason;
  if (code !== "VPNFailed" || (reason !== "cert-unknown" && reason !== "cert-changed") || !details?.serverCert) return undefined;
  return reason === "cert-changed" ? { pin: details.serverCert, pinned: details.pinned ?? "" } : { pin: details.serverCert };
}

/** What the dialog says about a gateway certificate: a changed one shows
 *  the saved pin beside the new one, with a warning. */
export function VpnCertBody({ name, cert }: { name: string; cert: VpnCert }) {
  const changed = cert.pinned !== undefined;
  return (
    <>
      <p className={"text-sm mb-3 " + (changed ? "text-warning" : "text-muted")}>{t(changed ? "vpn.trust.changed" : "vpn.trust.unknown", { name })}</p>
      <Fingerprint>
        {changed ? `${t("connect.keyChanged.saved")} ${cert.pinned}\n${t("connect.keyChanged.now")} ${cert.pin}` : cert.pin}
      </Fingerprint>
    </>
  );
}

/** Connect and disconnect a profile from anywhere. An untrusted gateway
 *  certificate asks through a trust dialog and connects once trusted; a
 *  changed one is refused and offers only to forget the saved one, after
 *  which connecting asks again (render `dialog` once, R21, AE8). Other
 *  failures show as a toast, or with `quiet` only as the profile's failed
 *  state. */
export function useVpnConnect() {
  const { push } = useToast();
  const { fail, dialog } = useConnect();
  const { vpns } = useVpns();
  const [trust, setTrust] = useState<{ id: string; cert: VpnCert; quiet: boolean } | null>(null);
  const connect = async (id: string, quiet = false): Promise<void> => {
    try {
      await api.connectVpn(id);
    } catch (e) {
      const cert = vpnCertPrompt(e);
      if (cert) setTrust({ id, cert, quiet });
      else fail(e, () => connect(id, quiet), quiet ? { onStep: () => {} } : {});
    }
  };
  const disconnect = (id: string) => api.disconnectVpn(id).catch((e) => push("error", api.errorInfo(e).message));
  const changed = trust?.cert.pinned !== undefined;
  // Either call reseals the secrets to the new pin; a stale copy saved from
  // an editor would undo it, so the list is read again before connecting.
  const answer = (call: () => Promise<unknown>) => {
    const { id, quiet } = trust!;
    setTrust(null);
    call().then(
      () => {
        reloadVpns();
        return connect(id, quiet);
      },
      (e) => push("error", api.errorInfo(e).message)
    );
  };
  const trustDialog = trust && (
    <Dialog
      title={t(changed ? "connect.certChanged.title" : "trust.titleCert")}
      onClose={() => setTrust(null)}
      footer={
        <>
          <Button variant="ghost" onClick={() => setTrust(null)}>
            {t("common.cancel")}
          </Button>
          {changed ? (
            <Button variant="danger" onClick={() => answer(() => api.forgetVpnCert(trust.id))}>
              {t("vpn.trust.forget")}
            </Button>
          ) : (
            <Button variant="primary" onClick={() => answer(() => api.trustVpnCert(trust.id, trust.cert.pin))}>
              {t("trust.confirm")}
            </Button>
          )}
        </>
      }
    >
      <VpnCertBody name={vpns.find((v) => v.id === trust.id)?.name ?? ""} cert={trust.cert} />
    </Dialog>
  );
  return {
    connect,
    disconnect,
    dialog: (
      <>
        {dialog}
        {trustDialog}
      </>
    ),
  };
}

/** What a signing-in profile waits for: a one-time code typed here, or the
 *  user's browser, which Cancel stops waiting on (R20). */
export function VpnSignIn({ id, state }: { id: string; state: VpnState }) {
  const { push } = useToast();
  const [code, setCode] = useState("");
  if (state.status !== "signing_in") return null;
  if (state.need === "browser")
    return (
      <div className="flex items-center gap-2 text-xs">
        <span className="text-muted">{t("vpn.waitingBrowser")}</span>
        {/* Go opened it already and sends only http and https pages. */}
        {state.url && (
          <Button size="sm" variant="ghost" onClick={() => Browser.OpenURL(state.url!)}>
            {t("vpn.openSignIn")}
          </Button>
        )}
        <Button size="sm" variant="ghost" onClick={() => api.disconnectVpn(id).catch((e) => push("error", api.errorInfo(e).message))}>
          {t("common.cancel")}
        </Button>
      </div>
    );
  return (
    <form
      className="flex items-center gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        if (!code.trim()) return;
        api.answerVpn(id, code.trim()).then(
          () => setCode(""),
          (err) => push("error", api.errorInfo(err).message)
        );
      }}
    >
      <Input
        className="!h-7 w-32 text-xs"
        autoFocus
        autoComplete="one-time-code"
        inputMode="numeric"
        aria-label={t("vpn.otp")}
        placeholder={t("vpn.otp")}
        value={code}
        onChange={(e) => setCode(e.target.value)}
      />
      <Button size="sm" type="submit">
        {t("vpn.otpSend")}
      </Button>
    </form>
  );
}

/** The VPN list without its data source, so tests can render it. */
export function VpnPanelView(p: {
  vpns: Vpn[];
  states: VpnStates;
  hosts: Host[];
  /** Per profile, the hosts with a live session through it (R11). */
  using: Record<string, string[]>;
  now: number;
  tag: string;
  text: string;
  /** The last state change, read out by screen readers. */
  announce: string;
  onTag: (tag: string) => void;
  onText: (text: string) => void;
  onNew: () => void;
  onImport: () => void;
  onEdit: (v: Vpn) => void;
  onConnect: (id: string) => void;
  onDisconnect: (id: string) => void;
  /** VPNs found on this computer that no profile names yet (R22); undefined until looked for. */
  detected?: DetectedVpn[];
  onDetect?: () => void;
  onAddDetected?: (d: DetectedVpn) => void;
  /** Opens an external profile's own app, which connects it (R3). */
  onLaunch?: (id: string) => void;
}) {
  const counts = tagCounts(p.hosts, p.vpns);
  // Chips for the tags profiles carry; each counts hosts and profiles.
  const chips = [...counts.entries()].filter(([, c]) => c.vpns > 0).sort((a, b) => a[1].label.localeCompare(b[1].label));
  const shown = filterVpns(p.vpns, p.tag, p.text);
  return (
    <>
      <PanelHeader title={t("nav.vpns")}>
        <IconButton label={t("vpn.import")} icon={ImportIcon} onClick={p.onImport} />
        {p.onDetect && <IconButton label={t("vpn.detect")} icon={RefreshIcon} onClick={p.onDetect} />}
        <IconButton label={t("vpn.new")} icon={AddIcon} onClick={p.onNew} />
      </PanelHeader>
      {p.detected && (
        <section className="px-2 pb-2" aria-label={t("vpn.detected")}>
          <h3 className="px-2 text-xs font-semibold text-muted">{t("vpn.detected")}</h3>
          {p.detected.length === 0 && <p className="px-2 text-xs text-muted">{t("vpn.noneDetected")}</p>}
          <ul>
            {p.detected.map((d) => (
              <li key={d.name} className="flex items-center gap-2 px-2 py-1 text-sm">
                <StatusDot status={d.connected ? "live" : undefined} />
                <span className="truncate flex-1">
                  {d.name} <span className="text-xs text-muted">{typeLabel(d.type)}</span>
                </span>
                <IconButton size="sm" label={t("vpn.addDetected", { name: d.name })} icon={AddIcon} onClick={() => p.onAddDetected?.(d)} />
              </li>
            ))}
          </ul>
        </section>
      )}
      <div role="status" aria-live="polite" className="sr-only">
        {p.announce}
      </div>
      {p.vpns.length > 0 && (
        <div className="px-2 pb-2 flex flex-col gap-1.5">
          <div className="relative">
            <SearchIcon className="size-3.5 text-muted absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none" aria-hidden />
            <Input
              type="search"
              aria-label={t("vpn.search")}
              placeholder={t("vpn.search")}
              value={p.text}
              onChange={(e) => p.onText(e.target.value)}
              className="w-full !h-8 pl-8"
            />
          </div>
          {chips.length > 0 && (
            <div className="flex flex-wrap gap-1" role="group" aria-label={t("vpn.tagFilter")}>
              {chips.map(([key, c]) => {
                const on = p.tag.toLowerCase() === key;
                return (
                  <button
                    key={key}
                    type="button"
                    aria-pressed={on}
                    title={t("vpn.tagCount", { hosts: c.hosts, vpns: c.vpns })}
                    onClick={() => p.onTag(on ? "" : c.label)}
                    className={
                      "h-6 px-2 rounded-full text-xs border " + (on ? "bg-active border-focus text-fg" : "border-line text-muted hover:text-fg hover:bg-hover")
                    }
                  >
                    {c.label} ({c.hosts + c.vpns})
                  </button>
                );
              })}
            </div>
          )}
        </div>
      )}
      {/* Config files dropped here start the import review (F5). */}
      <ul data-file-drop-target className="flex-1 min-h-0 overflow-auto px-2 pb-2 rounded [&.file-drop-target-active]:bg-hover">
        {shown.map((v) => {
          const s = p.states[v.id];
          const active = isVpnActive(s);
          const external = v.tier === "external";
          const users = p.using[v.id] ?? [];
          return (
            <li key={v.id} className="group px-2 py-1 rounded text-sm hover:bg-hover">
              <div className="flex items-center gap-2">
                <StatusDot status={dot(s)} />
                <button className="truncate text-left flex-1" onClick={() => p.onEdit(v)} title={t("vpn.edit", { name: v.name })}>
                  {v.name}
                </button>
                {external ? (
                  <IconButton size="sm" label={t("vpn.openClientNamed", { name: v.name })} icon={StartIcon} onClick={() => p.onLaunch?.(v.id)} />
                ) : (
                  <IconButton
                    size="sm"
                    label={t(active ? "vpn.disconnectNamed" : "vpn.connectNamed", { name: v.name })}
                    icon={active ? StopIcon : StartIcon}
                    onClick={() => (active ? p.onDisconnect(v.id) : p.onConnect(v.id))}
                  />
                )}
              </div>
              <div className="pl-4 text-xs text-muted truncate">
                {vpnStatusText(s)} · {typeLabel(v.type)}
              </div>
              {s?.status === "connected" && (
                <div className="pl-4 text-xs text-muted flex flex-col allow-select">
                  {!!s.iface && <span className="truncate">{t("vpn.iface", { iface: s.iface })}</span>}
                  {!!s.address?.length && <span className="truncate">{t("vpn.address", { address: s.address.join(", ") })}</span>}
                  {!!s.routes?.length && <span className="truncate">{t("vpn.routes", { routes: s.routes.join(", ") })}</span>}
                  {!!s.since && <span>{t("vpn.uptime", { time: uptime(s.since, p.now) })}</span>}
                  {users.length > 0 && <span className="truncate">{t("vpn.usedBy", { hosts: users.join(", ") })}</span>}
                </div>
              )}
              {s?.status === "signing_in" && (
                <div className="pl-4 pt-1">
                  <VpnSignIn id={v.id} state={s} />
                </div>
              )}
              {s?.status === "disconnected" && s.reason && <div className="pl-4 text-xs text-warning break-words allow-select">{s.reason}</div>}
              {(s?.status === "failed" || s?.status === "dropped") && (
                <div className="pl-4 flex items-start gap-2">
                  <span className="text-xs text-danger break-words flex-1 min-w-0 allow-select">{s.reason}</span>
                  <Button size="sm" variant="ghost" onClick={() => p.onConnect(v.id)}>
                    {t("vpn.retry")}
                  </Button>
                </div>
              )}
            </li>
          );
        })}
        {p.vpns.length === 0 ? (
          <li>
            <EmptyState
              icon={VpnIcon}
              title={t("vpn.empty.title")}
              actions={
                <>
                  <Button variant="primary" icon={AddIcon} onClick={p.onNew}>
                    {t("vpn.new")}
                  </Button>
                  <Button icon={ImportIcon} onClick={p.onImport}>
                    {t("vpn.import")}
                  </Button>
                </>
              }
            >
              {t("vpn.empty.description")}
            </EmptyState>
          </li>
        ) : (
          shown.length === 0 && <li className="px-2 py-2 text-xs text-muted">{t("vpn.noMatch")}</li>
        )}
      </ul>
    </>
  );
}

/** Saved VPN profiles with their live state: filter, connect, disconnect,
 *  and open the editor (R7, R8, R9, R11). */
export function VpnPanel() {
  const { vpns, states } = useVpns();
  const { hosts } = useHosts();
  const { sessions } = useSessions();
  const { connect, disconnect, dialog } = useVpnConnect();
  const [tag, setTag] = useState("");
  const [text, setText] = useState("");
  const [editing, setEditing] = useState<Vpn | null>(null);
  const [detected, setDetected] = useState<DetectedVpn[]>();
  const { push } = useToast();
  // Import dialogs, one at a time: a dropped file's path each, or "" to pick one.
  const [importing, setImporting] = useState<{ id: number; path: string }[]>([]);
  const nextImport = useRef(0);
  const queueImport = (paths: string[]) => setImporting((q) => [...q, ...paths.map((path) => ({ id: nextImport.current++, path }))]);
  useEffect(() => onFilesDropped(queueImport), []);
  const [now, setNow] = useState(() => Date.now());
  const [announce, setAnnounce] = useState("");

  useEffect(() => {
    reloadVpns();
    const timer = setInterval(() => setNow(Date.now()), 30000);
    return () => clearInterval(timer);
  }, []);

  // Read out each profile whose state changed since the last render.
  const seen = useRef(states);
  useEffect(() => {
    const changed = Object.values(states).find((s) => seen.current[s.id]?.status !== s.status);
    seen.current = states;
    const v = changed && vpns.find((x) => x.id === changed.id);
    if (v) setAnnounce(t("vpn.announce", { name: v.name, state: vpnStatusText(changed) }));
  }, [states, vpns]);

  const using: Record<string, string[]> = {};
  for (const c of sessions)
    if (c.host.vpnId && c.status === "live" && !c.dialing) (using[c.host.vpnId] ??= []).push(c.host.alias);

  return (
    <>
      <VpnPanelView
        vpns={vpns}
        states={states}
        hosts={hosts}
        using={using}
        now={now}
        tag={tag}
        text={text}
        announce={announce}
        onTag={setTag}
        onText={setText}
        onNew={() => setEditing({ ...NEW_VPN })}
        onImport={() => queueImport([""])}
        onEdit={(v) => setEditing(v)}
        // Failures show on the row through the failed state.
        onConnect={(id) => connect(id, true)}
        onDisconnect={disconnect}
        detected={detected}
        // A refresh may also drop a profile whose app was removed (R23).
        onDetect={() =>
          api.detectVpns().then(
            (d) => {
              setDetected(d);
              reloadVpns();
            },
            (e) => push("error", api.errorInfo(e).message)
          )
        }
        onAddDetected={(d) =>
          api.saveVpn({ id: "", name: d.name, type: d.type, tier: "external", client: d.name, clientPath: d.client, tags: [] }).then(
            () => {
              setDetected((x) => x?.filter((y) => y.name !== d.name));
              reloadVpns();
            },
            (e) => push("error", api.errorInfo(e).message)
          )
        }
        onLaunch={(id) => api.launchVpnClient(id).catch((e) => push("error", api.errorInfo(e).message))}
      />
      {importing.length > 0 && <VpnImport key={importing[0].id} path={importing[0].path || undefined} onClose={() => setImporting((q) => q.slice(1))} />}
      {editing && (
        <VpnForm
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reloadVpns();
          }}
        />
      )}
      {dialog}
    </>
  );
}
