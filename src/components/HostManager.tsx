import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import * as api from "../api";
import type { Host, Folder } from "../api";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { t } from "../i18n";
import { useToast, useAsyncToast } from "../ui/toast";
import { usePrompt } from "../ui/prompt";
import { useHosts } from "../state/hosts";
import { HostEditor } from "../shell/HostEditor";
import { BackupForm } from "../shell/settings/Backup";
import { MasterPasswordSetupDialog } from "./MasterPassword";
import { open } from "../dialogs";

const blank = (): Host => ({
  id: "",
  alias: "",
  hostname: "",
  port: 22,
  user: "",
  identityFile: "",
  tags: [],
  folderId: null,
  authType: "key",
  kind: "ssh",
});

export type HostManager = {
  needsMasterPassword: boolean;
  newHost: (folderId?: string | null) => void;
  editHost: (h: Host) => void;
  cloneHost: (h: Host) => void;
  deleteHost: (h: Host) => void;
  downloadHost: (h: Host) => void;
  addFolder: (parentId: string | null) => void;
  renameFolder: (f: Folder) => void;
  deleteFolder: (f: Folder) => void;
  importSshConfig: () => void;
  restore: () => void;
  setMasterPassword: () => void;
};

const Ctx = createContext<HostManager | null>(null);
export const useHostManager = () => useContext(Ctx)!;

/** Host, folder, restore and import operations with their dialogs (the host
 *  editor drawer among them), for the Hosts panel, the Start page, the
 *  palette and Settings. Backup and the Trash live in Settings > Backup. */
