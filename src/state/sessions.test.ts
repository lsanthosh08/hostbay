import { describe, expect, it } from "vitest";
import type { Host } from "../api";
import {
  activeTab,
  initialSessions,
  neverConnected,
  sessionAddress,
  sessionsReducer,
  SETTINGS_TAB,
  tabsOf,
  viewsFor,
  vpnTabNote,
  type Connection,
  type LocalTerm,
  type SessionsState,
} from "./sessions";

const host = (id: string): Host => ({ id, alias: id, hostname: `${id}.lan`, port: 22, user: "u", identityFile: "", tags: [] });
const conn = (id: string): Omit<Connection, "status"> => ({ sessionId: id, host: host(`h-${id}`) });
const open = (...ids: string[]) =>
  ids.reduce<SessionsState>((s, id) => sessionsReducer(s, { type: "open", conn: conn(id) }), initialSessions);

describe("sessions reducer", () => {
  it("opens a session as connecting, goes live on conn-state, and errors on a drop", () => {
    let s = open("a");
    expect(s.activeId).toBe("a");
    expect(s.sessions[0].status).toBe("connecting");
    s = sessionsReducer(s, { type: "connState", sessionId: "a", state: "live" });
    expect(s.sessions[0].status).toBe("live");
    s = sessionsReducer(s, { type: "connState", sessionId: "a", state: "dropped" });
    expect(s.sessions[0].status).toBe("error");
  });

  // R59: the Monitor view is for SSH hosts only (KTD29 samples over SSH).
  it("offers the Monitor view on an SSH host and not on an RDP one", () => {
    const s = open("a");
    expect(viewsFor(s.sessions[0])).toContain("monitor");
    expect(viewsFor({ ...s.sessions[0], host: { ...host("w"), kind: "rdp" } })).not.toContain("monitor");
  });

  it("keeps the same state when an update changes nothing", () => {
    const s = sessionsReducer(open("a"), { type: "connState", sessionId: "a", state: "live" });
    expect(sessionsReducer(s, { type: "connState", sessionId: "a", state: "live" })).toBe(s);
    expect(sessionsReducer(s, { type: "status", sessionId: "a", status: "live" })).toBe(s);
    expect(sessionsReducer(s, { type: "status", sessionId: "b", status: "error" })).toBe(s);
    expect(sessionsReducer(s, { type: "status", sessionId: "a", status: "error" })).not.toBe(s);
  });

  it("closing the active tab activates its right neighbour", () => {
    let s = sessionsReducer(open("a", "b", "c"), { type: "activate", sessionId: "b" });
    s = sessionsReducer(s, { type: "close", sessionId: "b" });
    expect(s.sessions.map((c) => c.sessionId)).toEqual(["a", "c"]);
    expect(s.activeId).toBe("c");
  });

  it("closing the active last tab activates the one on its left", () => {
    const s = sessionsReducer(open("a", "b", "c"), { type: "close", sessionId: "c" });
    expect(s.activeId).toBe("b");
  });

  it("closing an inactive tab keeps the active one", () => {
    const s = sessionsReducer(open("a", "b", "c"), { type: "close", sessionId: "a" });
    expect(s.activeId).toBe("c");
  });

  it("closing the only tab leaves no active session, so the Start page shows", () => {
    const s = sessionsReducer(open("a"), { type: "close", sessionId: "a" });
    expect(s.sessions).toEqual([]);
    expect(s.activeId).toBeNull();
    expect(s.views).toEqual({});
  });

  it("remembers each session's view and reorders tabs", () => {
    let s = sessionsReducer(open("a", "b"), { type: "view", sessionId: "a", view: "files" });
    expect(s.views.a).toBe("files");
    expect(s.views.b).toBe("dashboard");
    s = sessionsReducer(s, { type: "move", from: "b", before: "a" });
    expect(s.sessions.map((c) => c.sessionId)).toEqual(["b", "a"]);
  });

  // AE16: the tab opens at once and keeps a failure until the user acts.
  it("keeps a dialing tab's failure, then takes the backend's session ID once connected", () => {
    let s = sessionsReducer(initialSessions, { type: "open", conn: { ...conn("tab1"), dialing: true } });
    s = sessionsReducer(s, { type: "step", sessionId: "tab1", step: { kind: "failure", message: "refused" } });
    expect(s.sessions[0]).toMatchObject({ status: "error", dialing: true, step: { kind: "failure" } });
    s = sessionsReducer(s, { type: "step", sessionId: "tab1", host: { ...host("h-tab1"), hostname: "edited.lan" } });
    expect(s.sessions[0]).toMatchObject({ status: "connecting", step: undefined, host: { hostname: "edited.lan" } });
    s = sessionsReducer(s, { type: "view", sessionId: "tab1", view: "files" });
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "s1" });
    expect(s.sessions[0]).toMatchObject({ sessionId: "s1", status: "live", dialing: false });
    expect(s.activeId).toBe("s1");
    expect(s.views).toEqual({ s1: "files" });
  });

  // A tab whose first connect failed has nothing to disconnect; its tab's
  // close button closes it.
  it("knows a tab that never connected from one that dropped", () => {
    let s = sessionsReducer(initialSessions, { type: "open", conn: { ...conn("tab1"), dialing: true } });
    s = sessionsReducer(s, { type: "step", sessionId: "tab1", step: { kind: "failure", message: "refused" } });
    expect(neverConnected(s.sessions[0])).toBe(true);
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "s1" });
    s = sessionsReducer(s, { type: "connState", sessionId: "s1", state: "dropped" });
    expect(neverConnected(s.sessions[0])).toBe(false);
  });

  it("shows a drop after going live as the reconnect banner, cleared when live again", () => {
    let s = sessionsReducer(open("a"), { type: "connState", sessionId: "a", state: "dropped" });
    expect(s.sessions[0].step?.kind).toBe("reconnect");
    s = sessionsReducer(s, { type: "connState", sessionId: "a", state: "live" });
    expect(s.sessions[0].step).toBeUndefined();
  });

  it("opens Settings as its own tab and closing it returns to the last session", () => {
    let s = sessionsReducer(open("a"), { type: "settings", section: "backup" });
    expect(s).toMatchObject({ settings: "backup", activeId: SETTINGS_TAB });
    s = sessionsReducer(s, { type: "settings", section: null });
    expect(s).toMatchObject({ settings: null, activeId: "a" });
  });
  // A VNC session is one signed-in stream: reconnecting signs in again, so
  // the tab dials afresh and takes the new vncId.
  it("shows a VNC host as its desktop only, and redials it in place", () => {
    const vnc: Host = { ...host("mac"), kind: "vnc", user: "", port: 0 };
    let s = sessionsReducer(initialSessions, { type: "open", conn: { sessionId: "tab1", host: vnc, dialing: true } });
    expect(s.views).toEqual({ tab1: "vnc" });
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "v1", sshless: true });
    expect(sessionAddress(s.sessions[0])).toBe("mac.lan:5900");
    s = sessionsReducer(s, { type: "redial", sessionId: "v1" });
    expect(s.sessions[0]).toMatchObject({ sessionId: "v1", dialing: true, status: "connecting", step: undefined });
    s = sessionsReducer(s, { type: "dialed", from: "v1", sessionId: "v2", sshless: true });
    expect(s.sessions[0]).toMatchObject({ sessionId: "v2", dialing: false });
    expect(s.views).toEqual({ v2: "vnc" });
  });

  // R46: a SPICE host is its screen only, with no user, and its tab takes
  // the spiceId once signed in.
  it("shows a SPICE host as its screen only", () => {
    const spice: Host = { ...host("kvm"), kind: "spice", user: "", port: 0 };
    let s = sessionsReducer(initialSessions, { type: "open", conn: { sessionId: "tab1", host: spice, dialing: true } });
    expect(s.views).toEqual({ tab1: "vnc" });
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "s1", sshless: true });
    expect(s.views).toEqual({ s1: "vnc" });
    expect(sessionAddress(s.sessions[0])).toBe("kvm.lan:5900");
    expect(sessionAddress({ ...s.sessions[0], host: { ...spice, port: 5930 } })).toBe("kvm.lan:5930");
  });

  // R56: a database host is its database view only, and its tab takes the
  // databaseId once open.
  it("shows a database host as its database view only", () => {
    const pg: Host = { ...host("db"), kind: "postgres", user: "app", port: 0, database: "shop" };
    let s = sessionsReducer(initialSessions, { type: "open", conn: { sessionId: "tab1", host: pg, dialing: true } });
    expect(s.views).toEqual({ tab1: "database" });
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "d1", sshless: true });
    expect(viewsFor(s.sessions[0])).toEqual(["database"]);
    expect(sessionAddress(s.sessions[0])).toBe("app@db.lan:5432/shop");
    const lite: Host = { ...host("notes"), kind: "sqlite", hostname: "", user: "", port: 0, file: "/data/notes.db" };
    expect(sessionAddress({ ...s.sessions[0], host: lite })).toBe("/data/notes.db");
  });

  // R47: an SSH host's X11 desktop is its own tab, the remote screen only,
  // whose session ID once live is the vncId X11DesktopOpen returned.
  it("shows an SSH host's X11 desktop as the remote screen only, and redials it in place", () => {
    const web = host("web");
    let s = sessionsReducer(initialSessions, { type: "open", conn: { sessionId: "tab1", host: web, x11: true, dialing: true } });
    expect(s.views).toEqual({ tab1: "vnc" });
    expect(viewsFor(s.sessions[0])).toEqual(["vnc"]);
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "x1", sshless: true });
    expect(sessionAddress(s.sessions[0])).toBe("X11 desktop on u@web.lan:22");
    s = sessionsReducer(s, { type: "redial", sessionId: "x1" });
    s = sessionsReducer(s, { type: "dialed", from: "x1", sessionId: "x2", sshless: true });
    expect(s.sessions[0]).toMatchObject({ sessionId: "x2", x11: true, dialing: false });
    expect(s.views).toEqual({ x2: "vnc" });
  });
});

