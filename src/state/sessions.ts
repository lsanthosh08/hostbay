import { createContext, useContext } from "react";
import { isDatabaseHost, isFileHost, type Host } from "../api";
import type * as api from "../api";
import { route, type Step } from "../connect/router";
import { t, type MessageKey } from "../i18n";
import { statusLabel, type ConnStatus } from "../ui/StatusDot";
import { leaves, remove, rename, resize, split, type Pane, type Side } from "../workspace/layout";

export type { ConnStatus };

/** A view a session offers; the session toolbar switches between them. */
export type TabKey = "dashboard" | "terminal" | "files" | "docker" | "services" | "monitor" | "iis" | "rdp" | "vnc" | "database" | "kubernetes";

// sshless: no SSH workspace — a Windows host with no reachable SSH (RDP-only),
// a Telnet, raw TCP or Mosh host (terminal only), or a VNC host, whose
// session ID once live is the vncId that vncConnect returned, or a SPICE
// host, whose session ID once live is the spiceId of spiceConnect. x11: an SSH
// host's X11 desktop, a tab of its own beside any SSH workspace, whose
// session ID once live is the vncId that x11DesktopOpen returned (R47). A
// database host is sshless too: its session ID once live is the databaseId
// of databaseConnect (R56).
// term: a local shell or serial
// port on this computer, or a terminal on a container (a shell, or with
// follow its logs) through the engine of session engineSession; its host
// is a stand-in named after it.
export type LocalTerm =
  | { kind: "local"; shellId: string }
  | { kind: "serial"; serial: api.SerialSettings }
  | { kind: "container"; engineSession: string; containerId: string; shell: string; follow: boolean }
  // A shell in a pod's container, through the cluster tab clusterId (R57).
  | { kind: "pod"; clusterId: string; namespace: string; pod: string; container: string; command: string[] };
// dialing: the tab opened before the connect finished (AE16), so sessionId
// is the tab's own until the backend's arrives, and the body shows the
// connect's progress and any step in place of the views. step: what the user
// must answer, or after going live, the reconnect banner. files: the Files
// tab, two panes over this computer's disk with a stand-in host. A file host
// (FTP, SMB, WebDAV, S3) is a Files-only session whose ID, once live, is the
// transfer source ID that transferConnect returned. engine: the Containers
// tab for an engine on this computer (its containerLocalEngines ID), a
// stand-in session whose ID containerOpenLocal returned.
export type Connection = {
  sessionId: string;
  host: Host;
  status: ConnStatus;
  sshless?: boolean;
  x11?: boolean;
  term?: LocalTerm;
  files?: boolean;
  engine?: string;
  dialing?: boolean;
  step?: Step;
};

/** activeId while the Settings tab shows. */
export const SETTINGS_TAB = "settings";
/** The Settings tab's categories, in its list order. */
export const SETTINGS_SECTIONS = ["general", "appearance", "terminal", "keyboard", "security", "keys", "proxies", "ai", "sync", "backup", "about", "logging", "skills", "mcp"] as const;
export type SettingsSection = (typeof SETTINGS_SECTIONS)[number];
export const SECTION_LABEL: Record<SettingsSection, MessageKey> = {
  general: "settings.general",
  appearance: "settings.appearance",
  terminal: "settings.terminal",
  keyboard: "settings.keyboard",
  security: "settings.security",
  keys: "settings.keys",
  proxies: "settings.proxies",
  ai: "settings.ai",
  sync: "settings.sync",
  backup: "settings.backup",
  about: "settings.about",
  logging: "settings.logging",
  skills: "settings.skills",
  mcp: "settings.mcp",
};

/** Telnet, raw TCP and Mosh hosts open a terminal tab and nothing else. */
export const terminalOnly = (h: Host) => h.kind === "telnet" || h.kind === "tcp" || h.kind === "mosh";

/** host.id -> live status, for dots on rows that aren't the active tab. */
export type StatusMap = Record<string, ConnStatus>;

