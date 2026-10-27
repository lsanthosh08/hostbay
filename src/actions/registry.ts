import type { MessageKey } from "../i18n";
import { SECTION_LABEL, SETTINGS_SECTIONS, type SettingsSection } from "../state/sessions";
import {
  AddIcon,
  AIIcon,
  ArchiveIcon,
  CommandIcon,
  DeleteIcon,
  DownloadIcon,
  EditIcon,
  FilesIcon,
  HistoryIcon,
  HostsIcon,
  ImportIcon,
  KeyIcon,
  NewFolderIcon,
  QuickCommandsIcon,
  SerialIcon,
  SettingsIcon,
  SidePanelIcon,
  SplitDownIcon,
  SplitIcon,
  StartPageIcon,
  SyncIcon,
  TerminalIcon,
  ThemeIcon,
  TransfersIcon,
  TunnelIcon,
  UnplugIcon,
  WorkspaceIcon,
  type Icon,
} from "../ui/icons";
import { settings } from "../state/settings";
import { formatShortcut, overridden, platform, type Shortcut } from "./shortcuts";

export type PanelId = "hosts" | "tunnels" | "quickCommands" | "history";

/** What actions act on; the shell implements it (src/shell/AppShell.tsx). */
export type AppCommands = {
  hasSession: boolean;
  /** The active session is a saved host. */
  canEditHost: boolean;
  needsMasterPassword: boolean;
  openPalette(): void;
  showPanel(p: PanelId): void;
  toggleSidePanel(): void;
  showStart(): void;
  closeTab(): void;
  cycleTab(step: 1 | -1): void;
  goToTab(n: number): void;
  editActiveHost(): void;
  newHost(): void;
  newFolder(): void;
  importSshConfig(): void;
  importSshManager(): void;
  restore(): void;
  openLocalShell(): void;
  openSerial(): void;
  /** Opens the Files tab, this computer's disk in two panes. */
  openFiles(): void;
  showTransfers(): void;
  /** Opens the Settings tab at a category. */
  openSettings(section: SettingsSection): void;
  toggleTheme(): void;
  /** The active tab is split into panes. */
  hasPanes: boolean;
  /** A tab after the active one can move into a pane beside it. */
  canSplit: boolean;
  /** Moves the next tab into a pane beside the focused one (R65). */
  splitWithNext(side: "right" | "bottom"): void;
  focusPane(step: 1 | -1): void;
  /** Moves the focused pane out to a tab of its own. */
  unsplit(): void;
  saveWorkspace(): void;
  /** Shows or hides the AI sidebar. */
  toggleAISidebar(): void;
  /** Moves focus to the app-wide approval tray. */
  focusApprovals(): void;
};

export type Action = {
  id: string;
  label: MessageKey;
  vars?: Record<string, string | number>;
  icon?: Icon;
  /** Default binding; the user's own replaces it (bound, keysFor). */
  shortcut?: Shortcut;
  /** "settings" lists it with the settings categories in the palette. */
  group?: "settings";
  /** Where the activity rail shows it, if it does. */
  rail?: "top" | "bottom";
  /** Not shipped yet: kept out of the rail and the palette. */
  hidden?: boolean;
  /** Left out of the palette, which already lists what it does. */
  palette?: false;
  available?: (app: AppCommands) => boolean;
  run: (app: AppCommands) => void;
};

const tabActions: Action[] = Array.from({ length: 9 }, (_, i) => ({
  id: `tab.${i + 1}`,
  label: "action.goToTab",
  vars: { n: i + 1 },
  // Cmd never reaches the PTY on macOS; Ctrl+3…7 are terminal control keys.
  shortcut: { mac: `Meta+Digit${i + 1}`, other: `Ctrl+Digit${i + 1}`, global: "mac" },
  palette: false,
  run: (app) => app.goToTab(i + 1),
}));

