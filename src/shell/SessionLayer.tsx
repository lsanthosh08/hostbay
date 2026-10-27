import { useEffect } from "react";
import { createPortal } from "react-dom";
import { ConnectionStatusView, ReconnectBanner } from "../connect/ConnectionStatusView";
import { t } from "../i18n";
import { ToastScope } from "../ui/toast";
import { layoutOf, terminalOnly, useSessions, type Connection, type TabKey } from "../state/sessions";
import {
  DashboardIcon,
  DatabaseIcon,
  DockerIcon,
  FilesIcon,
  IisIcon,
  KubernetesIcon,
  MonitorIcon,
  RemoteDesktopIcon,
  ServicesIcon,
  TerminalIcon,
  VncIcon,
  type Icon,
} from "../ui/icons";
import { DashboardTab } from "../tabs/DashboardTab";
import { DockerTab } from "../tabs/DockerTab";
import { FilesTab } from "../tabs/FilesTab";
import { FilesView, localPane } from "../tabs/files/FilesView";
import { isDatabaseHost, isFileHost } from "../api";
import { DatabaseTab } from "../tabs/database/DatabaseTab";
import { KubernetesTab } from "../tabs/kubernetes/KubernetesTab";
import { IisTab } from "../tabs/IisTab";
import { MonitorTab } from "../tabs/MonitorTab";
import { RdpTab } from "../tabs/RdpTab";
import { ServicesTab } from "../tabs/ServicesTab";
import { SpiceTab } from "../tabs/SpiceTab";
import { TerminalTab } from "../tabs/TerminalTab";
import { VncTab } from "../tabs/VncTab";
import { WinDashboardTab } from "../tabs/WinDashboardTab";
import type { createSlots } from "./slots";

export type Slots = ReturnType<typeof createSlots<HTMLDivElement>>;

/** Each view's name and icon, for the toolbar's switcher and the palette. */
export const VIEW_INFO: Record<TabKey, { label: string; icon: Icon }> = {
  dashboard: { label: t("view.dashboard"), icon: DashboardIcon },
  terminal: { label: t("view.terminal"), icon: TerminalIcon },
  files: { label: t("view.files"), icon: FilesIcon },
  docker: { label: t("view.docker"), icon: DockerIcon },
  services: { label: t("view.services"), icon: ServicesIcon },
  monitor: { label: t("view.monitor"), icon: MonitorIcon },
  iis: { label: t("view.iis"), icon: IisIcon },
  rdp: { label: t("view.rdp"), icon: RemoteDesktopIcon },
  vnc: { label: t("view.vnc"), icon: VncIcon },
  database: { label: t("view.database"), icon: DatabaseIcon },
  kubernetes: { label: t("view.kubernetes"), icon: KubernetesIcon },
};

/** One session's views. Every view stays mounted and only the chosen one
 *  shows, so a terminal or a file listing keeps its state across switches.
 *  Until the session first goes live the connect's status fills the tab;
 *  a later drop shows a banner over the views, which stay mounted. */
function SessionView({ c, view }: { c: Connection; view: TabKey }) {
  if (c.dialing) return <ConnectionStatusView c={c} />;
  return (
    <>
      {c.step?.kind === "reconnect" && <ReconnectBanner c={c} />}
      <SessionViews c={c} view={view} />
    </>
  );
}

function SessionViews({ c, view }: { c: Connection; view: TabKey }) {
  const sessions = useSessions();
  const { setStatus, activeId } = sessions;
  const tab = layoutOf(sessions, activeId);
  const shown = activeId === c.sessionId || (!!tab && tab === layoutOf(sessions, c.sessionId));
  const pane = (k: TabKey) => (view === k ? "flex-1 min-h-0" : "hidden");
  if (c.term || terminalOnly(c.host)) return <TerminalTab conn={c} />;
  // The Files tab, and a file host's session: the two-pane view only.
  if (c.files) return <FilesView left={localPane()} />;
  if (c.engine) return <DockerTab conn={c} />;
  if (isFileHost(c.host)) return <FilesView left={{ source: c.sessionId, name: c.host.alias }} fixedLeft />;
  // A database host's session ID is the databaseId databaseConnect returned.
  if (isDatabaseHost(c.host)) return <DatabaseTab conn={c} />;
  // A cluster's session ID is the clusterId kubernetesConnect returned.
  if (c.host.kind === "kubernetes") return <KubernetesTab conn={c} />;
  // A VNC host's session ID is the signed-in vncId its screen streams, and
  // an X11 desktop's the vncId of the Xvnc it started, sized to the tab.
  // A SPICE host's is the spiceId its channels stream.
  if (c.host.kind === "spice") return <SpiceTab conn={c} onStatus={(s) => setStatus(c.sessionId, s)} />;
  if (c.host.kind === "vnc" || c.x11) return <VncTab conn={c} resize={c.x11} onStatus={(s) => setStatus(c.sessionId, s)} />;
  if (c.host.kind === "rdp") {
    // Windows workspace: Dashboard/Files/IIS/RDP over SSH. When sshless
    // (no SSH server), render ONLY the RDP tab — the others would fire
    // SSH queries and error with "session not found".
    return (
      <>
        {!c.sshless && (
          <>
            <div className={pane("dashboard")}>
              <WinDashboardTab conn={c} />
            </div>
            <div className={pane("files")}>
              <FilesTab conn={c} windows />
            </div>
            <div className={pane("iis")}>
              <IisTab conn={c} />
            </div>
          </>
        )}
        <div className={pane("rdp")}>
          {/* An RDP-only tab has no SSH connection, so its state is RDP's. */}
          <RdpTab host={c.host} onStatus={c.sshless ? (s) => setStatus(c.sessionId, s) : undefined} />
        </div>
      </>
    );
  }
  return (
    <>
      <div className={pane("dashboard")}>
        <DashboardTab conn={c} />
      </div>
      <div className={pane("terminal")}>
        <TerminalTab conn={c} />
      </div>
      <div className={pane("files")}>
        <FilesTab conn={c} />
      </div>
      <div className={pane("docker")}>
        <DockerTab conn={c} />
      </div>
      <div className={pane("services")}>
        <ServicesTab conn={c} />
      </div>
      <div className={pane("monitor")}>
        {/* Hidden views stay mounted (KTD16); the sampler pauses while hidden. */}
        <MonitorTab conn={c} visible={shown && view === "monitor"} />
      </div>
    </>
  );
}

/** Renders every session once, keyed by ID, into its own portal target
 *  (KTD16). It sits at the shell's root, so no layout change unmounts it:
 *  a PTY or RDP session closes when its component unmounts. */
export function SessionLayer({ slots }: { slots: Slots }) {
  const { sessions, views } = useSessions();
  // Forget the targets of closed sessions once their portals are gone.
  useEffect(() => {
    const open = new Set(sessions.map((c) => c.sessionId));
    slots.prune(slots.ids().filter((id) => !open.has(id)));
  }, [sessions, slots]);
  return (
    <>
      {sessions.map((c) =>
        createPortal(
          <ToastScope name={c.host.alias}>
            <SessionView c={c} view={views[c.sessionId]} />
          </ToastScope>,
          slots.slot(c.sessionId),
          c.sessionId
        )
      )}
    </>
  );
}