/** The views a session offers, by host kind; one view needs no switcher. */
export function viewsFor(c: Connection): TabKey[] {
  if (c.term || terminalOnly(c.host)) return ["terminal"];
  if (c.files || isFileHost(c.host)) return ["files"];
  if (c.engine) return ["docker"];
  if (isDatabaseHost(c.host)) return ["database"];
  // A cluster's session ID once live is the clusterId of kubernetesConnect.
  if (c.host.kind === "kubernetes") return ["kubernetes"];
  if (c.host.kind === "rdp") return c.sshless ? ["rdp"] : ["dashboard", "files", "iis", "rdp"];
  // "vnc" is the remote screen view, SPICE's too.
  if (c.host.kind === "vnc" || c.host.kind === "spice" || c.x11) return ["vnc"];
  return ["dashboard", "terminal", "files", "docker", "services", "monitor"];
}

/** A tab whose first connect failed: it was never connected. */
export const neverConnected = (c: Connection) => !!c.dialing && c.status === "error";

/** A session's state in words: a tab that never connected has not "lost"
 *  anything. */
export const connLabel = (c: Connection) => (neverConnected(c) ? t("status.failed") : statusLabel(c.status));

/** The port a database server listens on when its host leaves it 0. */
const DB_PORTS = { postgres: 5432, mysql: 3306, mssql: 1433, oracle: 1521 } as const;

/** Where a session is connected, for the toolbar and the status bar. */
export function sessionAddress(c: Connection): string {
  if (c.term?.kind === "local") return t("session.localShell");
  // A serial port's stand-in host is already named "port @ baud".
  if (c.term) return c.host.alias;
  if (c.files || c.engine) return t("files.local");
  const h = c.host;
  if (h.kind === "webdav" || h.kind === "kubernetes") return h.url ?? "";
  if (h.kind === "s3") return `${h.bucket} @ ${h.url}`;
  if (isFileHost(h)) return `${h.user ? `${h.user}@` : ""}${h.hostname}${h.port ? `:${h.port}` : ""}`;
  if (h.kind === "vnc") return `${h.user ? `${h.user}@` : ""}${h.hostname}:${h.port || 5900}`;
  if (h.kind === "spice") return `${h.hostname}:${h.port || 5900}`;
  if (h.kind === "sqlite") return h.file ?? "";
  if (h.kind === "redis" || h.kind === "mongodb")
    return `${h.user ? `${h.user}@` : ""}${h.hostname}:${h.port || (h.kind === "redis" ? 6379 : 27017)}${h.database ? `/${h.database}` : ""}`;
  if (h.kind === "elasticsearch" || h.kind === "rqlite") return h.url ?? "";
  if (h.kind && h.kind in DB_PORTS) return `${h.user}@${h.hostname}:${h.port || DB_PORTS[h.kind as keyof typeof DB_PORTS]}${h.database ? `/${h.database}` : ""}`;
  const at = `${c.host.hostname}:${c.host.port}`;
  if (c.x11) return t("session.x11Desktop", { address: `${c.host.user}@${at}` });
  return c.host.kind === "telnet" || c.host.kind === "tcp" ? at : `${c.host.user}@${at}`;
}

/** A tab split into panes (KTD40): its sessions' layout, the pane it
 *  last focused, and the saved workspace's name when it opened one. Every
 *  layout has two panes or more; one left with a single pane is a plain tab
 *  again. */
export type Layout = { id: string; tree: Pane; focus: string; name?: string };

export type SessionsState = {
  /** Open sessions in tab order; a split tab sits where its first one does. */
  sessions: Connection[];
  /** The session shown, or with panes the focused one; null shows the
   *  Start page, SETTINGS_TAB the Settings tab. */
  activeId: string | null;
  /** Each session remembers the view it was on. */
  views: Record<string, TabKey>;
  /** The Settings tab's category while it is open, else null. */
  settings: SettingsSection | null;
  layouts: Layout[];
};

export const initialSessions: SessionsState = { sessions: [], activeId: null, views: {}, settings: null, layouts: [] };