/** Every user action. The rail, the palette and shortcuts all read this list. */
export const actions: Action[] = [
  {
    id: "palette.open",
    label: "action.palette",
    icon: CommandIcon,
    shortcut: { mac: "Meta+K", other: "Ctrl+Shift+P", global: true },
    palette: false,
    run: (app) => app.openPalette(),
  },
  { id: "panel.hosts", label: "nav.hosts", icon: HostsIcon, rail: "top", run: (app) => app.showPanel("hosts") },
  // A tab, not a side panel: the two-pane Files view with Local on the left.
  { id: "files.open", label: "nav.files", icon: FilesIcon, rail: "top", run: (app) => app.openFiles() },
  { id: "panel.tunnels", label: "nav.tunnels", icon: TunnelIcon, rail: "top", run: (app) => app.showPanel("tunnels") },
  { id: "ai.sidebar", label: "ai.toggle", icon: AIIcon, rail: "top", run: (app) => app.toggleAISidebar() },
  { id: "ai.approvals", label: "ai.focusTray", icon: AIIcon, run: (app) => app.focusApprovals() },
  {
    id: "panel.quickCommands",
    label: "nav.quickCommands",
    icon: QuickCommandsIcon,
    rail: "top",
    run: (app) => app.showPanel("quickCommands"),
  },
  { id: "panel.history", label: "nav.history", icon: HistoryIcon, rail: "top", run: (app) => app.showPanel("history") },
  {
    id: "panel.toggle",
    label: "action.toggleSidePanel",
    icon: SidePanelIcon,
    shortcut: { mac: "Meta+B", other: "Ctrl+Shift+B", global: true },
    run: (app) => app.toggleSidePanel(),
  },
  { id: "sync.open", label: "nav.sync", icon: SyncIcon, rail: "bottom", group: "settings", run: (app) => app.openSettings("sync") },
  { id: "theme.toggle", label: "action.toggleTheme", icon: ThemeIcon, rail: "bottom", group: "settings", run: (app) => app.toggleTheme() },
  { id: "start.show", label: "action.startPage", icon: StartPageIcon, run: (app) => app.showStart() },
  { id: "host.new", label: "action.newHost", icon: AddIcon, run: (app) => app.newHost() },
  { id: "folder.new", label: "folder.new", icon: NewFolderIcon, run: (app) => app.newFolder() },
  { id: "import.sshConfig", label: "action.importSshConfig", icon: ImportIcon, run: (app) => app.importSshConfig() },
  { id: "import.sshManager", label: "action.importSshManager", icon: ImportIcon, run: (app) => app.importSshManager() },
  {
    id: "settings.open",
    label: "settings.title",
    icon: SettingsIcon,
    rail: "bottom",
    shortcut: { mac: "Meta+Comma", other: "Ctrl+Comma", global: true },
    palette: false,
    run: (app) => app.openSettings("general"),
  },
  // One palette entry per category; Sync has its own entry above.
  ...SETTINGS_SECTIONS.filter((s) => s !== "sync").map<Action>((s) => ({
    id: `settings.${s}`,
    label: SECTION_LABEL[s],
    icon: SettingsIcon,
    group: "settings",
    run: (app) => app.openSettings(s),
  })),
  { id: "backup.export", label: "action.backup", icon: DownloadIcon, group: "settings", run: (app) => app.openSettings("backup") },
  { id: "backup.restore", label: "action.restore", icon: ArchiveIcon, group: "settings", run: (app) => app.restore() },
  { id: "trash.open", label: "action.trash", icon: DeleteIcon, group: "settings", run: (app) => app.openSettings("backup") },
  {
    id: "masterPassword.set",
    label: "action.setMasterPassword",
    icon: KeyIcon,
    group: "settings",
    available: (app) => app.needsMasterPassword,
    run: (app) => app.openSettings("security"),
  },
  { id: "transfers.show", label: "action.transfers", icon: TransfersIcon, run: (app) => app.showTransfers() },
  { id: "local.shell", label: "action.localShell", icon: TerminalIcon, run: (app) => app.openLocalShell() },
  { id: "serial.open", label: "action.serialPort", icon: SerialIcon, run: (app) => app.openSerial() },
  {
    id: "session.disconnect",
    label: "action.disconnect",
    icon: UnplugIcon,
    available: (app) => app.hasSession,
    run: (app) => app.closeTab(),
  },
  {
    id: "session.editHost",
    label: "action.editHost",
    icon: EditIcon,
    available: (app) => app.canEditHost,
    run: (app) => app.editActiveHost(),
  },
  {
    id: "tab.next",
    label: "action.nextTab",
    shortcut: { mac: "Ctrl+Tab", other: "Ctrl+Tab", global: true },
    available: (app) => app.hasSession,
    run: (app) => app.cycleTab(1),
  },
  {
    id: "tab.prev",
    label: "action.prevTab",
    shortcut: { mac: "Ctrl+Shift+Tab", other: "Ctrl+Shift+Tab", global: true },
    available: (app) => app.hasSession,
    run: (app) => app.cycleTab(-1),
  },
  ...tabActions,
  // Split panes (R65): a tab moves into a pane by keyboard too (R74).
  {
    id: "pane.splitRight",
    label: "action.splitRight",
    icon: SplitIcon,
    shortcut: { mac: "Meta+D", other: "Alt+Shift+Equal", global: true },
    available: (app) => app.canSplit,
    run: (app) => app.splitWithNext("right"),
  },
  {
    id: "pane.splitDown",
    label: "action.splitDown",
    icon: SplitDownIcon,
    shortcut: { mac: "Meta+Shift+D", other: "Alt+Shift+Minus", global: true },
    available: (app) => app.canSplit,
    run: (app) => app.splitWithNext("bottom"),
  },
  {
    id: "pane.next",
    label: "action.nextPane",
    shortcut: { mac: "Meta+BracketRight", other: "Alt+ArrowRight", global: true },
    available: (app) => app.hasPanes,
    run: (app) => app.focusPane(1),
  },
  {
    id: "pane.prev",
    label: "action.prevPane",
    shortcut: { mac: "Meta+BracketLeft", other: "Alt+ArrowLeft", global: true },
    available: (app) => app.hasPanes,
    run: (app) => app.focusPane(-1),
  },
  { id: "pane.unsplit", label: "action.unsplit", available: (app) => app.hasPanes, run: (app) => app.unsplit() },
  {
    id: "workspace.save",
    label: "action.saveWorkspace",
    icon: WorkspaceIcon,
    available: (app) => app.hasSession,
    run: (app) => app.saveWorkspace(),
  },
];

/** Every action with the user's bindings over its defaults (KTD40). */
export const bound = (): Action[] => actions.map((a) => ({ ...a, shortcut: overridden(a.shortcut, settings().keys?.[a.id]) }));

/** What the keybinding editor lists: every action that ships, so a new one
 *  is bindable without editor changes. */
export const editable = () => bound().filter((a) => !a.hidden);

/** An action's keys as this platform writes them, e.g. ⌘K. */
export function keysFor(id: string): string | undefined {
  const a = actions.find((x) => x.id === id);
  const b = a && overridden(a.shortcut, settings().keys?.[id])?.[platform];
  return b && formatShortcut(b);
}
