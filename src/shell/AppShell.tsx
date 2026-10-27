import { useEffect, useRef, useState } from "react";
import * as api from "../api";
import { bound, type AppCommands, type PanelId } from "../actions/registry";
import { findAction, platform } from "../actions/shortcuts";
import { AISidebar } from "../components/ai/AISidebar";
import { APPROVAL_TRAY_ID, ApprovalTray } from "../components/ai/ApprovalTray";
import { followTarget } from "../components/ai/follow";
import { activeTerminal, onActiveTerminal } from "../tabs/terminal/active";
import { useHostManager } from "../components/HostManager";
import { TransferPanel } from "../components/TransferPanel";
import { TunnelsPanel } from "../components/TunnelsPanel";
import { HistoryPanel } from "../components/HistoryPanel";
import { QuickCommands } from "../components/QuickCommands";
import { useHosts } from "../state/hosts";
import { t } from "../i18n";
import { activeTab, layoutOf, SETTINGS_TAB, tabsOf, useSessions } from "../state/sessions";
import { useTransferActions, useTransfersPanel } from "../state/transfers";
import { usePrompt } from "../ui/prompt";
import { useTheme } from "../ui/theme";
import { useToast } from "../ui/toast";
import { leaves } from "../workspace/layout";
import { toSaved } from "../workspace/saved";
import { WorkspaceView } from "../workspace/WorkspaceView";
import { ActivityRail } from "./ActivityRail";
import { CommandPalette } from "./CommandPalette";
import { HostsPanel } from "./HostsPanel";
import { SessionLayer } from "./SessionLayer";
import { SessionTabBar } from "./SessionTabBar";
import { SessionToolbar } from "./SessionToolbar";
import { SettingsView } from "./SettingsView";
import { SidePanel } from "./SidePanel";
import { createSlots } from "./slots";
import { StartPage } from "./StartPage";
import { StatusBar, useSyncState } from "./StatusBar";

/** Keys typed here belong to the terminal or the field, not to shortcuts
 *  (xterm and Monaco both take keys through a textarea; a focused canvas is
 *  a remote screen, RDP or VNC, that takes every key). */
const isTyping = (el: EventTarget | null) =>
  el instanceof HTMLElement && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT|CANVAS)$/.test(el.tagName));

/** The window: activity rail, side panel, session tabs with their toolbar
 *  and body (or the Start page), and the status bar (KTD16). */