describe("Kubernetes sessions", () => {
  // R57: a cluster is its Kubernetes view only, and a pod shell is a
  // terminal tab of its own.
  it("shows a cluster as its Kubernetes view only", () => {
    const k: Host = { ...host("prod"), kind: "kubernetes", user: "", port: 6443, url: "https://k8s.lan:6443" };
    let s = sessionsReducer(initialSessions, { type: "open", conn: { sessionId: "tab1", host: k, dialing: true } });
    expect(s.views).toEqual({ tab1: "kubernetes" });
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "c1", sshless: true });
    expect(viewsFor(s.sessions[0])).toEqual(["kubernetes"]);
    expect(sessionAddress(s.sessions[0])).toBe("https://k8s.lan:6443");
    const pod = { ...s.sessions[0], term: { kind: "pod", clusterId: "c1", namespace: "apps", pod: "web-1", container: "web", command: ["sh"] } as LocalTerm };
    expect(viewsFor(pod)).toEqual(["terminal"]);
  });
});

describe("split panes", () => {
  const ids = (s: SessionsState) => tabsOf(s).map((t) => t.id);
  const paneTab = (s: SessionsState) => s.layouts[0]?.id;

  it("moves a tab into a pane beside another, keeping its session, and shows one tab", () => {
    let s = open("a", "b", "c");
    s = sessionsReducer(s, { type: "split", from: "c", to: "a", side: "right" });
    expect(s.sessions.map((c) => c.sessionId)).toEqual(["a", "b", "c"]);
    expect(s.layouts[0].tree).toEqual({ dir: "row", sizes: [0.5, 0.5], children: ["a", "c"] });
    expect(ids(s)).toEqual([paneTab(s), "b"]);
    expect(s.activeId).toBe("c");
    expect(activeTab(s)).toBe(paneTab(s));
  });

  it("changes nothing when a session is dropped onto the pane it already fills", () => {
    const s = sessionsReducer(open("a", "b"), { type: "split", from: "b", to: "a", side: "right" });
    expect(sessionsReducer(s, { type: "split", from: "a", to: "a", side: "left" })).toBe(s);
  });

  it("focuses panes, and the tab remembers its focused pane", () => {
    let s = sessionsReducer(open("a", "b", "c"), { type: "split", from: "b", to: "a", side: "bottom" });
    s = sessionsReducer(s, { type: "activate", sessionId: "a" });
    expect(s.layouts[0].focus).toBe("a");
    s = sessionsReducer(s, { type: "activate", sessionId: "c" });
    s = sessionsReducer(s, { type: "activate", sessionId: paneTab(s) });
    expect(s.activeId).toBe("a");
  });

  it("closes a pane when its session ends, collapsing to a plain tab with the other pane focused", () => {
    let s = sessionsReducer(open("a", "b"), { type: "split", from: "b", to: "a", side: "right" });
    s = sessionsReducer(s, { type: "close", sessionId: "b" });
    expect(s.layouts).toEqual([]);
    expect(ids(s)).toEqual(["a"]);
    expect(s.activeId).toBe("a");
    s = sessionsReducer(s, { type: "close", sessionId: "a" });
    expect(s).toMatchObject({ sessions: [], layouts: [], activeId: null });
  });

  it("follows a pane's session to the backend's ID once it connects", () => {
    let s = sessionsReducer(open("a"), { type: "open", conn: { ...conn("tab1"), dialing: true } });
    s = sessionsReducer(s, { type: "split", from: "tab1", to: "a", side: "right" });
    s = sessionsReducer(s, { type: "dialed", from: "tab1", sessionId: "s1" });
    expect(s.layouts[0]).toMatchObject({ tree: { children: ["a", "s1"] }, focus: "s1" });
    expect(s.activeId).toBe("s1");
  });

  it("moves a pane out to its own tab, and resizes panes", () => {
    let s = sessionsReducer(open("a", "b", "c"), { type: "split", from: "b", to: "a", side: "right" });
    s = sessionsReducer(s, { type: "split", from: "c", to: "b", side: "right" });
    s = sessionsReducer(s, { type: "resize", layoutId: paneTab(s), path: [], index: 0, delta: 0.1 });
    expect((s.layouts[0].tree as { sizes: number[] }).sizes[0]).toBeCloseTo(0.6);
    s = sessionsReducer(s, { type: "unsplit", sessionId: "c" });
    expect(ids(s)).toEqual([paneTab(s), "c"]);
    expect(s.layouts[0].tree).toMatchObject({ children: ["a", "b"] });
  });

  it("reorders a split tab with all its panes", () => {
    let s = sessionsReducer(open("a", "b", "c"), { type: "split", from: "c", to: "b", side: "right" });
    s = sessionsReducer(s, { type: "move", from: paneTab(s), before: "a" });
    expect(s.sessions.map((c) => c.sessionId)).toEqual(["b", "c", "a"]);
    s = sessionsReducer(s, { type: "move", from: paneTab(s), before: "a" });
    expect(s.sessions.map((c) => c.sessionId)).toEqual(["a", "b", "c"]);
  });

  it("lays out opened sessions as a workspace tab", () => {
    let s = open("a", "b", "c");
    s = sessionsReducer(s, { type: "layout", tree: { dir: "col", sizes: [0.3, 0.7], children: ["b", "c"] }, name: "prod" });
    expect(s.layouts[0]).toMatchObject({ name: "prod", focus: "b" });
    expect(ids(s)).toEqual(["a", paneTab(s)]);
    expect(s.activeId).toBe("b");
  });
});

