import { useMemo, useState, type KeyboardEvent, type MouseEvent } from "react";
import { hostKind, type Folder, type Host } from "../api";
import { t } from "../i18n";
import { hostIcon } from "../shell/kindIcon";
import { useHosts } from "../state/hosts";
import type { StatusMap } from "../state/sessions";
import { useContextMenu, type MenuItem } from "../ui/contextmenu";
import { IconButton } from "../ui/IconButton";
import { StatusDot } from "../ui/StatusDot";
import {
  AddIcon,
  CloneIcon,
  CollapsedIcon,
  DeleteIcon,
  DownloadIcon,
  EditIcon,
  ExpandedIcon,
  MoreIcon,
  NewFolderIcon,
  StartIcon,
  StopIcon,
  VncIcon,
  VpnIcon,
  type Icon,
} from "../ui/icons";
import { useHostManager } from "./HostManager";
import { isVpnActive, useVpnConnect, useVpns } from "./VpnPanel";

// Drag payload mime types.
const DND_HOST = "text/host-id";
const DND_FOLDER = "text/folder-id";

export type TreeNode = {
  folder: Folder;
  children: TreeNode[];
  hosts: Host[];
};

/** Build the nested folder tree + the root-level (folderless) hosts. */
export function useHostTree(hosts: Host[], folders: Folder[]) {
  return useMemo(() => {
    const byParent = new Map<string | null, Folder[]>();
    for (const f of folders) {
      const p = f.parentId ?? null;
      if (!byParent.has(p)) byParent.set(p, []);
      byParent.get(p)!.push(f);
    }
    const hostsIn = (fid: string | null) =>
      hosts.filter((h) => (h.folderId ?? null) === fid);

    const byOrder = <T extends { order?: number }>(
      a: T,
      b: T,
      tie: (a: T, b: T) => number
    ) => (a.order ?? 0) - (b.order ?? 0) || tie(a, b);

    const build = (parent: string | null): TreeNode[] =>
      (byParent.get(parent) ?? [])
        .sort((a, b) => byOrder(a, b, (x, y) => x.name.localeCompare(y.name)))
        .map((folder) => ({
          folder,
          children: build(folder.id),
          hosts: hostsIn(folder.id).sort((a, b) =>
            byOrder(a, b, (x, y) => x.alias.localeCompare(y.alias))
          ),
        }));

    return {
      roots: build(null),
      rootHosts: hostsIn(null).sort((a, b) =>
        byOrder(a, b, (x, y) => x.alias.localeCompare(y.alias))
      ),
    };
  }, [hosts, folders]);
}

// Shared callbacks passed down the tree. Bundled so HostRow/FolderNode have
// STABLE identities (defined at module scope, not inside HostTree's render) —
// re-creating them per render remounts every row and cancels in-flight drags.
type TreeCtx = {
  selectedId?: string | null;
  statusByHost: StatusMap;
  /** A flat list of search results: no folders, so no drag and drop. */
  flat: boolean;
  expanded: Record<string, boolean>;
  toggle: (id: string) => void;
  onSelect: (h: Host) => void;
  onConnect: (h: Host) => void;
  onDisconnect: (h: Host) => void;
  onOpenX11Desktop: (h: Host) => void;
  moveHost: (hostId: string, folderId: string | null) => void;
  moveFolder: (folderId: string, parentId: string | null) => void;
  reorderHost: (hostId: string, beforeHostId: string) => void;
  manager: ReturnType<typeof useHostManager>;
  menu: (items: MenuItem[]) => (e: MouseEvent) => void;
  menuAt: (items: MenuItem[], x: number, y: number) => void;
  /** The host's VPN items: connect or disconnect its profile, and choose one. */
  vpnMenu: (h: Host) => MenuItem[];
};

/** A row action: dim until the row is hovered; never starts the row's own click. */
function RowAction({
  label,
  icon,
  tone,
  onClick,
}: {
  label: string;
  icon: Icon;
  tone?: "accent" | "danger";
  onClick: (e: MouseEvent) => void;
}) {
  return (
    <IconButton
      label={label}
      icon={icon}
      tone={tone}
      size="sm"
      // The row's own keys (Enter, Shift+F10) reach every action.
      tabIndex={-1}
      onDoubleClick={(e) => e.stopPropagation()}
      onClick={(e) => {
        e.stopPropagation();
        onClick(e);
      }}
    />
  );
}