export function AppShell() {
  const sessions = useSessions();
  const { active } = sessions;
  const manager = useHostManager();
  const { toggle: toggleTheme } = useTheme();
  const { push } = useToast();
  const { prompt } = usePrompt();
  const { workspaces, reloadWorkspaces } = useHosts();
  const { setOpen: setTransfersOpen } = useTransferActions();
  const { open: transfersOpen } = useTransfersPanel();
  const [panel, setPanel] = useState<PanelId>("hosts");
  const [panelOpen, setPanelOpen] = useState(true);
  const [palette, setPalette] = useState(false);
  // Mounted on first open and kept, so a conversation goes on while hidden.
  const [ai, setAI] = useState<"never" | "open" | "hidden">("never");
  // Sync's state is read again whenever the Settings category changes.
  const sync = useSyncState(sessions.settings);
  const [slots] = useState(() => createSlots(() => document.createElement("div")));

  const tabs = tabsOf(sessions);
  const tabAt = tabs.findIndex((x) => x.id === activeTab(sessions));
  const goTo = (i: number) => {
    if (tabs[i]) sessions.activate(tabs[i].id);
  };
  const layout = layoutOf(sessions, sessions.activeId);
  // The next plain tab after the active one, round the strip.
  const nextTab = active ? [...tabs.slice(tabAt + 1), ...tabs.slice(0, tabAt)].find((x) => x.plain) : undefined;
  // A session's saved host, if it is one a workspace can reopen.
  const hostOf = (id: string) => {
    const c = sessions.sessions.find((x) => x.sessionId === id);
    return c && !c.term && !c.files && !c.engine && !c.x11 ? c.host.id : undefined;
  };
  const saveWorkspace = async () => {
    const tree = layout?.tree ?? active?.sessionId;
    const saved = tree && toSaved(tree, hostOf);
    if (!saved) return push("error", t("workspace.nothing"));
    const name = await prompt(t("workspace.name"), layout?.name ?? active?.host.alias ?? "");
    if (!name?.trim()) return;
    const same = workspaces.find((w) => w.name === name.trim());
    try {
      await api.saveWorkspace({ id: same?.id ?? "", name, layout: saved });
      reloadWorkspaces();
      push("success", t("workspace.saved", { name: name.trim() }));
    } catch (e) {
      push("error", api.errorInfo(e).message);
    }
  };
  const app: AppCommands = {
    hasSession: !!active,
    canEditHost: !!active && !active.term && !active.files && !active.engine,
    needsMasterPassword: manager.needsMasterPassword,
    openPalette: () => setPalette(true),
    showPanel: (p) => {
      setPanel(p);
      setPanelOpen(true);
    },
    toggleSidePanel: () => setPanelOpen((o) => !o),
    showStart: () => sessions.activate(null),
    closeTab: () => active && sessions.disconnect(active.sessionId),
    cycleTab: (step) => {
      const n = tabs.length;
      if (n) goTo(tabAt < 0 ? 0 : (tabAt + step + n) % n);
    },
    goToTab: (n) => goTo(n - 1),
    editActiveHost: () => active && !active.term && !active.files && !active.engine && manager.editHost(active.host),
    newHost: () => manager.newHost(),
    newFolder: () => manager.addFolder(null),
    importSshConfig: () => manager.importSshConfig(),
    importSshManager: () => manager.importSshManager(),
    restore: () => manager.restore(),
    // The palette lists every shell; this opens the first, the system's own.
    openLocalShell: () =>
      api
        .listLocalShells()
        .then((s) => (s[0] ? sessions.openLocalShell(s[0]) : push("error", "No local shell found")))
        .catch((e) => push("error", api.errorInfo(e).message)),
    openSerial: sessions.openSerial,
    openFiles: sessions.openFiles,
    showTransfers: () => setTransfersOpen(true),
    openSettings: (section) => sessions.showSettings(section),
    toggleTheme,
    hasPanes: !!layout,
    canSplit: !!nextTab,
    splitWithNext: (side) => nextTab && active && sessions.split(nextTab.id, active.sessionId, side),
    focusPane: (step) => {
      const panes = layout ? leaves(layout.tree) : [];
      const at = panes.indexOf(sessions.activeId ?? "");
      if (panes.length) sessions.activate(panes[(at + step + panes.length) % panes.length]);
    },
    unsplit: () => active && sessions.unsplit(active.sessionId),
    saveWorkspace,
    toggleAISidebar: () => setAI((s) => (s === "open" ? "hidden" : "open")),
    focusApprovals: () => document.getElementById(APPROVAL_TRAY_ID)?.focus(),
  };

  // A follow-active AI conversation acts on the focused pane's terminal.
  const activeId = sessions.activeId;
  useEffect(() => {
    const report = () => api.aiSetActiveTerminal(followTarget(activeId, activeTerminal())).catch(() => {});
    report();
    return onActiveTerminal(report);
  }, [activeId]);

  // One capture-phase listener runs shortcuts before xterm or a field sees
  // the key; only global ones fire while typing (src/actions/shortcuts.ts).
  const live = useRef(app);
  live.current = app;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.isComposing) return;
      const a = findAction(
        bound().filter((x) => !x.hidden && (!x.available || x.available(live.current))),
        e,
        platform,
        isTyping(e.target)
      );
      if (!a) return;
      e.preventDefault();
      e.stopPropagation();
      a.run(live.current);
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, []);

  return (
    <div className="h-full flex flex-col">
      <div className="flex-1 min-h-0 flex">
        <ActivityRail app={app} panel={panelOpen ? panel : null} sync={sync} />
        {panelOpen && (
          <SidePanel>
            {panel === "tunnels" ? (
              <TunnelsPanel />
            ) : panel === "quickCommands" ? (
              <QuickCommands />
            ) : panel === "history" ? (
              <HistoryPanel />
            ) : (
              <HostsPanel />
            )}
          </SidePanel>
        )}
        <main className="flex-1 min-w-0 flex flex-col">
          <SessionTabBar onStart={app.showStart} onPalette={app.openPalette} />
          <SessionToolbar onEditHost={app.editActiveHost} />
          <WorkspaceView slots={slots} className={active ? "flex-1 min-h-0 flex flex-col" : "hidden"} />
          {sessions.activeId === SETTINGS_TAB && sessions.settings ? (
            <SettingsView section={sessions.settings} onSection={sessions.showSettings} />
          ) : (
            !active && <StartPage app={app} />
          )}
          {transfersOpen && <TransferPanel />}
        </main>
        {ai !== "never" && <AISidebar hidden={ai === "hidden"} onClose={() => setAI("hidden")} onSettings={() => app.openSettings("ai")} />}
      </div>
      <StatusBar sync={sync} onTunnels={() => app.showPanel("tunnels")} onSync={() => app.openSettings("sync")} />
      <SessionLayer slots={slots} />
      <ApprovalTray />
      {palette && <CommandPalette app={app} onClose={() => setPalette(false)} />}
    </div>
  );
}
