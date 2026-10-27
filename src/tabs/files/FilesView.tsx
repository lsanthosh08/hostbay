import { useCallback, useEffect, useRef, useState } from "react";
import * as api from "../../api";
import { isFileHost, type Host } from "../../api";
import { useConnect } from "../../connect/useConnect";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { useSessions, type Connection } from "../../state/sessions";
import { Select } from "../../ui/Select";
import { FilePane } from "./FilePane";

/** What a pane lists. `owned`: a file host this pane signed in to itself,
 *  signed out again when the pane moves on. */
export type PaneSource = { source: string; name: string; owned?: boolean };

export const localPane = (): PaneSource => ({ source: api.transferLocal, name: t("files.local") });

/** Sessions a pane can list: an SSH session's SFTP (or SCP), or a file host. */
const listable = (c: Connection) => !c.dialing && !c.files && !c.term && (isFileHost(c.host) || (!c.sshless && c.host.kind !== "telnet"));

/** Picks a pane's source: this computer, an open session, or a saved file
 *  host, which it signs in to (asking to trust a new certificate first). */
function SourcePicker({ side, value, onPick }: { side: "left" | "right"; value: PaneSource; onPick: (p: PaneSource) => void }) {
  const { sessions } = useSessions();
  const { hosts } = useHosts();
  const { fail, dialog } = useConnect();
  const open = sessions.filter(listable);
  const saved = hosts.filter((h) => isFileHost(h) && !open.some((c) => c.host.id === h.id));

  // Each pick, and closing the view, takes the next number: a sign-in that
  // one overtook signs out again, since no pane owns its source.
  const pick = useRef(0);
  useEffect(
    () => () => {
      pick.current++;
    },
    []
  );

  const signIn = async (h: Host) => {
    const seq = ++pick.current;
    try {
      const src = await api.transferConnect(h.id);
      if (seq !== pick.current) return void api.transferDisconnect(src.id).catch(() => {});
      onPick({ source: src.id, name: h.alias, owned: true });
    } catch (e) {
      if (seq !== pick.current) return;
      fail(e, () => signIn(h), { host: h });
    }
  };

  return (
    <>
      {dialog}
      <Select
        compact
        className="max-w-full"
        aria-label={side === "left" ? t("files.leftSource") : t("files.rightSource")}
        value={value.source}
        onChange={(e) => {
          const v = e.target.value;
          pick.current++;
          if (v === api.transferLocal) return onPick(localPane());
          const c = open.find((x) => x.sessionId === v);
          if (c) return onPick({ source: c.sessionId, name: c.host.alias });
          const h = saved.find((x) => `host:${x.id}` === v);
          if (h) signIn(h);
        }}
      >
        <option value={api.transferLocal}>{t("files.local")}</option>
        {/* A file host this pane signed in to is not a session tab. */}
        {value.owned && <option value={value.source}>{value.name}</option>}
        {open.length > 0 && (
          <optgroup label={t("files.openSessions")}>
            {open.map((c) => (
              <option key={c.sessionId} value={c.sessionId}>
                {c.host.alias}
              </option>
            ))}
          </optgroup>
        )}
        {saved.length > 0 && (
          <optgroup label={t("files.savedHosts")}>
            {saved.map((h) => (
              <option key={h.id} value={`host:${h.id}`}>
                {h.alias}
              </option>
            ))}
          </optgroup>
        )}
      </Select>
    </>
  );
}

/** Signs out of a file host a pane signed in to, once the pane leaves it. */
function useRelease(p: PaneSource) {
  useEffect(() => {
    if (!p.owned) return;
    return () => {
      api.transferDisconnect(p.source).catch(() => {});
    };
  }, [p.source, p.owned]);
}

/**
 * The Files view: two panes side by side, each over any source, with the
 * selection copied across by the transfer engine (R38–R44). `left` is fixed
 * for a session's own view; the Files tab lets both panes choose.
 */
export function FilesView({
  left: initialLeft,
  leftDir,
  fixedLeft = false,
  onOpenFile,
  className = "p-3",
}: {
  left: PaneSource;
  leftDir?: string;
  fixedLeft?: boolean;
  /** Opens a file of the left pane in the editor (SFTP sessions). */
  onOpenFile?: (path: string) => void;
  className?: string;
}) {
  const [left, setLeft] = useState(initialLeft);
  const [right, setRight] = useState(localPane);
  const [leftCwd, setLeftCwd] = useState("");
  const [rightCwd, setRightCwd] = useState("");
  useRelease(left);
  useRelease(right);
  const onLeftCwd = useCallback((d: string) => setLeftCwd(d), []);
  const onRightCwd = useCallback((d: string) => setRightCwd(d), []);
  const name = (p: PaneSource) => <span className="text-sm font-medium truncate block">{p.name}</span>;

  return (
    <div className={`flex-1 min-h-0 grid grid-cols-1 md:grid-cols-2 gap-3 ${className}`}>
      <FilePane
        key={`l:${left.source}`}
        source={left.source}
        label={t("files.leftPane", { name: left.name })}
        picker={fixedLeft ? name(left) : <SourcePicker side="left" value={left} onPick={(p) => (setLeftCwd(""), setLeft(p))} />}
        dir={leftDir}
        other={rightCwd ? { source: right.source, cwd: rightCwd, name: right.name, side: "right" } : undefined}
        onCwd={onLeftCwd}
        onOpenFile={onOpenFile}
      />
      <FilePane
        key={`r:${right.source}`}
        source={right.source}
        label={t("files.rightPane", { name: right.name })}
        picker={<SourcePicker side="right" value={right} onPick={(p) => (setRightCwd(""), setRight(p))} />}
        other={leftCwd ? { source: left.source, cwd: leftCwd, name: left.name, side: "left" } : undefined}
        onCwd={onRightCwd}
      />
    </div>
  );
}
