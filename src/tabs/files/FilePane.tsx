import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import * as api from "../../api";
import type { DirEntry } from "../../api";
import { t } from "../../i18n";
import { useTransferActions, useTransferLanded } from "../../state/transfers";
import { Button, focusRing } from "../../ui/Button";
import { useContextMenu } from "../../ui/contextmenu";
import { EmptyState } from "../../ui/EmptyState";
import { Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import {
  CopyLeftIcon,
  CopyRightIcon,
  DeleteIcon,
  EditIcon,
  FileIcon,
  FolderIcon,
  NewFolderIcon,
  ParentFolderIcon,
  RefreshIcon,
} from "../../ui/icons";
import { Spinner } from "../../ui/kit";
import { usePrompt } from "../../ui/prompt";
import { useToast } from "../../ui/toast";
import { humanSize, joinPath, parentPath } from "./paths";

/** The pane across from this one, where Copy sends the selection. */
export type OtherPane = { source: string; cwd: string; name: string; side: "left" | "right" };

const byName = (a: DirEntry, b: DirEntry) => (a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1);

/**
 * One side of the Files view: a folder of any transfer source (this
 * computer, a session's SFTP or SCP, or a file host). Files and whole
 * folders are selected with their checkboxes and copied to the other pane
 * through the transfer engine, which asks about each name conflict.
 */
export function FilePane({
  source,
  label,
  picker,
  dir,
  other,
  onCwd,
  onOpenFile,
}: {
  source: string;
  /** The pane's accessible name. */
  label: string;
  /** What the pane shows: a source picker, or the source's name. */
  picker: ReactNode;
  /** The first folder; the source's home when unset. */
  dir?: string;
  other?: OtherPane;
  onCwd: (cwd: string) => void;
  /** Opens a file in the editor (SFTP sessions). */
  onOpenFile?: (path: string) => void;
}) {
  const { push } = useToast();
  const { prompt, confirm } = usePrompt();
  const transfers = useTransferActions();
  const { menu, node: menuNode } = useContextMenu();
  const [kind, setKind] = useState("");
  const [cwd, setCwd] = useState("");
  const [pathInput, setPathInput] = useState("");
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  // A response that is no longer the latest is dropped, so a slow listing
  // cannot replace a newer one.
  const seq = useRef(0);

  const list = useCallback(
    async (path: string) => {
      const my = ++seq.current;
      setLoading(true);
      try {
        const res = await api.transferList(source, path);
        if (my !== seq.current) return;
        setEntries(res.sort(byName));
        setCwd(path);
        setPathInput(path);
        setSelected(new Set());
        setError("");
        onCwd(path);
      } catch (e) {
        if (my === seq.current) setError(api.errorInfo(e).message);
      } finally {
        if (my === seq.current) setLoading(false);
      }
    },
    [source, onCwd]
  );

  // A new source starts in the given folder, else its home.
  useEffect(() => {
    let live = true;
    setKind("");
    api
      .transferOpen(source)
      .then((s) => {
        if (!live) return;
        setKind(s.kind);
        list(dir || s.home || "/");
      })
      .catch((e) => live && (setError(api.errorInfo(e).message), setLoading(false)));
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [source]);

  // A transfer that finished in this folder shows its files.
  const landed = useTransferLanded();
  useEffect(() => {
    if (landed && landed.to === source && landed.toDir === cwd) list(cwd);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [landed]);

  const run = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
    } catch (e) {
      push("error", api.errorInfo(e).message);
    }
    list(cwd);
  };

  const picked = entries.filter((e) => selected.has(e.path));
  const toggle = (path: string, on: boolean) =>
    setSelected((s) => {
      const next = new Set(s);
      if (on) next.add(path);
      else next.delete(path);
      return next;
    });

  const copy = (items: DirEntry[]) => {
    if (!other || !items.length) return;
    const what = items.length === 1 ? items[0].name : t("files.items", { n: items.length });
    transfers.start(source, items.map((e) => e.path), other.source, other.cwd, t("files.copyLabel", { what, to: other.name, dir: other.cwd }));
  };
  const newFolder = async () => {
    const name = await prompt(t("files.newFolderPrompt"));
    if (name?.trim()) run(() => api.transferMkdir(source, cwd, name.trim()));
  };
  const rename = async (e: DirEntry) => {
    const name = await prompt(t("files.renamePrompt", { name: e.name }), e.name);
    if (name?.trim() && name !== e.name) run(() => api.transferRename(source, e.path, joinPath(parentPath(e.path), name.trim())));
  };
  const remove = async (items: DirEntry[]) => {
    if (!items.length) return;
    const what = items.length === 1 ? items[0].name : t("files.items", { n: items.length });
    if (!(await confirm(t("files.deleteConfirm", { what })))) return;
    // A folder must be empty; the source refuses one that is not.
    run(async () => {
      for (const e of items) await api.transferRemove(source, e.path, e.isDir);
    });
  };

  const CopyIcon = other?.side === "left" ? CopyLeftIcon : CopyRightIcon;
  const allOn = entries.length > 0 && selected.size === entries.length;

  return (
    <section aria-label={label} className="panel rounded-card flex flex-col min-h-0 min-w-0">
      {menuNode}
      <div className="h-11 px-3 border-b border-line flex items-center gap-2 shrink-0">
        <div className="flex-1 min-w-0">{picker}</div>
        {kind === "scp" && (
          <span className="text-xs text-muted" title={t("files.scpHint")}>
            {t("files.scp")}
          </span>
        )}
      </div>
      <div className="px-3 py-2 flex items-center gap-1 shrink-0">
        <IconButton label={t("files.up")} icon={ParentFolderIcon} disabled={!cwd || cwd === "/"} onClick={() => list(parentPath(cwd))} />
        <form
          className="flex-1 min-w-0"
          onSubmit={(e) => {
            e.preventDefault();
            list(pathInput.trim() || "/");
          }}
        >
          <Input
            className="w-full h-8 font-mono text-xs"
            aria-label={t("files.path")}
            spellCheck={false}
            value={pathInput}
            onChange={(e) => setPathInput(e.target.value)}
          />
        </form>
        <IconButton label={t("files.refresh")} icon={RefreshIcon} onClick={() => list(cwd)} />
        <IconButton label={t("files.newFolder")} icon={NewFolderIcon} disabled={!cwd} onClick={newFolder} />
      </div>
      <div className="px-3 pb-2 flex items-center gap-2 shrink-0 min-h-9">
        {other && (
          <Button size="sm" variant="primary" icon={CopyIcon} disabled={!picked.length} onClick={() => copy(picked)}>
            {t("files.copyTo", { name: other.name })}
          </Button>
        )}
        <Button size="sm" variant="ghost" icon={EditIcon} disabled={picked.length !== 1} onClick={() => rename(picked[0])}>
          {t("files.rename")}
        </Button>
        <Button size="sm" variant="ghost" icon={DeleteIcon} disabled={!picked.length} onClick={() => remove(picked)}>
          {t("files.delete")}
        </Button>
        <span className="ml-auto text-xs text-muted tabular-nums" aria-live="polite">
          {picked.length ? t("files.selected", { n: picked.length }) : ""}
        </span>
      </div>
      <div className="flex-1 min-h-0 overflow-auto border-t border-line">
        {error ? (
          <div role="alert" className="p-4 flex flex-col gap-2 items-start">
            <p className="text-sm text-danger allow-select break-words">{error}</p>
            <Button size="sm" icon={RefreshIcon} onClick={() => list(cwd || "/")}>
              {t("connect.retry")}
            </Button>
          </div>
        ) : loading && !entries.length ? (
          <div className="flex items-center justify-center h-32">
            <Spinner />
          </div>
        ) : entries.length === 0 ? (
          <EmptyState icon={FolderIcon}>{t("files.empty")}</EmptyState>
        ) : (
          <table className="w-full table-fixed text-sm" aria-busy={loading}>
            <thead className="sticky top-0 bg-surface text-left text-xs text-muted">
              <tr>
                <th className="w-9 px-3 py-1.5">
                  <input
                    type="checkbox"
                    className="accent-accent size-4 align-middle"
                    aria-label={t("files.selectAll")}
                    checked={allOn}
                    onChange={(e) => setSelected(new Set(e.target.checked ? entries.map((x) => x.path) : []))}
                  />
                </th>
                <th className="px-2 py-1.5 font-medium">{t("files.name")}</th>
                <th className="w-20 px-2 py-1.5 font-medium text-right">{t("files.size")}</th>
                <th className="w-40 px-2 py-1.5 font-medium hidden xl:table-cell">{t("files.modified")}</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => {
                const Icon = e.isDir ? FolderIcon : FileIcon;
                const opens = e.isDir || !!onOpenFile;
                return (
                  <tr
                    key={e.path}
                    className={selected.has(e.path) ? "bg-active" : "hover:bg-hover"}
                    onContextMenu={(ev) => {
                      const items = selected.has(e.path) ? picked : [e];
                      menu([
                        ...(other ? [{ label: t("files.copyTo", { name: other.name }), onClick: () => copy(items) }] : []),
                        ...(e.isDir || onOpenFile
                          ? [{ label: e.isDir ? t("files.open") : t("files.edit"), onClick: () => (e.isDir ? list(e.path) : onOpenFile!(e.path)) }]
                          : []),
                        { label: t("files.rename"), onClick: () => rename(e) },
                        { label: t("files.delete"), danger: true, onClick: () => remove(items) },
                      ])(ev);
                    }}
                  >
                    <td className="px-3 py-1">
                      <input
                        type="checkbox"
                        className="accent-accent size-4 align-middle"
                        aria-label={t("files.select", { name: e.name })}
                        checked={selected.has(e.path)}
                        onChange={(ev) => toggle(e.path, ev.target.checked)}
                      />
                    </td>
                    <td className="px-2 py-1 truncate">
                      <Icon className="inline size-4 mr-1.5 -mt-0.5 text-muted" aria-hidden />
                      {opens ? (
                        <button
                          type="button"
                          className={`hover:underline text-left rounded-chip ${focusRing}`}
                          title={e.isDir ? t("files.open") : t("files.edit")}
                          onClick={() => (e.isDir ? list(e.path) : onOpenFile!(e.path))}
                        >
                          {e.name}
                        </button>
                      ) : (
                        e.name
                      )}
                    </td>
                    <td className="px-2 py-1 text-right tabular-nums text-muted whitespace-nowrap">{e.isDir ? "" : humanSize(e.size)}</td>
                    <td className="px-2 py-1 text-muted whitespace-nowrap hidden xl:table-cell">
                      {e.mtime ? new Date(e.mtime * 1000).toLocaleString() : ""}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}
