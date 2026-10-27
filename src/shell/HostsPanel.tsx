import { useState, type MouseEvent } from "react";
import * as api from "../api";
import type { Host } from "../api";
import { t } from "../i18n";
import { HostTree } from "../components/HostTree";
import { useHostManager } from "../components/HostManager";
import { recentHosts, useHosts } from "../state/hosts";
import { useSessions } from "../state/sessions";
import { useContextMenu, type MenuItem } from "../ui/contextmenu";
import { Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import {
  AddIcon,
  ArchiveIcon,
  CollapsedIcon,
  DeleteIcon,
  DownloadIcon,
  ExpandedIcon,
  ImportIcon,
  KeyIcon,
  MoreIcon,
  NewFolderIcon,
  SearchIcon,
  SerialIcon,
  TerminalIcon,
  WorkspaceIcon,
} from "../ui/icons";
import { usePrompt } from "../ui/prompt";
import { useToast } from "../ui/toast";
import { Select } from "../ui/Select";
import { StatusDot } from "../ui/StatusDot";
import { fuzzyRank } from "./fuzzy";
import { hostIcon } from "./kindIcon";
import { PanelHeader } from "./SidePanel";

type Kind = NonNullable<Host["kind"]>;
const KINDS: Record<Kind, string> = {
  ssh: t("hosts.kind.ssh"),
  rdp: t("hosts.kind.rdp"),
  vnc: t("hosts.kind.vnc"),
  spice: t("hosts.kind.spice"),
  telnet: t("hosts.kind.telnet"),
  mosh: t("hosts.kind.mosh"),
  tcp: t("hosts.kind.tcp"),
  ftp: t("hosts.kind.ftp"),
  smb: t("hosts.kind.smb"),
  webdav: t("hosts.kind.webdav"),
  s3: t("hosts.kind.s3"),
  postgres: t("hosts.kind.postgres"),
  sqlite: t("hosts.kind.sqlite"),
  kubernetes: t("hosts.kind.kubernetes"),
  redis: t("hosts.kind.redis"),
  mongodb: t("hosts.kind.mongodb"),
  elasticsearch: t("hosts.kind.elasticsearch"),
  mysql: t("hosts.kind.mysql"),
  mssql: t("hosts.kind.mssql"),
  oracle: t("hosts.kind.oracle"),
  rqlite: t("hosts.kind.rqlite"),
};

/** "+" menu items that open a local shell, one per shell this computer has. */
export async function localShellItems(open: (s: api.LocalShell) => void, onError: (msg: string) => void) {
  const shells = await api.listLocalShells().catch((err) => {
    onError(api.errorInfo(err).message);
    return [];
  });
  return shells.map((s): MenuItem => ({ label: s.name, icon: TerminalIcon, onClick: () => open(s) }));
}

/** The one place to find, organise and connect to hosts (R74): search with a
 *  kind filter, Recent, and the folder tree. Single click selects; Enter or
 *  double-click connects. */
export function HostsPanel() {
  const { hosts, clearRecent, workspaces, reloadWorkspaces } = useHosts();
  const manager = useHostManager();
  const { connect, disconnect, openX11Desktop, sessions, statusByHost, openLocalShell, openSerial, showSettings, openWorkspace } =
    useSessions();
  const { push } = useToast();
  const { confirm } = usePrompt();
  const { at, node: menuNode } = useContextMenu();
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState<Kind | "">("");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [recentOpen, setRecentOpen] = useState(true);
  const [workspacesOpen, setWorkspacesOpen] = useState(true);
  const deleteWorkspace = async (w: api.Workspace) => {
    if (!(await confirm(t("workspace.deleteConfirm", { name: w.name })))) return;
    await api.deleteWorkspace(w.id).catch((e) => push("error", api.errorInfo(e).message));
    reloadWorkspaces();
  };

  const filtering = !!query.trim() || !!kind;
  const results = filtering
    ? fuzzyRank(
        hosts.filter((h) => !kind || api.hostKind(h) === kind),
        query,
        (h) => `${h.alias} ${h.hostname}`
      )
    : undefined;
  const recent = recentHosts(hosts);
  const disconnectHost = (h: Host) => {
    const c = sessions.find((x) => x.host.id === h.id);
    if (c) disconnect(c.sessionId);
  };

  // A menu under the button that opened it. stopPropagation keeps this click
  // from reaching the window listener that closes the menu.
  const openMenu = (items: MenuItem[]) => (e: MouseEvent<HTMLElement>) => {
    e.stopPropagation();
    const r = e.currentTarget.getBoundingClientRect();
    at(items, r.left, r.bottom + 4);
  };
  const openAddMenu = async (e: MouseEvent<HTMLElement>) => {
    e.stopPropagation();
    const r = e.currentTarget.getBoundingClientRect();
    const shells = await localShellItems(openLocalShell, (msg) => push("error", msg));
    at(
      [
        { label: t("action.newHost"), icon: AddIcon, onClick: () => manager.newHost() },
        { label: t("folder.new"), icon: NewFolderIcon, onClick: () => manager.addFolder(null) },
        {
          label: t("hosts.import"),
          icon: ImportIcon,
          items: [
            { label: t("action.importSshConfig"), onClick: manager.importSshConfig },
            { label: t("action.importSshManager"), onClick: manager.importSshManager },
            { label: t("action.restore"), onClick: manager.restore },
          ],
        },
        { label: t("action.localShell"), icon: TerminalIcon, items: shells, disabled: shells.length === 0 },
        { label: t("action.serialPort"), icon: SerialIcon, onClick: openSerial },
      ],
      r.left,
      r.bottom + 4
    );
  };
  const moreItems: MenuItem[] = [
    { label: t("action.backup"), icon: DownloadIcon, onClick: () => showSettings("backup") },
    { label: t("action.restore"), icon: ArchiveIcon, onClick: manager.restore },
    { label: t("action.trash"), icon: DeleteIcon, onClick: () => showSettings("backup") },
    ...(manager.needsMasterPassword
      ? [{ label: t("action.setMasterPassword"), icon: KeyIcon, onClick: () => showSettings("security") }]
      : []),
  ];

  const tree = (flat?: Host[]) => (
    <HostTree
      flat={flat}
      selectedId={selectedId}
      statusByHost={statusByHost}
      onSelect={(h) => setSelectedId(h.id)}
      onConnect={connect}
      onDisconnect={disconnectHost}
      onOpenX11Desktop={openX11Desktop}
    />
  );

  return (
    <>
      <PanelHeader title={t("nav.hosts")}>
        <IconButton label={t("hosts.add")} icon={AddIcon} aria-haspopup="menu" onClick={openAddMenu} />
        <IconButton label={t("common.more")} icon={MoreIcon} aria-haspopup="menu" onClick={openMenu(moreItems)} />
      </PanelHeader>
      <div className="px-2 pb-2 flex gap-1.5">
        <div className="relative flex-1 min-w-0">
          <SearchIcon className="size-3.5 text-muted absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none" aria-hidden />
          <Input
            type="search"
            aria-label={t("hosts.search")}
            placeholder={t("hosts.search")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              // Down moves into the list; Enter connects the best match.
              if (e.key === "ArrowDown") {
                e.preventDefault();
                e.currentTarget.closest("aside")?.querySelector<HTMLElement>("[role=tree] [data-row]")?.focus();
              } else if (e.key === "Enter" && results?.[0]) connect(results[0]);
              else if (e.key === "Escape" && query) {
                e.preventDefault();
                setQuery("");
              }
            }}
            className="w-full !h-8 pl-8"
          />
        </div>
        <Select
          compact
          aria-label={t("hosts.kindFilter")}
          value={kind}
          onChange={(e) => setKind(e.target.value as Kind | "")}
          className="!h-8 w-20 shrink-0"
        >
          <option value="">{t("hosts.kind.all")}</option>
          {Object.entries(KINDS).map(([k, label]) => (
            <option key={k} value={k}>
              {label}
            </option>
          ))}
        </Select>
      </div>

      <div className="flex-1 min-h-0 overflow-auto px-2 pb-2">
        {results ? (
          tree(results)
        ) : (
          <>
            {recent.length > 0 && (
              <section className="mb-2">
                <div className="flex items-center px-1 py-1 text-xs text-muted">
                  <button
                    onClick={() => setRecentOpen((o) => !o)}
                    aria-expanded={recentOpen}
                    className="flex items-center gap-1 hover:text-fg"
                  >
                    {recentOpen ? <ExpandedIcon className="size-3" aria-hidden /> : <CollapsedIcon className="size-3" aria-hidden />}
                    {t("sidebar.recent")}
                  </button>
                  <button onClick={clearRecent} className="ml-auto hover:text-danger" title={t("sidebar.clearRecentHint")}>
                    {t("sidebar.clearRecent")}
                  </button>
                </div>
                {recentOpen &&
                  recent.map((h) => {
                    const Kind = hostIcon(h);
                    return (
                      <button
                        key={h.id}
                        onClick={() => setSelectedId(h.id)}
                        onDoubleClick={() => connect(h)}
                        onKeyDown={(e) => {
                          if (e.key !== "Enter") return;
                          e.preventDefault();
                          connect(h);
                        }}
                        className={
                          "w-full text-left px-2 py-1 rounded-control text-sm flex items-center gap-1.5 " +
                          (selectedId === h.id ? "bg-active" : "hover:bg-hover")
                        }
                        title={t("sidebar.recentHint", { address: `${h.user}@${h.hostname}:${h.port}` })}
                      >
                        <Kind className="size-3.5 shrink-0 text-muted" aria-hidden />
                        <span className="truncate flex-1">{h.alias}</span>
                        {statusByHost[h.id] && <StatusDot status={statusByHost[h.id]} />}
                      </button>
                    );
                  })}
              </section>
            )}
            {/* Saved workspaces (R65): Enter or double-click opens one. */}
            {workspaces.length > 0 && (
              <section className="mb-2">
                <div className="flex items-center px-1 py-1 text-xs text-muted">
                  <button
                    onClick={() => setWorkspacesOpen((o) => !o)}
                    aria-expanded={workspacesOpen}
                    className="flex items-center gap-1 hover:text-fg"
                  >
                    {workspacesOpen ? <ExpandedIcon className="size-3" aria-hidden /> : <CollapsedIcon className="size-3" aria-hidden />}
                    {t("workspace.title")}
                  </button>
                </div>
                {workspacesOpen &&
                  workspaces.map((w) => (
                    <div key={w.id} className="group flex items-center rounded-control hover:bg-hover focus-within:bg-hover">
                      <button
                        onDoubleClick={() => openWorkspace(w)}
                        onKeyDown={(e) => {
                          if (e.key !== "Enter") return;
                          e.preventDefault();
                          openWorkspace(w);
                        }}
                        className="flex-1 min-w-0 text-left px-2 py-1 text-sm flex items-center gap-1.5"
                        title={t("workspace.open", { name: w.name })}
                      >
                        <WorkspaceIcon className="size-3.5 shrink-0 text-muted" aria-hidden />
                        <span className="truncate">{w.name}</span>
                      </button>
                      <IconButton
                        size="sm"
                        label={t("workspace.delete")}
                        icon={DeleteIcon}
                        onClick={() => deleteWorkspace(w)}
                        className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100"
                      />
                    </div>
                  ))}
              </section>
            )}
            {tree()}
          </>
        )}
      </div>
      {menuNode}
    </>
  );
}