export function HostManagerProvider({ children }: { children: ReactNode }) {
  const { hosts, folders, reloadAll: onChanged } = useHosts();
  const { push } = useToast();
  const run = useAsyncToast();
  const { prompt, confirm } = usePrompt();
  const [editing, setEditing] = useState<Host | null>(null);
  // A single host's backup, while its format dialog is open.
  const [backingUp, setBackingUp] = useState<Host | null>(null);
  const [folderDelete, setFolderDelete] = useState<{ folder: Folder; count: number } | null>(null);
  // Linux only: no master password yet, so passwords cannot be saved (KTD7).
  const [needsMaster, setNeedsMaster] = useState(false);
  const [settingMaster, setSettingMaster] = useState(false);

  useEffect(() => {
    api.needsMasterPassword().then(setNeedsMaster).catch(() => {});
  }, []);

  const del = async (h: Host) => {
    if (!(await confirm(`Delete host ${h.alias}?`))) return;
    await run(() => api.deleteHost(h.id), "Deleted");
    onChanged();
  };

  // folder CRUD
  const addFolder = async (parentId: string | null) => {
    const name = await prompt("New folder name:");
    if (!name?.trim()) return;
    await run(() => api.saveFolder({ id: "", name: name.trim(), parentId }), "Folder created");
    onChanged();
  };
  const renameFolder = async (f: Folder) => {
    const name = await prompt("Rename folder:", f.name);
    if (!name?.trim() || name === f.name) return;
    await run(() => api.saveFolder({ ...f, name: name.trim() }), "Renamed");
    onChanged();
  };
  // Count hosts under a folder + all its descendants.
  const hostsUnder = (folderId: string): number => {
    const childFolders = folders.filter((x) => x.parentId === folderId);
    const direct = hosts.filter((h) => h.folderId === folderId).length;
    return direct + childFolders.reduce((n, cf) => n + hostsUnder(cf.id), 0);
  };
  const deleteFolder = (f: Folder) => {
    const n = hostsUnder(f.id);
    if (n === 0) {
      // Empty folder — one plain confirm.
      confirm(`Delete folder "${f.name}"?`).then((ok) => {
        if (ok) run(() => api.deleteFolder(f.id, "reparent"), "Folder deleted").then(onChanged);
      });
      return;
    }
    setFolderDelete({ folder: f, count: n });
  };
  const doDeleteFolder = async (mode: "reparent" | "purge") => {
    if (!folderDelete) return;
    await run(
      () => api.deleteFolder(folderDelete.folder.id, mode),
      mode === "purge" ? "Folder + hosts deleted" : "Folder deleted, hosts moved up"
    );
    setFolderDelete(null);
    onChanged();
  };

  const importCfg = async () => {
    // Let the user pick an SSH config file (or a folder containing `config`).
    const picked = await open({
      filters: [{ name: "SSH config", extensions: ["", "config", "txt"] }],
    });
    if (typeof picked !== "string") return;
    const r = await run(() => api.importSshConfig(picked));
    if (r !== undefined) {
      push("success", `Imported ${r.imported} host(s)`);
      for (const note of r.notes ?? []) push("info", note);
      setEditing(null); // bulk import → close the form so new hosts show
      onChanged();
    }
  };

  // Restore from a Hostbay backup (asks for the passphrase) or a plain JSON
  // export.
  const restoreStore = async () => {
    const path = await open({
      filters: [
        { name: "Backup", extensions: ["hostbay", "json"] },
      ],
    });
    if (typeof path !== "string") return;
    let pass: string | undefined;
    if (/\.hostbay$/i.test(path)) {
      const p = await prompt("Enter the backup passphrase", "", true);
      if (!p) return;
      pass = p;
    }
    const r = await run(() => api.importStore(path, pass));
    if (r !== undefined) {
      push("success", `Restored ${r.imported} host(s)`);
      for (const note of r.notes ?? []) push("info", note);
      onChanged();
    }
  };

  const value: HostManager = {
    needsMasterPassword: needsMaster,
    newHost: (folderId = null) => setEditing({ ...blank(), folderId }),
    // A session or a failed connect holds the host as it was when the tab
    // opened; edit the saved one, so a save cannot undo a later edit.
    editHost: (h) => setEditing(hosts.find((x) => x.id === h.id) ?? h),
    cloneHost: async (h) => {
      const c = await run(() => api.cloneHost(h.id), `Cloned ${h.alias}`);
      if (c) onChanged();
    },
    deleteHost: del,
    downloadHost: setBackingUp,
    addFolder,
    renameFolder,
    deleteFolder,
    importSshConfig: importCfg,
    restore: restoreStore,
    setMasterPassword: () => setSettingMaster(true),
  };

  return (
    <Ctx.Provider value={value}>
      {children}
      {settingMaster && (
        <MasterPasswordSetupDialog
          onDone={() => {
            setSettingMaster(false);
            setNeedsMaster(false);
            push("success", "Master password set");
          }}
          onCancel={() => setSettingMaster(false)}
        />
      )}

      {backingUp && (
        <Dialog title={t("backup.hostTitle", { name: backingUp.alias })} onClose={() => setBackingUp(null)}>
          <BackupForm
            name={backingUp.alias}
            onExport={async (path, pass) => {
              if ((await run(() => api.exportHost(backingUp.id, path, pass), t("backup.hostSaved", { name: backingUp.alias }))) !== undefined)
                setBackingUp(null);
            }}
          />
        </Dialog>
      )}

      {folderDelete && (
        <Dialog title={`Delete folder “${folderDelete.folder.name}”`} onClose={() => setFolderDelete(null)}>
          <p className="text-sm text-muted mb-4">
            This folder contains <b>{folderDelete.count}</b> host
            {folderDelete.count === 1 ? "" : "s"} (including subfolders). What
            should happen to them?
          </p>
          <div className="flex flex-col gap-2">
            <Button variant="primary" onClick={() => doDeleteFolder("reparent")}>
              Keep hosts — move them up to the parent
            </Button>
            <Button variant="danger" onClick={() => doDeleteFolder("purge")}>
              Delete the hosts too (to Trash)
            </Button>
            <Button variant="ghost" onClick={() => setFolderDelete(null)}>
              Cancel
            </Button>
          </div>
        </Dialog>
      )}

      {editing && (
        <HostEditor
          // A new key per opening, so the drawer starts from this host.
          key={editing.id || "new"}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={onChanged}
          onImportSshConfig={importCfg}
        />
      )}
    </Ctx.Provider>
  );
}
