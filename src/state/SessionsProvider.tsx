import { useEffect, useMemo, useReducer, useRef, useState, type ReactNode } from "react";
import * as api from "../api";
import { isDatabaseHost, isFileHost, type Host } from "../api";
import { route } from "../connect/router";
import { databaseCapabilities } from "../tabs/database/capabilities";
import { clusters } from "../tabs/kubernetes/clusters";
import { t } from "../i18n";
import { useToast } from "../ui/toast";
import { SerialDialog } from "../components/SerialDialog";
import { leaves, rename } from "../workspace/layout";
import { fromSaved, openInOrder, waiting } from "../workspace/saved";
import { useHosts } from "./hosts";
import {
  initialSessions,
  layoutOf,
  sessionsReducer,
  SessionsCtx,
  type Connection,
  type LocalTerm,
  type Sessions,
  type StatusMap,
} from "./sessions";

/** Open sessions and the connect flow: a session tab opens at once, and a
 *  failed connect leaves the router's next step in it (src/connect/). */
export function SessionsProvider({ children }: { children: ReactNode }) {
  const { push } = useToast();
  const { hosts, reloadHosts } = useHosts();
  const [state, dispatch] = useReducer(sessionsReducer, initialSessions);
  const { sessions, activeId } = state;
  // Tab IDs with a dial in flight, so a retry that lands mid-dial does not
  // dial twice. A ref is current inside a click handler before the next render.
  const dialing = useRef(new Set<string>());
  // The latest sessions, for calls that finish after a later render.
  const sessionsRef = useRef(sessions);
  sessionsRef.current = sessions;
  const [serialDialog, setSerialDialog] = useState(false);
  // Panes of an opening workspace, each waiting for its connect to settle.
  const waiters = useRef(new Map<string, () => void>());
  useEffect(() => {
    for (const [id, done] of waiters.current)
      if (!waiting(sessions.find((c) => c.sessionId === id))) {
        waiters.current.delete(id);
        done();
      }
  }, [sessions]);

  // Liveness: the backend reports when a session's SSH connection drops and
  // when a later call rebuilds it, so the dot never lies and nothing polls.
  const sshSessions = sessions
    .filter((c) => !c.sshless && !c.dialing)
    .map((c) => c.sessionId)
    .join(" ");
  useEffect(() => {
    const ids = sshSessions ? sshSessions.split(" ") : [];
    const subs = ids.map((id) => api.onConnState(id, (state) => dispatch({ type: "connState", sessionId: id, state })));
    return () => subs.forEach((p) => p.then((off) => off()));
  }, [sshSessions]);

  const open = (conn: Omit<Connection, "status">) => dispatch({ type: "open", conn: { ...conn, status: "live" } });

  // Open an RDP-only session (no SSH workspace): one Remote Desktop tab.
  const openRdpOnly = (host: Host, sessionId = crypto.randomUUID()) => open({ sessionId, host, sshless: true });

  // Open a local shell or serial port in a terminal-only tab. Its stand-in
  // host is never saved; the tab's ID owns the terminal, as for Telnet.
  const openLocalTerm = (name: string, term: LocalTerm) => {
    const sessionId = crypto.randomUUID();
    const host: Host = { id: sessionId, alias: name, hostname: "", port: 0, user: "", identityFile: "", tags: [] };
    open({ sessionId, host, sshless: true, term });
  };

  // The Files tab: two panes over this computer's disk, with a stand-in host.
  const openFiles = () => {
    const existing = sessionsRef.current.find((c) => c.files);
    if (existing) return dispatch({ type: "activate", sessionId: existing.sessionId });
    const sessionId = crypto.randomUUID();
    const host: Host = { id: sessionId, alias: t("files.title"), hostname: "", port: 0, user: "", identityFile: "", tags: [] };
    open({ sessionId, host, sshless: true, files: true });
  };

  // The Containers tab for an engine on this computer: a stand-in session
  // the backend resolved (binary, socket) under the ID it returned. The tab
  // switches engine itself; this opens the first one found. A second click
  // while the first open is on its way does nothing, rather than opening
  // another engine session.
  const openingContainers = useRef(false);
  const openContainers = async () => {
    const existing = sessionsRef.current.find((c) => c.engine);
    if (existing) return dispatch({ type: "activate", sessionId: existing.sessionId });
    if (openingContainers.current) return;
    openingContainers.current = true;
    try {
      const [engine] = await api.containerLocalEngines();
      if (!engine) return push("error", t("containers.none"));
      const sessionId = await api.containerOpenLocal(engine.id);
      const host: Host = { id: sessionId, alias: t("containers.title"), hostname: "", port: 0, user: "", identityFile: "", tags: [] };
      open({ sessionId, host, sshless: true, engine: engine.id });
    } catch (e) {
      push("error", api.errorInfo(e).message);
    } finally {
      openingContainers.current = false;
    }
  };

  // A shell in a container, or its followed logs, in a terminal tab of its own.
  const openContainerTerm = (engineSession: string, containerId: string, name: string, follow: boolean) =>
    openLocalTerm(t(follow ? "containers.logsTab" : "containers.shellTab", { name }), {
      kind: "container",
      engineSession,
      containerId,
      shell: "sh",
      follow,
    });

  // A pod shell in a terminal tab of its own; the cluster tab owns the
  // terminal in Go, so closing the cluster closes it too.
  const openPodTerm = (clusterId: string, namespace: string, pod: string, container: string, command: string[]) =>
    openLocalTerm(t("kubernetes.shellTab", { name: `${pod}/${container}` }), { kind: "pod", clusterId, namespace, pod, container, command });

  // A VNC or SPICE host's screen, or an SSH host's X11 desktop: the tab's
  // session ID is a vncId (spiceId for SPICE) once live.
  const screen = (host: Host, x11?: boolean) => host.kind === "vnc" || host.kind === "spice" || !!x11;

  // Ends what a session holds in the backend: a file host's transfer
  // source, an open database, a SPICE session, a VNC sign-in or X11
  // desktop, or the SSH session.
  const release = (host: Host, sessionId: string, x11?: boolean) =>
    (isFileHost(host)
      ? api.transferDisconnect(sessionId)
      : isDatabaseHost(host)
        ? (databaseCapabilities.delete(sessionId), api.databaseClose(sessionId))
      : host.kind === "kubernetes"
        ? (clusters.delete(sessionId), api.kubernetesClose(sessionId))
      : host.kind === "spice"
        ? api.spiceClose(sessionId)
        : screen(host, x11)
          ? api.vncClose(sessionId)
          : api.disconnect(sessionId)
    ).catch(() => {});

  // Dialing tabs closed before their connect returned; a late session is
  // released instead of opened.
  const cancelled = useRef(new Set<string>());

  // Opens host's tab; dial, when there is one, runs its connect.
  const begin = (host: Host): { sessionId: string; dial?: () => Promise<void> } => {
    const sessionId = crypto.randomUUID();
    // RDP-only host: when the SSH and RDP ports are the same, there's no
    // separate SSH server to reach — skip the SSH attempt and open the Remote
    // Desktop tab directly (no workspace tabs).
    const rdpPort = host.rdpPort || 3389;
    if (host.kind === "rdp" && (host.port || 22) === rdpPort) {
      openRdpOnly(host, sessionId);
      return { sessionId };
    }

    // Telnet and raw TCP need no SSH session; the terminal tab dials the
    // host itself, through its jump host or proxy.
    if (host.kind === "telnet" || host.kind === "tcp") {
      open({ sessionId, host, sshless: true });
      return { sessionId };
    }

    // The tab opens at once and shows the connect in place (AE16). Its ID
    // is its own until the backend's session ID arrives.
    dispatch({ type: "open", conn: { sessionId, host, dialing: true } });
    return { sessionId, dial: () => dial(sessionId, host) };
  };

  // Already connected? Just focus its tab (an X11 desktop is not the host's workspace).
  const sessionOf = (host: Host) => sessionsRef.current.find((c) => c.host.id === host.id && !c.x11);

  const connect = async (host: Host) => {
    const existing = sessionOf(host);
    if (existing) return dispatch({ type: "activate", sessionId: existing.sessionId });
    await begin(host).dial?.();
  };

  // A saved workspace opens as one split tab, its panes connecting one at
  // a time so prompts never overlap (KTD40). A host already open brings its
  // session along, since a host has one session; a deleted one is listed.
  const openWorkspace = async (ws: api.Workspace) => {
    const { tree, missing } = fromSaved(ws.layout, hosts);
    if (missing.length) push("info", t(tree === null ? "workspace.allMissing" : "workspace.missing", { name: ws.name, n: missing.length }));
    if (tree === null) return;
    let panes = tree;
    const dials: Record<string, () => Promise<void>> = {};
    for (const hostId of leaves(tree)) {
      const host = hosts.find((h) => h.id === hostId)!;
      const open = sessionOf(host);
      const started = open ? { sessionId: open.sessionId } : begin(host);
      if (started.dial) dials[started.sessionId] = started.dial;
      panes = rename(panes, hostId, started.sessionId);
    }
    if (typeof panes === "string") dispatch({ type: "activate", sessionId: panes });
    else dispatch({ type: "layout", tree: panes, name: ws.name });
    await openInOrder(
      leaves(panes).filter((id) => dials[id]),
      (id) => !cancelled.current.has(id) && (void dials[id](), true),
      (id) => new Promise((done) => waiters.current.set(id, done))
    );
  };

  // An SSH host's X11 desktop opens in a tab of its own, dialled and
  // retried through the same connect flow as the host's workspace.
  const openX11Desktop = async (host: Host) => {
    const existing = sessionsRef.current.find((c) => c.host.id === host.id && c.x11);
    if (existing) return dispatch({ type: "activate", sessionId: existing.sessionId });
    const tabId = crypto.randomUUID();
    dispatch({ type: "open", conn: { sessionId: tabId, host, x11: true, dialing: true } });
    await dial(tabId, host, true);
  };

  // Runs the connect for a dialing tab, and on failure leaves the router's
  // step in the tab until the user acts (src/connect/router.ts).
  const dial = async (tabId: string, host: Host, x11?: boolean) => {
    if (dialing.current.has(tabId)) return;
    dialing.current.add(tabId);
    dispatch({ type: "step", sessionId: tabId, host });
    try {
      // Windows (rdp) + Linux both connect over SSH for the tab workspace
      // (Dashboard/Files/IIS or Dashboard/Terminal/Files/...). RDP is a tab
      // that connects to the desktop on demand.
      // A file host signs in over the transfer service, and its tab's
      // session ID becomes the transfer source ID. A VNC host signs in in
      // Go (KTD19), and its tab's session ID becomes the vncId VncTab streams;
      // so does an X11 desktop's, which Go starts over the host's SSH connection,
      // and a SPICE host's spiceId, which SpiceTab streams per channel. A
      // database host opens in Go too; its tab's session ID becomes the
      // databaseId DatabaseTab queries.
      const file = isFileHost(host);
      const db = isDatabaseHost(host);
      const k8s = host.kind === "kubernetes";
      const vnc = screen(host, x11);
      const sessionId = file
        ? (await api.transferConnect(host.id)).id
        : db
          ? await api.databaseConnect(host.id).then((d) => (databaseCapabilities.set(d.databaseId, d.capabilities), d.databaseId))
        : k8s
          ? await api.kubernetesConnect(host.id).then((c) => (clusters.set(c.clusterId, c), c.clusterId))
        : x11
          ? await api.x11DesktopOpen(host.id)
          : host.kind === "spice"
            ? await api.spiceConnect(host.id)
            : vnc
              ? await api.vncConnect(host.id)
              : await api.connect(host.id);
      if (cancelled.current.has(tabId)) {
        release(host, sessionId, x11);
        return;
      }
      // Mosh only borrows the SSH session to start mosh-server, file,
      // database and VNC hosts have no SSH connection, and an X11 desktop's
      // ends with its screen, so none follows an SSH connection's state.
      dispatch({ type: "dialed", from: tabId, sessionId, sshless: host.kind === "mosh" || file || db || k8s || vnc });
      // Connect stamped recency for the "Recent" list and may have detected
      // the MAC; saving this stale copy of the host would wipe the MAC.
      reloadHosts().catch(() => {});
    } catch (e) {
      if (cancelled.current.has(tabId)) return;
      // Windows host with no SSH server reachable -> RDP-only fallback: the
      // tab only shows Remote Desktop (workspace tabs need SSH).
      if (host.kind === "rdp") {
        dispatch({ type: "dialed", from: tabId, sessionId: tabId, sshless: true });
        dispatch({ type: "view", sessionId: tabId, view: "rdp" });
        push("info", t("connect.rdpOnly", { name: host.alias }));
        return;
      }
      dispatch({ type: "step", sessionId: tabId, step: route(api.errorInfo(e), { host, hosts }) });
    } finally {
      dialing.current.delete(tabId);
    }
  };

  const retry = (sessionId: string) => {
    const c = sessionsRef.current.find((x) => x.sessionId === sessionId);
    // The host may have been edited from the failure; dial what is saved now.
    if (c?.dialing) dial(sessionId, hosts.find((h) => h.id === c.host.id) ?? c.host, c.x11);
  };

  // Rebuilds a dropped session's connection; a failure stays on the banner.
  // A VNC session or X11 desktop cannot be rebuilt under its tab: the tab
  // signs in or starts the desktop again.
  const reconnect = async (sessionId: string) => {
    const c = sessionsRef.current.find((x) => x.sessionId === sessionId);
    if (c && screen(c.host, c.x11)) {
      dispatch({ type: "redial", sessionId });
      return dial(sessionId, hosts.find((h) => h.id === c.host.id) ?? c.host, c.x11);
    }
    try {
      await api.reconnect(sessionId);
      dispatch({ type: "connState", sessionId, state: "live" });
    } catch (e) {
      dispatch({ type: "step", sessionId, step: route(api.errorInfo(e), { host: c?.host, wasLive: true }) });
    }
  };

  // Send a Wake-on-LAN magic packet: from this Mac, and via any live SSH
  // session as a relay (covers the case where the Mac isn't on the LAN).
  const wakeHost = async (host: Host) => {
    if (!host.mac) {
      push("error", t("connect.wake.noMac"));
      return;
    }
    let sent = false;
    try {
      await api.wolWake(host.mac);
      sent = true;
    } catch {
      /* Mac may not be on the LAN — fall through to a relay. */
    }
    // Also try any currently-live SSH session as a relay.
    const relay = sessionsRef.current.find((c) => c.status === "live" && !c.sshless);
    if (relay) {
      try {
        await api.wolWakeVia(relay.sessionId, host.mac);
        sent = true;
      } catch {
        /* ignore relay failure */
      }
    }
    push(sent ? "success" : "error", sent ? t("connect.wake.sent", { name: host.alias }) : t("connect.wake.failed"));
  };

  const disconnect = (sessionId: string) => {
    const c = sessionsRef.current.find((x) => x.sessionId === sessionId);
    // A dialing tab closed mid-dial, or a workspace pane closed before its turn.
    if (dialing.current.has(sessionId) || c?.dialing) cancelled.current.add(sessionId);
    // A dialing file host, database host, VNC host or X11 desktop holds
    // nothing yet, and the Files tab holds none; everything else is an SSH
    // session or a terminal's owner ID.
    if (c && (isFileHost(c.host) || isDatabaseHost(c.host) || c.host.kind === "kubernetes" || screen(c.host, c.x11))) {
      if (!c.dialing) release(c.host, sessionId, c.x11);
    } else if (c?.engine) api.containerClose(sessionId).catch(() => {});
    else if (!c?.files) api.disconnect(sessionId).catch(() => {});
    dispatch({ type: "close", sessionId });
  };

  // The functions read only state, hosts, push and reloadHosts (the rest are
  // refs and dispatch), so the value is new only when one of those changes.
  const value = useMemo<Sessions>(() => {
    // host.id -> current status (for dots on rows everywhere).
    const statusByHost: StatusMap = {};
    for (const c of sessions) statusByHost[c.host.id] = c.status;
    return {
      ...state,
      active: sessions.find((c) => c.sessionId === activeId) ?? null,
      statusByHost,
      connect,
      openX11Desktop,
      disconnect,
      activate: (sessionId) => dispatch({ type: "activate", sessionId }),
      setView: (sessionId, view) => dispatch({ type: "view", sessionId, view }),
      move: (from, before) => dispatch({ type: "move", from, before }),
      openLocalShell: (s) => openLocalTerm(s.name, { kind: "local", shellId: s.id }),
      openSerial: () => setSerialDialog(true),
      openFiles,
      openContainers,
      openContainerTerm,
      openPodTerm,
      retry,
      reconnect,
      wakeHost,
      setStatus: (sessionId, status) => dispatch({ type: "status", sessionId, status }),
      showSettings: (section) => dispatch({ type: "settings", section }),
      split: (from, to, side) => dispatch({ type: "split", from, to, side }),
      unsplit: (sessionId) => dispatch({ type: "unsplit", sessionId }),
      resizePanes: (layoutId, path, index, delta) => dispatch({ type: "resize", layoutId, path, index, delta }),
      openWorkspace,
      // A pane's cancelled prompt leaves it on Retry, so the rest of its
      // tab stays; a plain tab closes.
      cancelPrompt: (sessionId) =>
        layoutOf(state, sessionId)
          ? dispatch({ type: "step", sessionId, step: { kind: "failure", message: t("pane.cancelled") } })
          : disconnect(sessionId),
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state, hosts, push, reloadHosts]);

  return (
    <SessionsCtx.Provider value={value}>
      {children}
      {serialDialog && (
        <SerialDialog
          onCancel={() => setSerialDialog(false)}
          onOpen={(serial) => {
            setSerialDialog(false);
            openLocalTerm(`${serial.port} @ ${serial.baud}`, { kind: "serial", serial });
          }}
        />
      )}
    </SessionsCtx.Provider>
  );
}