function allowDrop(e: React.DragEvent) {
  // preventDefault permits the drop. No DOM mutation / setState during drag —
  // either cancels the in-flight drag in WKWebView.
  e.preventDefault();
  e.stopPropagation();
}

const INDENT = 16; // px per tree level

/** Tree guide lines: a vertical rail per ancestor level + a horizontal elbow
 * into the current node. Purely decorative. */
function Guides({ depth }: { depth: number }) {
  if (depth === 0) return null;
  return (
    <span className="flex shrink-0 self-stretch" aria-hidden>
      {Array.from({ length: depth }).map((_, i) => {
        const last = i === depth - 1;
        return (
          <span key={i} className="relative" style={{ width: INDENT }}>
            <span className="absolute top-0 bottom-0 left-1/2 w-px bg-line" />
            {last && <span className="absolute top-1/2 left-1/2 right-0 h-px bg-line" />}
          </span>
        );
      })}
    </span>
  );
}

const hostMenu = (h: Host, ctx: TreeCtx): MenuItem[] => [
  ctx.statusByHost[h.id]
    ? { label: t("host.disconnect"), icon: StopIcon, onClick: () => ctx.onDisconnect(h) }
    : { label: t("host.connect"), icon: StartIcon, onClick: () => ctx.onConnect(h) },
  // Connect and Edit first: the row's More button lists the rest.
  { label: t("host.edit"), icon: EditIcon, onClick: () => ctx.manager.editHost(h) },
  ...ctx.vpnMenu(h),
  ...(hostKind(h) === "ssh" ? [{ label: t("x11.openDesktop"), icon: VncIcon, onClick: () => ctx.onOpenX11Desktop(h) }] : []),
  { label: t("host.clone"), icon: CloneIcon, onClick: () => ctx.manager.cloneHost(h) },
  { label: t("host.download"), icon: DownloadIcon, onClick: () => ctx.manager.downloadHost(h) },
  { label: t("host.delete"), icon: DeleteIcon, danger: true, onClick: () => ctx.manager.deleteHost(h) },
];

const folderMenu = (f: Folder, ctx: TreeCtx): MenuItem[] => [
  { label: t("folder.addHost"), icon: AddIcon, onClick: () => ctx.manager.newHost(f.id) },
  { label: t("folder.newSubfolder"), icon: NewFolderIcon, onClick: () => ctx.manager.addFolder(f.id) },
  { label: t("folder.rename"), icon: EditIcon, onClick: () => ctx.manager.renameFolder(f) },
  { label: t("folder.delete"), icon: DeleteIcon, danger: true, onClick: () => ctx.manager.deleteFolder(f) },
];

/** Shift+F10 or the Menu key opens a row's context menu under it. */
const menuKey = (e: KeyboardEvent<HTMLElement>, items: MenuItem[], ctx: TreeCtx) => {
  if (e.key !== "ContextMenu" && !(e.key === "F10" && e.shiftKey)) return false;
  e.preventDefault();
  const r = e.currentTarget.getBoundingClientRect();
  ctx.menuAt(items, r.left + 24, r.bottom);
  return true;
};

const moreAt = (items: MenuItem[], ctx: TreeCtx) => (e: MouseEvent) => {
  const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
  ctx.menuAt(items, r.left, r.bottom);
};

