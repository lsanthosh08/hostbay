import { useCallback, useEffect, useRef, useState } from "react";
import Editor from "@monaco-editor/react";
import { applyEditorTheme } from "../monaco";
import { open, openFolder, save } from "../dialogs";
import * as api from "../api";
import type { DirEntry } from "../api";
import type { Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { AddIcon, CloseIcon, FileIcon, FolderIcon, SplitIcon } from "../ui/icons";
import { Panel, Spinner, Badge } from "../ui/kit";
import { useToast, useAsyncToast } from "../ui/toast";
import { usePrompt } from "../ui/prompt";
import { useContextMenu } from "../ui/contextmenu";
import { basename, humanSize, joinPath, parentPath } from "./files/paths";
import { FilesView } from "./files/FilesView";
import { t } from "../i18n";
import { useTransferActions } from "../state/transfers";

const octalPerms = (mode: number): string =>
  (mode & 0o777).toString(8).padStart(3, "0");

export function FilesTab({ conn, windows }: { conn: Connection; windows?: boolean }) {
  const { push } = useToast();
  const run = useAsyncToast();
  const { prompt, confirm } = usePrompt();
  const { menu, node: menuNode } = useContextMenu();
  const transfers = useTransferActions();
  // Two panes: this session beside another source, for transfers. The
  // single-pane listing and editor stay mounted underneath.
  const [split, setSplit] = useState(false);

  // Windows OpenSSH SFTP uses forward slashes with a drive root (/C:/...).
  const home = windows
    ? conn.host.user
      ? `/C:/Users/${conn.host.user}`
      : "/C:/"
    : conn.host.user
    ? `/home/${conn.host.user}`
    : "/";
  const [cwd, setCwd] = useState<string>(home);
  const [pathInput, setPathInput] = useState<string>(home);
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [sudo, setSudo] = useState(false);
  const [clipboard, setClipboard] = useState<string | null>(null);

  // editor
  const [openFile, setOpenFile] = useState<string | null>(null);
  const [content, setContent] = useState<string>("");
  const [fileLoading, setFileLoading] = useState(false);

  // Draggable split: left (file list) width as a % of the row. Drag the
  // divider left to grow the editor.
  const [leftPct, setLeftPct] = useState(50);
  const splitRef = useRef<HTMLDivElement>(null);
  const startDrag = (e: React.MouseEvent) => {
    e.preventDefault();
    const row = splitRef.current;
    if (!row) return;
    const onMove = (ev: MouseEvent) => {
      const r = row.getBoundingClientRect();
      const pct = ((ev.clientX - r.left) / r.width) * 100;
      setLeftPct(Math.max(20, Math.min(80, pct))); // clamp 20–80%
    };
    const onUp = () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  };

  // inline "new name" prompt: kind tells us what to create
  const [creating, setCreating] = useState<null | "file" | "folder">(null);
  const [newName, setNewName] = useState("");

  // Bumped per request; a response that is no longer the latest is dropped
  // so a slow earlier listing or read cannot replace a newer one.
  const listSeq = useRef(0);
  const fileSeq = useRef(0);

  const list = useCallback(
    async (path: string) => {
      const my = ++listSeq.current;
      setLoading(true);
      try {
        const res = await api.sftpList(conn.sessionId, path);
        if (my !== listSeq.current) return;
        res.sort((a, b) =>
          a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1
        );
        setEntries(res);
        setCwd(path);
        setPathInput(path);
        setSelected(null);
      } catch (e) {
        push("error", e instanceof Error ? e.message : String(e));
      } finally {
        if (my === listSeq.current) setLoading(false);
      }
    },
    [conn.sessionId, push]
  );

  useEffect(() => {
    list(home).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conn.sessionId]);

  const navigate = (path: string) => list(path);

  const openTextFile = async (path: string) => {
    const my = ++fileSeq.current;
    setFileLoading(true);
    try {
      const text = await api.sftpRead(conn.sessionId, path);
      if (my !== fileSeq.current) return;
      setOpenFile(path);
      setContent(text);
    } catch (e) {
      push("error", e instanceof Error ? e.message : String(e));
    } finally {
      if (my === fileSeq.current) setFileLoading(false);
    }
  };

  const onRowClick = (e: DirEntry) => {
    setSelected(e.path);
    if (e.isDir) navigate(e.path);
    else openTextFile(e.path);
  };

  // Write with sudo, reusing the stored login password first (backend does
  // that automatically). Only if that's rejected do we prompt for a sudo
  // password and retry once. Non-sudo writes go straight through.
  const writeWithSudo = async (path: string, body: string, okMsg: string): Promise<boolean> => {
    if (!sudo) {
      const r = await run(() => api.sftpWrite(conn.sessionId, path, body, false), okMsg);
      return r !== undefined;
    }
    try {
      await api.sftpWrite(conn.sessionId, path, body, true); // uses stored pw
      push("success", okMsg);
      return true;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (!/password/i.test(msg)) {
        push("error", msg);
        return false;
      }
      // Stored password missing/wrong -> ask and retry once.
      const pw = await prompt(`sudo password for ${conn.host.user}`, "", true);
      if (!pw) return false;
      const r = await run(
        () => api.sftpWrite(conn.sessionId, path, body, true, pw),
        okMsg
      );
      return r !== undefined;
    }
  };

  const saveFile = async () => {
    if (!openFile) return;
    await writeWithSudo(openFile, content, "Saved");
  };

  const doCreate = async () => {
    const name = newName.trim();
    if (!name || !creating) {
      setCreating(null);
      setNewName("");
      return;
    }
    const target = joinPath(cwd, name);
    if (creating === "folder") {
      await run(() => api.sftpMkdir(conn.sessionId, target), "Folder created");
    } else {
      await writeWithSudo(target, "", "File created");
    }
    setCreating(null);
    setNewName("");
    list(cwd);
  };

  const renamePath = async (path: string) => {
    const cur = basename(path);
    const name = await prompt("Rename to:", cur);
    if (!name || name === cur) return;
    const to = joinPath(parentPath(path), name);
    await run(() => api.sftpRename(conn.sessionId, path, to), "Renamed");
    list(cwd);
  };

  const removePath = async (path: string, isDir: boolean) => {
    if (!(await confirm(`Delete ${basename(path)}?`))) return;
    await run(() => api.sftpRemove(conn.sessionId, path, isDir, sudo), "Deleted");
    if (openFile === path) {
      setOpenFile(null);
      setContent("");
    }
    list(cwd);
  };

  const paste = async () => {
    if (!clipboard) return;
    const to = joinPath(cwd, basename(clipboard));
    await run(() => api.sftpCopy(conn.sessionId, clipboard, to, sudo), "Pasted");
    list(cwd);
  };

  const upload = async () => {
    const local = await open();
    if (typeof local !== "string") return;
    const remote = joinPath(cwd, basename(local));
    await run(() => api.sftpUpload(conn.sessionId, local, remote), "Uploaded");
    list(cwd);
  };

  // Whole folders go through the transfer engine, which asks about each
  // name conflict and shows progress in the transfers panel.
  const uploadFolder = async () => {
    const dir = await openFolder();
    if (dir) transfers.start(api.transferLocal, [dir], conn.sessionId, cwd, t("files.copyLabel", { what: basename(dir), to: conn.host.alias, dir: cwd }));
  };
  const downloadFolder = async (path: string) => {
    const dir = await openFolder();
    if (dir) transfers.start(conn.sessionId, [path], api.transferLocal, dir, t("files.copyLabel", { what: basename(path), to: t("files.local"), dir }));
  };

  const downloadPath = async (path: string) => {
    const local = await save({ defaultPath: basename(path) });
    if (typeof local !== "string") return;
    await run(() => api.sftpDownload(conn.sessionId, path, local), "Downloaded");
  };
  // Editable, persisted bookmarks (home is always shown as ~).
  const [bookmarks, setBookmarks] = useState<string[]>([]);
  useEffect(() => {
    api.listBookmarks().then(setBookmarks).catch(() => {});
  }, []);
  const persistBookmarks = (list: string[]) => {
    setBookmarks(list);
    api.saveBookmarks(list).catch(() => {});
  };
  const addBookmark = () => {
    if (bookmarks.includes(cwd)) return;
    persistBookmarks([...bookmarks, cwd]);
  };
  const removeBookmark = (p: string) => persistBookmarks(bookmarks.filter((b) => b !== p));

  return (
    <div className="flex flex-col h-full p-4 gap-3">
      {menuNode}
      {/* top bar */}
      <div className="flex items-center gap-2 flex-wrap">
        <Badge tone="ok">{conn.host.alias}</Badge>
        <form
          className="flex items-center gap-2 flex-1 min-w-[16rem]"
          onSubmit={(e) => {
            e.preventDefault();
            navigate(pathInput.trim() || "/");
          }}
        >
          <Input
            className="flex-1"
            value={pathInput}
            onChange={(e) => setPathInput(e.target.value)}
            placeholder="/path/to/dir"
          />
          <Button variant="default" type="submit">
            Go
          </Button>
        </form>
        <Button variant="ghost" onClick={() => list(cwd)}>
          Refresh
        </Button>
        <Button variant="ghost" icon={SplitIcon} aria-pressed={split} onClick={() => setSplit(!split)}>
          {t("files.twoPanes")}
        </Button>
      </div>
      {split && (
        <FilesView
          left={{ source: conn.sessionId, name: conn.host.alias }}
          leftDir={cwd}
          fixedLeft
          className=""
          onOpenFile={(path) => {
            setSplit(false);
            openTextFile(path);
          }}
        />
      )}
      <div className={split ? "hidden" : "contents"}>

      {/* bookmarks — editable */}
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-xs opacity-60">Bookmarks:</span>
        <Button variant="ghost" onClick={() => navigate(home)}>
          ~
        </Button>
        {bookmarks.map((p) => (
          <span
            key={p}
            className="group inline-flex items-center rounded-control bg-control-bg text-sm"
          >
            <button
              onClick={() => navigate(p)}
              className="px-2 py-1 hover:text-accent"
              title={p}
            >
              {p}
            </button>
            <IconButton
              size="sm"
              label="Remove bookmark"
              icon={CloseIcon}
              className="opacity-40 group-hover:opacity-100"
              onClick={() => removeBookmark(p)}
            />
          </span>
        ))}
        <Button variant="ghost" icon={AddIcon} onClick={addBookmark} title={`Bookmark ${cwd}`}>
          Bookmark this
        </Button>
      </div>

      {/* toolbar — file actions live in the right-click menus (on rows and
          in empty space). Global bits only here. */}
      <div className="flex items-center gap-2 flex-wrap text-sm">
        <span className="text-xs opacity-50">Right-click a file or empty space for actions.</span>
        <label className="flex items-center gap-1.5 ml-auto cursor-pointer">
          <input type="checkbox" checked={sudo} onChange={(e) => setSudo(e.target.checked)} />
          sudo
        </label>
      </div>

      {creating && (
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            doCreate();
          }}
        >
          <span className="text-xs opacity-60">
            New {creating} in {cwd}:
          </span>
          <Input
            autoFocus
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            placeholder="name"
          />
          <Button variant="primary" type="submit">
            Create
          </Button>
          <Button
            variant="ghost"
            type="button"
            onClick={() => {
              setCreating(null);
              setNewName("");
            }}
          >
            Cancel
          </Button>
        </form>
      )}

      {/* split: listing | editor — divider is draggable to resize */}
      <div ref={splitRef} className="flex-1 min-h-0 flex">
        {/* listing */}
        <div className="min-w-0 h-full flex" style={{ width: `${leftPct}%` }}>
        <Panel
          className="flex-1"
          title={<span className="font-mono text-xs truncate">{cwd}</span>}
          bodyClassName="flex-1 min-h-0 overflow-auto"
        >
          <div
            className="min-h-full"
            onContextMenu={(ev) => {
              // Empty-space menu: only if the click wasn't on a row (rows
              // stopPropagation via their own handler firing first).
              menu([
                { label: "New folder", onClick: () => setCreating("folder") },
                { label: "New file", onClick: () => setCreating("file") },
                {
                  label: clipboard ? `Paste (${basename(clipboard)})` : "Paste",
                  disabled: !clipboard,
                  onClick: paste,
                },
                { label: "Upload file…", onClick: upload },
                { label: t("files.uploadFolder"), onClick: uploadFolder },
                { label: "Refresh", onClick: () => list(cwd) },
              ])(ev);
            }}
          >
            {loading ? (
              <div className="flex items-center justify-center h-40">
                <Spinner />
              </div>
            ) : (
              <table className="w-full text-sm">
                <thead className="sticky top-0 bg-surface text-left text-xs opacity-60">
                  <tr>
                    <th className="px-3 py-2">Name</th>
                    <th className="px-3 py-2">Size</th>
                    <th className="px-3 py-2">Perms</th>
                    <th className="px-3 py-2">Modified</th>
                  </tr>
                </thead>
                <tbody>
                  {cwd !== "/" && (
                    <tr
                      className="cursor-pointer hover:bg-hover"
                      onClick={() => navigate(parentPath(cwd))}
                    >
                      <td className="px-3 py-1.5" colSpan={4}>
                        <span className="inline-flex items-center gap-1.5">
                          <FolderIcon className="size-4 text-muted" aria-hidden /> ..
                        </span>
                      </td>
                    </tr>
                  )}
                  {entries.map((e) => (
                    <tr
                      key={e.path}
                      onClick={() => onRowClick(e)}
                      onContextMenu={(ev) => {
                        setSelected(e.path);
                        menu([
                          e.isDir
                            ? { label: "Open", onClick: () => navigate(e.path) }
                            : { label: "Edit", onClick: () => openTextFile(e.path) },
                          { label: "Rename", onClick: () => renamePath(e.path) },
                          {
                            label: "Copy",
                            onClick: () => {
                              setClipboard(e.path);
                              push("info", `Copied ${basename(e.path)}`);
                            },
                          },
                          {
                            label: e.isDir ? t("files.downloadFolder") : "Download",
                            onClick: () => (e.isDir ? downloadFolder(e.path) : downloadPath(e.path)),
                          },
                          { label: "Delete", danger: true, onClick: () => removePath(e.path, e.isDir) },
                        ])(ev);
                      }}
                      className={
                        "cursor-pointer " +
                        (selected === e.path
                          ? "bg-active"
                          : "hover:bg-hover")
                      }
                    >
                      <td className="px-3 py-1.5 truncate max-w-[14rem]">
                        {e.isDir ? (
                          <FolderIcon className="inline size-4 mr-1.5 -mt-0.5 text-muted" aria-hidden />
                        ) : (
                          <FileIcon className="inline size-4 mr-1.5 -mt-0.5 text-muted" aria-hidden />
                        )}
                        {e.name}
                      </td>
                      <td className="px-3 py-1.5 whitespace-nowrap opacity-80">
                        {e.isDir ? "—" : humanSize(e.size)}
                      </td>
                      <td className="px-3 py-1.5 font-mono opacity-80">{octalPerms(e.mode)}</td>
                      <td className="px-3 py-1.5 whitespace-nowrap opacity-70">
                        {new Date(e.mtime * 1000).toLocaleString()}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {!loading && entries.length === 0 && (
              <EmptyState>Empty directory</EmptyState>
            )}
          </div>
        </Panel>
        </div>

        {/* drag handle */}
        <div
          onMouseDown={startDrag}
          className="w-1.5 mx-1 shrink-0 rounded cursor-col-resize bg-transparent hover:bg-accent/40 active:bg-accent/60 transition-colors"
          title="Drag to resize"
        />

        {/* editor */}
        <div className="min-w-0 h-full flex flex-1">
        <Panel
          className="flex-1"
          title={
            <span className="font-mono text-xs truncate">
              {openFile ?? "No file open"}
            </span>
          }
          actions={
            <Button variant="primary" onClick={saveFile} disabled={!openFile || fileLoading}>
              Save
            </Button>
          }
          bodyClassName="flex-1 min-h-0"
        >
          <div className="h-full min-h-0">
            {fileLoading ? (
              <div className="flex items-center justify-center h-full">
                <Spinner />
              </div>
            ) : openFile ? (
              <Editor
                height="100%"
                theme="hostbay"
                beforeMount={applyEditorTheme}
                path={openFile}
                value={content}
                onChange={(v) => setContent(v ?? "")}
                options={{
                  fontSize: 13,
                  minimap: { enabled: false },
                  wordWrap: "on",
                  // Reflow to the container when the split divider resizes it —
                  // without this the editor keeps its old width and shifts off
                  // to the side after shrinking.
                  automaticLayout: true,
                }}
              />
            ) : (
              <EmptyState>Click a file to edit it</EmptyState>
            )}
          </div>
        </Panel>
        </div>
      </div>
      </div>
    </div>
  );
}