/** The split tab holding session id, if any. */
export const layoutOf = (s: SessionsState, id: string | null) => s.layouts.find((l) => id !== null && leaves(l.tree).includes(id));

/** The tabs in order: a plain tab's ID is its session's, a split tab's its
 *  layout's. */
export function tabsOf(s: SessionsState): { id: string; sessions: string[]; plain: boolean }[] {
  const tabs: { id: string; sessions: string[]; plain: boolean }[] = [];
  for (const c of s.sessions) {
    const l = layoutOf(s, c.sessionId);
    if (!l) tabs.push({ id: c.sessionId, sessions: [c.sessionId], plain: true });
    else if (!tabs.some((t) => t.id === l.id)) tabs.push({ id: l.id, sessions: leaves(l.tree), plain: false });
  }
  return tabs;
}

/** The active tab's ID. */
export const activeTab = (s: SessionsState) => layoutOf(s, s.activeId)?.id ?? s.activeId;

// Drops layouts left with one pane and keeps each focus on one of its
// panes, the active session where it is one.
function tidy(s: SessionsState): SessionsState {
  let changed = false;
  const layouts = s.layouts.flatMap((l) => {
    const panes = leaves(l.tree);
    if (panes.length < 2) {
      changed = true;
      return [];
    }
    const focus = s.activeId && panes.includes(s.activeId) ? s.activeId : panes.includes(l.focus) ? l.focus : panes[0];
    if (focus === l.focus) return [l];
    changed = true;
    return [{ ...l, focus }];
  });
  return changed ? { ...s, layouts } : s;
}

// Takes sessions out of the layouts that hold them.
function without(layouts: Layout[], ids: string[]) {
  const next = layouts.flatMap((l) => {
    const tree = ids.reduce<Pane | null>((t, id) => t && remove(t, id), l.tree);
    return tree === l.tree ? [l] : tree ? [{ ...l, tree }] : [];
  });
  return next.length === layouts.length && next.every((l, i) => l === layouts[i]) ? layouts : next;
}

export type SessionsAction =
  | { type: "open"; conn: Omit<Connection, "status"> & { status?: ConnStatus }; view?: TabKey }
  | { type: "connState"; sessionId: string; state: "live" | "dropped" }
  | { type: "status"; sessionId: string; status: ConnStatus }
  // host: the saved host as it is now, when a retry follows an edit.
  | { type: "step"; sessionId: string; step?: Step; host?: Host }
  | { type: "dialed"; from: string; sessionId: string; sshless?: boolean }
  // A live tab dials again in place (VNC: a new sign-in and vncId).
  | { type: "redial"; sessionId: string }
  | { type: "settings"; section: SettingsSection | null }
  | { type: "close"; sessionId: string }
  | { type: "activate"; sessionId: string | null }
  | { type: "view"; sessionId: string; view: TabKey }
  // from and before are tab IDs (tabsOf).
  | { type: "move"; from: string; before: string }
  // Puts session `from` in a pane beside session `to`'s (R65).
  | { type: "split"; from: string; to: string; side: Side }
  | { type: "unsplit"; sessionId: string }
  | { type: "resize"; layoutId: string; path: number[]; index: number; delta: number }
  // Open sessions laid out as one tab: a saved workspace opening.
  | { type: "layout"; tree: Pane; name?: string };

// Field by field; an absent field equals one set to undefined.
const same = (a: Connection, b: Connection) =>
  [...Object.keys(a), ...Object.keys(b)].every((k) => a[k as keyof Connection] === b[k as keyof Connection]);

// An edit that changes nothing keeps the old state, so nothing re-renders.
const edit = (s: SessionsState, id: string, fn: (c: Connection) => Connection): SessionsState => {
  let changed = false;
  const sessions = s.sessions.map((c) => {
    if (c.sessionId !== id) return c;
    const next = fn(c);
    if (same(c, next)) return c;
    changed = true;
    return next;
  });
  return changed ? { ...s, sessions } : s;
};

export const sessionsReducer = (s: SessionsState, a: SessionsAction): SessionsState => tidy(reduce(s, a));