function HostRow({ h, ctx, depth = 0 }: { h: Host; ctx: TreeCtx; depth?: number }) {
  const status = ctx.statusByHost[h.id];
  const selected = ctx.selectedId === h.id;
  const Kind = hostIcon(h);
  const dnd = ctx.flat
    ? {}
    : {
        draggable: true,
        onDragStart: (e: React.DragEvent) => {
          e.stopPropagation();
          e.dataTransfer.setData(DND_HOST, h.id);
          e.dataTransfer.effectAllowed = "move";
        },
        onDragOver: allowDrop,
        onDrop: (e: React.DragEvent) => {
          e.preventDefault();
          e.stopPropagation();
          const dragged = e.dataTransfer.getData(DND_HOST);
          if (dragged && dragged !== h.id) ctx.reorderHost(dragged, h.id);
        },
      };
  return (
    <div
      {...dnd}
      role="treeitem"
      aria-selected={selected}
      data-row="host"
      tabIndex={selected ? 0 : -1}
      // Single click selects (after a drag ends, never during one); Enter or
      // double-click connects.
      onClick={() => ctx.onSelect(h)}
      onDoubleClick={() => ctx.onConnect(h)}
      onKeyDown={(e) => {
        if (menuKey(e, hostMenu(h, ctx), ctx)) return;
        if (e.key === "Enter") {
          e.preventDefault();
          ctx.onConnect(h);
        }
      }}
      onContextMenu={ctx.menu(hostMenu(h, ctx))}
      className={
        "px-2 py-1 rounded-control text-sm cursor-pointer flex items-center gap-1.5 group outline-none focus-visible:ring-2 focus-visible:ring-focus " +
        (selected ? "bg-active" : "hover:bg-hover")
      }
      title={`${h.user}@${h.hostname}:${h.port}`}
    >
      <Guides depth={depth} />
      <Kind className="size-3.5 shrink-0 text-muted" aria-hidden />
      <span className="truncate flex-1 min-w-0">{h.alias}</span>
      {status && <StatusDot status={status} />}
      <span className="flex items-center opacity-0 group-hover:opacity-100 shrink-0">
        {status ? (
          <RowAction label={t("host.disconnect")} icon={StopIcon} tone="danger" onClick={() => ctx.onDisconnect(h)} />
        ) : (
          <RowAction label={t("host.connect")} icon={StartIcon} tone="accent" onClick={() => ctx.onConnect(h)} />
        )}
        <RowAction label={t("host.edit")} icon={EditIcon} onClick={() => ctx.manager.editHost(h)} />
        <RowAction label={t("common.more")} icon={MoreIcon} onClick={moreAt(hostMenu(h, ctx).slice(2), ctx)} />
      </span>
    </div>
  );
}

