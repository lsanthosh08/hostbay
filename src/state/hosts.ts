import { createContext, createElement, useContext, useEffect, useState, type ReactNode } from "react";
import * as api from "../api";
import type { Folder, Host, Workspace } from "../api";

/** The hosts connected to most recently, newest first. */
export const recentHosts = (hosts: Host[], n = 5) =>
  hosts
    .filter((h) => h.lastConnected)
    .sort((a, b) => Number(b.lastConnected) - Number(a.lastConnected))
    .slice(0, n);

export type Hosts = {
  hosts: Host[];
  folders: Folder[];
  /** Saved workspaces (R65); listing them connects nothing. */
  workspaces: Workspace[];
  reloadWorkspaces: () => void;
  reloadHosts: () => Promise<void>;
  reloadAll: () => void;
  moveHost: (hostId: string, folderId: string | null) => void;
  moveFolder: (folderId: string, parentId: string | null) => void;
  reorderHost: (hostId: string, beforeHostId: string) => void;
  clearRecent: () => void;
};

const HostsCtx = createContext<Hosts | null>(null);
export const useHosts = () => useContext(HostsCtx)!;

/** Saved hosts and folders, loaded from the backend, with the tree edits. */
export function HostsProvider({ children }: { children: ReactNode }) {
  const [hosts, setHosts] = useState<Host[]>([]);
  const [folders, setFolders] = useState<Folder[]>([]);
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);

  const reloadHosts = () => api.listHosts().then(setHosts).catch(() => {});
  const reloadFolders = () => api.listFolders().then(setFolders).catch(() => {});
  const reloadAll = () => {
    reloadHosts();
    reloadFolders();
  };
  useEffect(reloadAll, []);
  // Whatever reloads the hosts (sync, a restore) may have changed these too.
  const reloadWorkspaces = () => void api.listWorkspaces().then(setWorkspaces).catch(() => {});
  useEffect(reloadWorkspaces, [hosts]);

  const clearRecent = async () => {
    await api.clearRecent().catch(() => {});
    reloadHosts();
  };

  const moveHost = async (hostId: string, folderId: string | null) => {
    const h = hosts.find((x) => x.id === hostId);
    if (!h || (h.folderId ?? null) === folderId) return;
    await api.saveHost({ ...h, folderId }).catch(() => {});
    reloadHosts();
  };
  const moveFolder = async (folderId: string, parentId: string | null) => {
    const f = folders.find((x) => x.id === folderId);
    if (!f || (f.parentId ?? null) === parentId) return;
    await api.saveFolder({ ...f, parentId }).catch(() => {});
    reloadFolders();
  };

  // Drop `hostId` immediately before `beforeHostId`: reindex that folder's
  // hosts with the dragged one inserted at the target position.
  const reorderHost = async (hostId: string, beforeHostId: string) => {
    const target = hosts.find((h) => h.id === beforeHostId);
    const dragged = hosts.find((h) => h.id === hostId);
    if (!target || !dragged) return;
    const folderId = target.folderId ?? null;
    const siblings = hosts
      .filter((h) => (h.folderId ?? null) === folderId && h.id !== hostId)
      .sort((a, b) => (a.order ?? 0) - (b.order ?? 0) || a.alias.localeCompare(b.alias));
    const idx = siblings.findIndex((h) => h.id === beforeHostId);
    const ordered = [...siblings];
    ordered.splice(idx < 0 ? ordered.length : idx, 0, { ...dragged, folderId });
    // Persist new sequential order; only save rows that changed.
    await Promise.all(
      ordered.map((h, i) =>
        h.order === i && (h.folderId ?? null) === folderId
          ? Promise.resolve()
          : api.saveHost({ ...h, folderId, order: i }).then(() => {})
      )
    ).catch(() => {});
    reloadHosts();
  };

  const value: Hosts = { hosts, folders, workspaces, reloadWorkspaces, reloadHosts, reloadAll, moveHost, moveFolder, reorderHost, clearRecent };
  return createElement(HostsCtx.Provider, { value }, children);
}