function reduce(s: SessionsState, a: SessionsAction): SessionsState {
  switch (a.type) {
    case "open": {
      const conn: Connection = { status: "connecting", ...a.conn };
      const view = a.view ?? viewsFor(conn)[0];
      return {
        ...s,
        sessions: [...s.sessions, conn],
        activeId: conn.sessionId,
        views: { ...s.views, [conn.sessionId]: view },
      };
    }
    case "connState":
      // The backend reports when a session's connection drops and when a
      // later call rebuilds it, so the dot never lies and nothing polls.
      return edit(s, a.sessionId, (c) =>
        a.state === "live"
          ? { ...c, status: "live", step: undefined }
          : { ...c, status: "error", step: route({ message: "" }, { host: c.host, wasLive: true }) }
      );
    case "status":
      return edit(s, a.sessionId, (c) => ({ ...c, status: a.status }));
    case "step":
      return edit(s, a.sessionId, (c) => ({ ...c, host: a.host ?? c.host, step: a.step, status: a.step ? "error" : "connecting" }));
    case "dialed": {
      // The tab takes the backend's session ID; its views mount now.
      const { [a.from]: view, ...views } = s.views;
      return {
        ...s,
        sessions: s.sessions.map((c) =>
          c.sessionId === a.from
            ? { ...c, sessionId: a.sessionId, sshless: a.sshless, status: "live", dialing: false, step: undefined }
            : c
        ),
        activeId: s.activeId === a.from ? a.sessionId : s.activeId,
        views: { ...views, [a.sessionId]: view },
        layouts: s.layouts.map((l) =>
          leaves(l.tree).includes(a.from) ? { ...l, tree: rename(l.tree, a.from, a.sessionId), focus: l.focus === a.from ? a.sessionId : l.focus } : l
        ),
      };
    }
    case "redial":
      return edit(s, a.sessionId, (c) => ({ ...c, dialing: true, status: "connecting", step: undefined }));
    case "settings":
      return {
        ...s,
        settings: a.section,
        activeId: a.section ? SETTINGS_TAB : s.activeId === SETTINGS_TAB ? s.sessions[s.sessions.length - 1]?.sessionId ?? null : s.activeId,
      };
    case "close": {
      const at = s.sessions.findIndex((c) => c.sessionId === a.sessionId);
      if (at < 0) return s;
      const sessions = s.sessions.filter((c) => c.sessionId !== a.sessionId);
      const { [a.sessionId]: _drop, ...views } = s.views;
      // A closed pane's tab keeps the focus on another of its panes; a
      // closed tab's right neighbour takes its place, else its left one.
      const l = layoutOf(s, a.sessionId);
      const panes = l ? leaves(l.tree).filter((id) => id !== a.sessionId) : [];
      const activeId =
        s.activeId === a.sessionId ? panes[0] ?? (sessions[at] ?? sessions[at - 1])?.sessionId ?? null : s.activeId;
      return { ...s, sessions, activeId, views, layouts: without(s.layouts, [a.sessionId]) };
    }
    case "activate":
      // A split tab's ID activates its focused pane.
      return { ...s, activeId: s.layouts.find((l) => l.id === a.sessionId)?.focus ?? a.sessionId };
    case "view":
      return { ...s, views: { ...s.views, [a.sessionId]: a.view } };
    case "move": {
      // A split tab moves with all its sessions, to `before`'s place.
      const tabs = tabsOf(s);
      const from = tabs.find((t) => t.id === a.from)?.sessions;
      const before = tabs.find((t) => t.id === a.before)?.sessions;
      if (!from || !before || from === before) return s;
      const at = (ids: string[], arr: Connection[]) => arr.findIndex((c) => ids.includes(c.sessionId));
      const right = at(from, s.sessions) < at(before, s.sessions);
      const rest = s.sessions.filter((c) => !from.includes(c.sessionId));
      const i = right ? rest.reduce((end, c, k) => (before.includes(c.sessionId) ? k + 1 : end), 0) : at(before, rest);
      rest.splice(i, 0, ...s.sessions.filter((c) => from.includes(c.sessionId)));
      return { ...s, sessions: rest };
    }
    case "split": {
      const open = (id: string) => s.sessions.some((c) => c.sessionId === id);
      if (a.from === a.to || !open(a.from) || !open(a.to)) return s;
      const dst = layoutOf(s, a.to);
      const layouts = without(s.layouts.filter((l) => l !== dst), [a.from]);
      const tree = split(dst?.tree ?? a.to, a.to, a.from, a.side);
      return { ...s, activeId: a.from, layouts: [...layouts, { id: dst?.id ?? `panes-${crypto.randomUUID()}`, ...dst, tree, focus: a.from }] };
    }
    case "unsplit": {
      const layouts = without(s.layouts, [a.sessionId]);
      return layouts === s.layouts ? s : { ...s, layouts };
    }
    case "resize": {
      const l = s.layouts.find((x) => x.id === a.layoutId);
      const tree = l && resize(l.tree, a.path, a.index, a.delta);
      return !l || tree === l.tree ? s : { ...s, layouts: s.layouts.map((x) => (x === l ? { ...l, tree: tree! } : x)) };
    }
    case "layout": {
      const panes = leaves(a.tree);
      const id = `panes-${crypto.randomUUID()}`;
      return { ...s, activeId: panes[0], layouts: [...without(s.layouts, panes), { id, tree: a.tree, focus: panes[0], name: a.name }] };
    }
  }
}

