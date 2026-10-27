import { useCallback, useEffect, useState } from "react";
import * as api from "../api";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { BackIcon, FileIcon, FolderIcon, SymlinkIcon } from "../ui/icons";
import { useAsyncToast } from "../ui/toast";

// Read-only file explorer inside a running container: ls -lA and head run
// inside it through the session's engine (exec, no shell), over SSH or on
// this computer. View is browse + read a text file; no writes.

type Entry = { name: string; isDir: boolean; size: string; kind: string };

// Parse `ls -lA` output into entries. Line shape:
//   drwxr-xr-x 2 root root 4096 Jan  1 00:00 name
// The name is everything after the 8th whitespace-delimited field (handles
// spaces in names; symlinks show "name -> target", we keep the left side).
function parseLs(out: string): Entry[] {
  const rows: Entry[] = [];
  for (const line of out.split("\n")) {
    if (!line.trim() || line.startsWith("total ")) continue;
    const m = line.match(/^([dlbcps-])\S*\s+\S+\s+\S+\s+\S+\s+(\S+)\s+\S+\s+\S+\s+\S+\s+(.*)$/);
    if (!m) continue;
    const [, typeCh, size, rawName] = m;
    let name = rawName;
    if (typeCh === "l") name = rawName.split(" -> ")[0]; // symlink: keep the link name
    if (name === "." || name === "..") continue;
    rows.push({
      name,
      isDir: typeCh === "d",
      size,
      kind: typeCh === "l" ? "link" : typeCh === "d" ? "dir" : "file",
    });
  }
  // Dirs first, then files, each alphabetical.
  rows.sort((a, b) =>
    a.isDir !== b.isDir ? (a.isDir ? -1 : 1) : a.name.localeCompare(b.name)
  );
  return rows;
}

export function ContainerFiles({
  sessionId,
  containerId,
  name,
  onClose,
}: {
  sessionId: string;
  containerId: string;
  name: string;
  onClose: () => void;
}) {
  const run = useAsyncToast();
  const [path, setPath] = useState("/");
  const [entries, setEntries] = useState<Entry[]>([]);
  const [loading, setLoading] = useState(false);
  const [viewer, setViewer] = useState<{ name: string; text: string } | null>(null);

  const load = useCallback(
    async (p: string) => {
      setLoading(true);
      const r = await run(() => api.containerListDir(sessionId, containerId, p));
      setLoading(false);
      if (r === undefined) return;
      setEntries(parseLs(r));
      setPath(p);
    },
    [sessionId, containerId, run]
  );

  useEffect(() => {
    void load("/");
  }, [load]);

  const goInto = (e: Entry) => {
    if (e.isDir || e.kind === "link") {
      const next = path.endsWith("/") ? path + e.name : `${path}/${e.name}`;
      void load(next);
    } else {
      void openFile(e);
    }
  };

  const openFile = async (e: Entry) => {
    const full = path.endsWith("/") ? path + e.name : `${path}/${e.name}`;
    // Cap the read so a huge/binary file can't flood the UI.
    const r = await run(() => api.containerReadFile(sessionId, containerId, full));
    if (r === undefined) return;
    setViewer({ name: e.name, text: r });
  };

  // Breadcrumb segments -> absolute paths.
  const segments = path.split("/").filter(Boolean);
  const crumbPath = (i: number) => "/" + segments.slice(0, i + 1).join("/");

  return (
    <Dialog title={`Files — ${name}`} size="xl" onClose={onClose} bodyClassName="flex flex-col">
      {/* Breadcrumbs */}
      <div className="flex items-center gap-1 px-4 py-2 text-xs flex-wrap border-b border-line">
        <button className="hover:text-accent" onClick={() => load("/")}>
          /
        </button>
        {segments.map((seg, i) => (
          <span key={i} className="flex items-center gap-1">
            <span className="opacity-40">/</span>
            <button className="hover:text-accent" onClick={() => load(crumbPath(i))}>
              {seg}
            </button>
          </span>
        ))}
        {loading && <span className="ml-2 opacity-50">loading…</span>}
      </div>

      {viewer ? (
        <div className="flex-1 min-h-0 flex flex-col">
          <div className="flex items-center gap-2 px-4 py-2 text-xs border-b border-line">
            <span className="font-mono truncate">{viewer.name}</span>
            <Button variant="ghost" icon={BackIcon} className="ml-auto" onClick={() => setViewer(null)}>
              Back
            </Button>
          </div>
          <pre className="flex-1 min-h-0 overflow-auto p-4 text-xs font-mono whitespace-pre-wrap break-words">
            {viewer.text || "(empty)"}
          </pre>
        </div>
      ) : (
        <div className="flex-1 min-h-0 overflow-auto">
          <table className="w-full text-sm">
            <tbody>
              {entries.length === 0 && !loading && (
                <tr>
                  <td className="px-4 py-6 text-center opacity-50">empty directory</td>
                </tr>
              )}
              {entries.map((e) => (
                <tr
                  key={e.name}
                  className="hover:bg-hover cursor-pointer"
                  onDoubleClick={() => goInto(e)}
                >
                  <td className="px-4 py-1.5 flex items-center gap-2">
                    {e.isDir ? (
                      <FolderIcon className="size-4 shrink-0 text-muted" aria-hidden />
                    ) : e.kind === "link" ? (
                      <SymlinkIcon className="size-4 shrink-0 text-muted" aria-hidden />
                    ) : (
                      <FileIcon className="size-4 shrink-0 text-muted" aria-hidden />
                    )}
                    <span className="truncate">{e.name}</span>
                  </td>
                  <td className="px-4 py-1.5 text-right font-mono text-xs opacity-60 w-24">
                    {e.isDir ? "" : e.size}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="px-4 py-2 text-[11px] opacity-40 border-t border-line">
        Double-click a folder to open, a file to view (first 256 KB). Read-only.
      </div>
    </Dialog>
  );
}