function FolderNode({ node, ctx, depth = 0 }: { node: TreeNode; ctx: TreeCtx; depth?: number }) {
  const f = node.folder;
  const isCollapsed = !ctx.expanded[f.id];
  const count = node.hosts.length + node.children.length;
  const dropInto = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    const hostId = e.dataTransfer.getData(DND_HOST);
    if (hostId) return ctx.moveHost(hostId, f.id);
    const movedFolder = e.dataTransfer.getData(DND_FOLDER);
    if (movedFolder && movedFolder !== f.id) ctx.moveFolder(movedFolder, f.id);
  };
  return (
    <div role="group">
      <div
        draggable
        onDragStart={(e) => {
          e.stopPropagation();
          e.dataTransfer.setData(DND_FOLDER, f.id);
          e.dataTransfer.effectAllowed = "move";
        }}
        onDragOver={allowDrop}
        onDrop={dropInto}
        role="treeitem"
        aria-expanded={!isCollapsed}
        data-row="folder"
        tabIndex={-1}
        onClick={() => ctx.toggle(f.id)}
        onKeyDown={(e) => {
          if (menuKey(e, folderMenu(f, ctx), ctx)) return;
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            ctx.toggle(f.id);
          }
        }}
        onContextMenu={ctx.menu(folderMenu(f, ctx))}
        className="px-2 py-1 rounded-control flex items-center gap-1 text-xs font-semibold uppercase tracking-wide cursor-pointer transition-colors group text-muted hover:text-fg hover:bg-hover outline-none focus-visible:ring-2 focus-visible:ring-focus"
      >
        <Guides depth={depth} />
        {isCollapsed ? <CollapsedIcon className="size-3 shrink-0" aria-hidden /> : <ExpandedIcon className="size-3 shrink-0" aria-hidden />}
        <span className="truncate">{f.name}</span>
        <span className="opacity-60 font-normal lowercase">{count}</span>
        <span className="ml-auto flex items-center opacity-0 group-hover:opacity-100 normal-case">
          <RowAction label={t("folder.addHost")} icon={AddIcon} onClick={() => ctx.manager.newHost(f.id)} />
          <RowAction label={t("common.more")} icon={MoreIcon} onClick={moreAt(folderMenu(f, ctx).slice(1), ctx)} />
        </span>
      </div>
      {!isCollapsed && (
        <div>
          {node.children.map((c) => (
            <FolderNode key={c.folder.id} node={c} ctx={ctx} depth={depth + 1} />
          ))}
          {node.hosts.map((h) => (
            <HostRow key={h.id} h={h} ctx={ctx} depth={depth + 1} />
          ))}
          {count === 0 && (
            <div
              onDragOver={allowDrop}
              onDrop={dropInto}
              className="px-2 py-1.5 text-xs text-muted italic"
              style={{ paddingLeft: (depth + 1) * INDENT + 8 }}
            >
              {t("folder.empty")}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** The host tree of the Hosts panel: folders with drag and drop, or a flat
 *  list of search results. Arrow keys move between rows. */
export function HostTree(props: {
  /** Search results to list instead of the tree. */
  flat?: Host[];
  selectedId?: string | null;
  statusByHost: StatusMap;
  onSelect: (h: Host) => void;
  onConnect: (h: Host) => void;
  onDisconnect: (h: Host) => void;
  onOpenX11Desktop: (h: Host) => void;
}) {
  const { hosts, folders, moveHost, moveFolder, reorderHost } = useHosts();
  const manager = useHostManager();
  const { roots, rootHosts } = useHostTree(hosts, folders);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const { menu, at, node: menuNode } = useContextMenu();
  const { vpns, states } = useVpns();
  const vpn = useVpnConnect();

  const ctx: TreeCtx = {
    ...props,
    flat: !!props.flat,
    expanded,
    toggle: (id) => setExpanded((c) => ({ ...c, [id]: !c[id] })),
    moveHost,
    moveFolder,
    reorderHost,
    manager,
    menu,
    menuAt: at,
    vpnMenu: (h) => {
      const v = h.vpnId ? vpns.find((x) => x.id === h.vpnId) : undefined;
      return [
        ...(v
          ? [
              isVpnActive(states[v.id])
                ? { label: t("host.vpnDisconnect", { name: v.name }), icon: VpnIcon, onClick: () => vpn.disconnect(v.id) }
                : { label: t("host.vpnConnect", { name: v.name }), icon: VpnIcon, onClick: () => vpn.connect(v.id) },
            ]
          : []),
        // The host editor is where a host names its profile.
        { label: t("host.vpnSet"), icon: VpnIcon, onClick: () => manager.editHost(h) },
      ];
    },
  };

  const dropRoot = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    const hostId = e.dataTransfer.getData(DND_HOST);
    if (hostId) return moveHost(hostId, null);
    const movedFolder = e.dataTransfer.getData(DND_FOLDER);
    if (movedFolder) moveFolder(movedFolder, null);
  };

  // Up and Down move focus between visible rows; a host row it lands on is selected.
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    e.preventDefault();
    const rows = [...e.currentTarget.querySelectorAll<HTMLElement>("[data-row]")];
    const at = rows.indexOf(document.activeElement as HTMLElement);
    const next = rows[Math.max(0, Math.min(rows.length - 1, at + (e.key === "ArrowDown" ? 1 : -1)))];
    next?.focus();
    if (next?.dataset.row === "host") next.click();
  };

  const empty = props.flat ? props.flat.length === 0 : roots.length === 0 && rootHosts.length === 0;
  return (
    <div
      role="tree"
      aria-label={t("nav.hosts")}
      onKeyDown={onKeyDown}
      onDragOver={allowDrop}
      onDrop={dropRoot}
      className="flex flex-col gap-px min-h-8"
    >
      {menuNode}
      {vpn.dialog}
      {props.flat ? (
        props.flat.map((h) => <HostRow key={h.id} h={h} ctx={ctx} />)
      ) : (
        <>
          {roots.map((node) => (
            <FolderNode key={node.folder.id} node={node} ctx={ctx} />
          ))}
          {rootHosts.map((h) => (
            <HostRow key={h.id} h={h} ctx={ctx} />
          ))}
        </>
      )}
      {empty && <div className="text-xs text-muted px-2 py-2">{t(props.flat ? "hosts.noMatch" : "hosts.empty.title")}</div>}
    </div>
  );
}