// F1: a tab opening a host behind a down profile says so, then names it.
describe("VPN tab note", () => {
  const vpns = [{ id: "v1", name: "Atlas VPN", type: "wireguard", tier: "app" }];
  const behind = (over: Partial<Connection>): Connection => ({ sessionId: "s", host: { ...host("h"), vpnId: "v1" }, status: "connecting", ...over });

  it("shows Connecting VPN P while the profile comes up, then via P once live", () => {
    const dialing = behind({ dialing: true });
    expect(vpnTabNote(dialing, vpns, { v1: { id: "v1", status: "connecting" } })).toEqual({ kind: "pending", name: "Atlas VPN", state: { id: "v1", status: "connecting" } });
    expect(vpnTabNote(dialing, vpns, { v1: { id: "v1", status: "signing_in", need: "otp" } })?.kind).toBe("pending");
    // The VPN is up and the host is being dialled: the usual connecting view.
    expect(vpnTabNote(dialing, vpns, { v1: { id: "v1", status: "connected" } })).toBeUndefined();
    expect(vpnTabNote(behind({ status: "live" }), vpns, { v1: { id: "v1", status: "connected" } })).toEqual({ kind: "via", name: "Atlas VPN" });
  });

  it("says nothing for a host without a profile, or a step to answer", () => {
    expect(vpnTabNote({ ...behind({ status: "live" }), host: host("h") }, vpns, {})).toBeUndefined();
    expect(vpnTabNote(behind({ dialing: true, step: { kind: "failure", message: "x" } }), vpns, { v1: { id: "v1", status: "connecting" } })).toBeUndefined();
    expect(vpnTabNote(behind({ status: "live" }), [], {})).toBeUndefined();
  });
});