/** What the session provider offers the shell (src/state/SessionsProvider.tsx). */
export type Sessions = SessionsState & {
  active: Connection | null;
  statusByHost: StatusMap;
  connect: (h: Host) => Promise<void>;
  /** Opens an SSH host's X11 desktop in a tab of its own, or focuses it. */
  openX11Desktop: (h: Host) => Promise<void>;
  disconnect: (sessionId: string) => void;
  activate: (sessionId: string | null) => void;
  setView: (sessionId: string, view: TabKey) => void;
  move: (from: string, before: string) => void;
  openLocalShell: (shell: api.LocalShell) => void;
  openSerial: () => void;
  /** Opens the Files tab (this computer's disk in two panes), or focuses it. */
  openFiles: () => void;
  /** Opens the Containers tab for an engine on this computer, or focuses it. */
  openContainers: () => Promise<void>;
  /** Opens a terminal tab on a container of a session's engine: a shell, or
   *  with follow its logs. */
  openContainerTerm: (engineSession: string, containerId: string, name: string, follow: boolean) => void;
  /** Opens a terminal tab running command in a pod's container (R57). */
  openPodTerm: (clusterId: string, namespace: string, pod: string, container: string, command: string[]) => void;
  /** Runs a dialing tab's connect again. */
  retry: (sessionId: string) => void;
  /** Rebuilds a dropped session's connection (the banner's Reconnect); a
   *  VNC or X11 desktop tab dials again in place. */
  reconnect: (sessionId: string) => Promise<void>;
  wakeHost: (h: Host) => Promise<void>;
  /** An RDP-only, VNC, SPICE or X11 desktop tab's real state, from RdpTab,
   *  VncTab or SpiceTab. */
  setStatus: (sessionId: string, status: ConnStatus) => void;
  /** Opens the Settings tab at a category; null closes it. */
  showSettings: (section: SettingsSection | null) => void;
  /** Puts session `from` in a pane beside session `to`'s (R65). */
  split: (from: string, to: string, side: Side) => void;
  /** Moves a pane's session out to a tab of its own. */
  unsplit: (sessionId: string) => void;
  resizePanes: (layoutId: string, path: number[], index: number, delta: number) => void;
  /** Opens a saved workspace as one split tab (KTD40). */
  openWorkspace: (ws: api.Workspace) => Promise<void>;
  /** A connect prompt's Cancel: a pane stays on Retry, a plain tab closes. */
  cancelPrompt: (sessionId: string) => void;
};

export const SessionsCtx = createContext<Sessions | null>(null);
export const useSessions = () => useContext(SessionsCtx)!;
