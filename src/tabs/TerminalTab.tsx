import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import * as api from "../api";
import { PathList } from "../components/TransferPanel";
import { t } from "../i18n";
import type { Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { IconButton } from "../ui/IconButton";
import { AddIcon, CloseIcon, LogsIcon, RefreshIcon, StopIcon } from "../ui/icons";
import { Badge } from "../ui/kit";
import { Tabs } from "../ui/Tabs";
import { onThemeChange } from "../ui/theme";
import { useToast } from "../ui/toast";
import { terminalPalette, terminalTheme } from "../theme/terminal";
import { termFontSize } from "../shell/settings/Terminal";
import { backgroundImage, loadBackground, onSettingsChange, settings } from "../state/settings";
import { humanSize } from "./files/paths";
import { activeTerminal, clearActiveTerminal, notifyActiveTerminal, setActiveTerminal, type ActiveTerm } from "./terminal/active";
import { InputLine } from "./terminal/inputLine";
import { accepts, AI_PAUSE_MS, completionRest, historySuggestion, mayAskAI } from "./terminal/suggestions";
import {
  startZmodem,
  type ZmodemConflict,
  type ZmodemDirection,
  type ZmodemProgress,
  type ZmodemResult,
} from "./zmodem";

/** e.g. "9600 8N1", how serial line settings are usually written. */
const serialLine = (s: api.SerialSettings) =>
  `${s.baud} ${s.dataBits}${s.parity[0].toUpperCase()}${s.stopBits}${s.localEcho ? " · local echo" : ""}`;

/** A Zmodem transfer in a pane, from `sz` or `rz` until it is dismissed. */
type ZmodemView = {
  direction: ZmodemDirection;
  progress: ZmodemProgress | null;
  ask: { name: string; answer: (c: ZmodemConflict) => void } | null;
  result: ZmodemResult | null;
  cancel: () => void;
};

const ZM_TITLE = { download: "zmodem.download", upload: "zmodem.upload" } as const;

/** Over the bottom of the terminal: the file moving with Cancel, the name
 *  conflict question, then what was saved where, skipped or failed. */
function ZmodemStrip({ view, onDismiss }: { view: ZmodemView; onDismiss: () => void }) {
  const { direction, progress: p, ask, result: r } = view;
  const title = t(ZM_TITLE[direction]);
  const pct = p?.size ? Math.min(100, Math.round((p.bytes / p.size) * 100)) : 0;
  return (
    <section aria-label={title} className="absolute inset-x-2 bottom-2 panel rounded-card shadow-lg p-3 flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <h3 className="text-sm font-semibold flex-1 min-w-0 truncate">{title}</h3>
        {r ? (
          <IconButton size="sm" label={t("transfer.dismiss")} icon={CloseIcon} onClick={onDismiss} />
        ) : (
          <>
            <span className="text-xs text-muted">{t("zmodem.cancelHint")}</span>
            <Button size="sm" variant="ghost" icon={StopIcon} onClick={view.cancel}>
              {t("transfer.cancel")}
            </Button>
          </>
        )}
      </div>
      {!r && !ask && !p && <p className="text-xs text-muted">{t("zmodem.waiting")}</p>}
      {!r && !ask && p && (
        <>
          <div
            role="progressbar"
            aria-label={p.name}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={pct}
            className="h-1.5 rounded-full bg-control-bg overflow-hidden"
          >
            <div className="h-full rounded-full bg-accent" style={{ width: `${pct}%` }} />
          </div>
          <p className="text-xs text-muted tabular-nums flex gap-3 min-w-0">
            <span className="truncate font-mono">{p.name}</span>
            <span className="shrink-0">
              {p.size
                ? t("zmodem.progress", { bytes: humanSize(p.bytes), size: humanSize(p.size), pct })
                : humanSize(p.bytes)}
            </span>
          </p>
        </>
      )}
      {!r && ask && (
        <div role="group" aria-label={t("transfer.conflictTitle")} className="flex flex-col gap-2 rounded-control bg-warning-soft p-2.5">
          <p className="text-sm allow-select break-words">{t("zmodem.conflict", { name: ask.name })}</p>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={() => ask.answer("overwrite")}>
              {t("transfer.overwrite")}
            </Button>
            <Button size="sm" onClick={() => ask.answer("skip")}>
              {t("transfer.skip")}
            </Button>
            {/* The question takes focus from the terminal; Keep both loses nothing. */}
            <Button size="sm" autoFocus onClick={() => ask.answer("rename")}>
              {t("transfer.rename")}
            </Button>
          </div>
        </div>
      )}
      {r && (
        <div role="status" className="flex flex-col gap-1">
          {r.outcome === "cancelled" && <p className="text-sm text-muted">{t("zmodem.cancelled")}</p>}
          {r.outcome === "stopped" && (
            <p className="text-sm text-danger allow-select break-words">{t("zmodem.stopped", { error: r.error ?? "" })}</p>
          )}
          <PathList
            title={
              direction === "download"
                ? t("zmodem.savedIn", { n: r.done.length, folder: r.folder })
                : t("zmodem.sent", { n: r.done.length })
            }
            items={r.done.map((path) => ({ path }))}
          />
          <PathList title={t("transfer.skippedList", { n: r.skipped.length })} items={r.skipped.map((path) => ({ path }))} />
          <PathList title={t("transfer.failedList", { n: r.failed.length })} items={r.failed.map((f) => ({ path: f.name, error: f.error }))} />
        </div>
      )}
    </section>
  );
}

let nextId = 1;
const makeId = () => `t${nextId++}`;

/** One xterm + one pty, owned for the component's lifetime. */
function TermPane({ conn }: { conn: Connection }) {
  const { push } = useToast();
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const ptyRef = useRef<string | null>(null);
  // The terminal's log and local echo, from Go (KTD39).
  const [tools, setTools] = useState<api.TerminalState | null>(null);
  // Bumping `gen` re-runs the effect => opens a fresh pty in the same pane.
  const [gen, setGen] = useState(0);
  const [closed, setClosed] = useState(false);
  const [zmodem, setZmodem] = useState<ZmodemView | null>(null);
  // A Mosh tab that failed can fall back to a plain SSH shell on the session.
  const [useSsh, setUseSsh] = useState(false);
  // The terminal theme and background image, redrawn when they change.
  const [, redraw] = useState(0);
  useEffect(() => {
    loadBackground();
    return onSettingsChange(() => redraw((n) => n + 1));
  }, []);
  const s = settings();
  const image = backgroundImage();
  const kind = useSsh ? "ssh" : conn.host.kind;
  // An SSH shell rides the session's shared connection: after a drop the
  // session's banner rebuilds it (KTD5), and a closed shell reopens here once
  // the session is live again. Other kinds reconnect per pane.
  const shared = !conn.term && (kind === "ssh" || kind === "rdp" || !kind);
  const [wasDropped, setWasDropped] = useState(false);
  useEffect(() => {
    if (!shared) return;
    if (conn.status === "error") setWasDropped(true);
    else if (conn.status === "live" && wasDropped) {
      setWasDropped(false);
      if (closed) setGen((g) => g + 1);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conn.status]);

  useEffect(() => {
    const el = hostRef.current;
    if (!el) return;
    setClosed(false);
    setZmodem(null);
    setTools(null);

    const look = () => terminalTheme(settings(), !!backgroundImage());
    const term = new Terminal({
      theme: look(),
      allowTransparency: true, // for the background image (KTD41)
      fontFamily: "monospace",
      fontSize: termFontSize(),
      cursorBlink: true,
      convertEol: true,
    });
    termRef.current = term;
    const restyle = () => (term.options.theme = look());
    const offTheme = onThemeChange(restyle);
    const offSettings = onSettingsChange(restyle);

    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(el);
    fit.fit();

    let ptyId: string | null = null;
    let transfer: { cancel(): void } | null = null; // a Zmodem transfer owns the keyboard while it runs
    const unlisten: (() => void)[] = [];
    const stopListening = () => unlisten.splice(0).forEach((f) => f());
    let disposed = false;
    // The quick-commands and history panels act on the pane focused last.
    const active: ActiveTerm = { sessionId: conn.sessionId, ptyId: () => ptyId, bracketed: () => term.modes.bracketedPasteMode, focus: () => term.focus() };
    term.textarea?.addEventListener("focus", () => setActiveTerminal(active));

    // Completion (R62): ghost text after the cursor from the host's
    // history or, when turned on, the AI. Right Arrow at the end of the
    // line accepts it, only while it shows; Tab always goes to the shell.
    const ghostEl = document.createElement("div");
    ghostEl.setAttribute("aria-hidden", "true");
    ghostEl.className = "absolute pointer-events-none whitespace-pre overflow-hidden opacity-50";
    el.appendChild(ghostEl);
    let input: InputLine | null = null;
    let localEcho = true;
    let history: string[] = [];
    let aiOn = false;
    let ai = { line: "", completed: "" };
    let aiTimer: ReturnType<typeof setTimeout> | undefined;
    let ghost = "";
    const readAIOn = () => api.settingsGetLocal().then((l) => (aiOn = !!l.aiSuggestions), () => {});
    const showGhost = () => {
      const buf = term.buffer.active;
      // Not on the alternate screen (vim, less, top) or during a Zmodem transfer.
      const cur = !transfer && buf.type === "normal" && buf.viewportY === buf.baseY ? input?.current(buf) : null;
      ghost = cur && cur.echoed && cur.atEnd ? historySuggestion(cur.text, history) || (ai.line === cur.text ? completionRest(cur.text, ai.completed) : "") : "";
      ghostEl.textContent = ghost;
      if (!ghost) return;
      const screen = term.element?.querySelector(".xterm-screen") as HTMLElement | null;
      if (!screen) return;
      const w = screen.clientWidth / term.cols;
      const h = screen.clientHeight / term.rows;
      const at = screen.getBoundingClientRect();
      const origin = el.getBoundingClientRect();
      Object.assign(ghostEl.style, {
        left: `${at.left - origin.left + buf.cursorX * w}px`,
        top: `${at.top - origin.top + buf.cursorY * h}px`,
        width: `${(term.cols - buf.cursorX) * w}px`,
        height: `${h}px`,
        lineHeight: `${h}px`,
        font: `${term.options.fontSize}px ${term.options.fontFamily}`,
        color: term.options.theme?.foreground ?? "",
      });
    };
    term.onRender(showGhost);
    term.textarea?.addEventListener("focus", readAIOn);
    const askAI = () => {
      clearTimeout(aiTimer);
      aiTimer = setTimeout(() => {
        const cur = input?.current(term.buffer.active);
        // Never from a line typed with echo off (KTD39).
        const normal = term.buffer.active.type === "normal";
        if (!ptyId || transfer || !cur || !cur.atEnd || !mayAskAI({ enabled: aiOn, localEcho, echoed: cur.echoed, normal, line: cur.text })) return;
        if (historySuggestion(cur.text, history)) return;
        const line = cur.text;
        api.aiSuggest(ptyId, line).then(
          (completed) => {
            ai = { line, completed };
            showGhost();
          },
          () => {}
        );
      }, AI_PAUSE_MS);
    };
    term.attachCustomKeyEventHandler((e) => {
      if (transfer || term.buffer.active.type !== "normal") return true; // the transfer owns the keyboard
      if (e.type !== "keydown" || !accepts(e, ghost, !!input?.current(term.buffer.active)?.atEnd)) return true;
      if (ptyId) {
        input?.onData(ghost, term.buffer.active);
        api.ptyWrite(ptyId, ghost).catch(() => {});
      }
      ghost = ghostEl.textContent = "";
      return false;
    });

    (async () => {
      // The backend reopens the shell on the session's shared connection:
      // a new channel while it is alive, one shared rebuild after a drop. The
      // session ID stays valid either way, so no second connect is needed.
      try {
        const id = await (conn.term?.kind === "local"
          ? api.ptyOpenLocal(conn.sessionId, conn.term.shellId, term.cols, term.rows)
          : conn.term?.kind === "serial"
          ? api.ptyOpenSerial(conn.sessionId, conn.term.serial)
          : conn.term?.kind === "container"
          ? api.ptyOpenContainer(
              conn.sessionId,
              conn.term.engineSession,
              conn.term.containerId,
              conn.term.shell,
              conn.term.follow,
              term.cols,
              term.rows
            )
          : conn.term?.kind === "pod"
          ? api.kubernetesExec(conn.term.clusterId, conn.term.namespace, conn.term.pod, conn.term.container, conn.term.command, term.cols, term.rows)
          : kind === "telnet"
          ? api.ptyOpenTelnet(conn.sessionId, conn.host.id, term.cols, term.rows)
          : kind === "tcp"
          ? api.ptyOpenTCP(conn.sessionId, conn.host.id)
          : kind === "mosh"
          ? api.ptyOpenMosh(conn.sessionId, conn.host.id, term.cols, term.rows)
          : api.ptyOpen(conn.sessionId, term.cols, term.rows));
        if (disposed) {
          await api.ptyClose(id).catch(() => {});
          return;
        }
        ptyId = id;
        ptyRef.current = id;
        const state = await api.terminalState(id).catch(() => null);
        setTools(state);
        // History is read at Enter, and is off where Go adds local echo.
        input = state && !state.localEcho ? new InputLine() : null;
        localEcho = !state || state.localEcho;
        if (input) {
          api.terminalHistory(id).then((h) => (history = h), () => {});
          readAIOn();
        }
        if (activeTerminal() === active) notifyActiveTerminal(); // a follow-active AI conversation now has its ID
        unlisten.push(await api.onPtyData(id, (d) => term.write(d)));
        unlisten.push(
          await api.onTerminalNotice(id, (message) => {
            term.write(`\r\n\x1b[33m[${message}]\x1b[0m\r\n`);
            api.terminalState(id).then(setTools, () => {});
          })
        );
        unlisten.push(
          await api.onPtyClosed(id, (reason) => {
            term.write(`\r\n\x1b[33m[${reason}]\x1b[0m\r\n`);
            ptyId = null;
            ptyRef.current = null;
            setClosed(true);
          })
        );
        unlisten.push(
          await api.onZmodem(id, (direction) => {
            if (transfer) return;
            const update = (v: Partial<ZmodemView>) => setZmodem((z) => z && { ...z, ...v });
            transfer = startZmodem(id, direction, term, {
              progress: (progress) => update({ progress }),
              ask: (name) =>
                new Promise((resolve) =>
                  update({
                    ask: {
                      name,
                      answer: (c) => {
                        update({ ask: null });
                        term.focus();
                        resolve(c);
                      },
                    },
                  })
                ),
              finish: (result) => {
                transfer = null;
                update({ result, ask: null, progress: null });
              },
            });
            setZmodem({ direction, progress: null, ask: null, result: null, cancel: transfer.cancel });
          })
        );
        if (disposed) return stopListening();
        term.onData((d) => {
          // Keystrokes would corrupt the transfer; Ctrl+C cancels it.
          if (transfer) {
            if (d.includes("\x03")) transfer.cancel();
            return;
          }
          if (!ptyId) return;
          const line = input?.onData(d, term.buffer.active);
          if (line) {
            api.addHistory(ptyId, line).catch(() => {});
            history = [...history, line];
          }
          ai = { line: "", completed: "" };
          clearTimeout(aiTimer);
          if (input && !/[\r\x03\x04]/.test(d)) askAI();
          api.ptyWrite(ptyId, d).catch(() => {});
        });
        // Subscribed: now let the held banner and prompt through.
        await api.ptyAttach(id);
      } catch (e) {
        term.write(`\r\n\x1b[31m${api.errorInfo(e).message}\x1b[0m\r\n`);
        setClosed(true);
      }
    })();

    const doResize = () => {
      fit.fit();
      if (ptyId) api.ptyResize(ptyId, term.cols, term.rows).catch(() => {});
    };
    window.addEventListener("resize", doResize);
    const ro = new ResizeObserver(doResize);
    ro.observe(el);

    return () => {
      disposed = true;
      clearTimeout(aiTimer);
      clearActiveTerminal(active);
      ptyRef.current = null;
      offTheme();
      offSettings();
      window.removeEventListener("resize", doResize);
      ro.disconnect();
      stopListening();
      if (ptyId) api.ptyClose(ptyId).catch(() => {});
      term.dispose();
      ghostEl.remove();
      termRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gen]);

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-2 px-2 py-1">
        <Button variant="ghost" onClick={() => termRef.current?.clear()}>
          Clear
        </Button>
        {closed && !(shared && conn.status === "error") && (
          <Button variant="primary" icon={RefreshIcon} onClick={() => setGen((g) => g + 1)}>
            Reconnect
          </Button>
        )}
        {closed && kind === "mosh" && (
          <Button
            variant="ghost"
            onClick={() => {
              setUseSsh(true);
              setGen((g) => g + 1);
            }}
          >
            Open SSH terminal instead
          </Button>
        )}
        {closed && <span className="text-xs text-danger">session closed</span>}
        <div className="flex-1" />
        {tools && !closed && (
          <Button
            variant={tools.logging ? "primary" : "ghost"}
            icon={LogsIcon}
            aria-pressed={tools.logging}
            title={tools.logging ? t("terminal.logOn", { path: tools.logPath ?? "" }) : t("terminal.logOff")}
            onClick={() =>
              ptyRef.current &&
              api.terminalSetLogging(ptyRef.current, !tools.logging).then(setTools, (e) => push("error", api.errorInfo(e).message))
            }
          >
            {t("terminal.log")}
          </Button>
        )}
      </div>
      <div className="relative flex-1 min-h-0">
        <div
          className="relative h-full overflow-hidden rounded-control"
          style={{ background: terminalPalette(s.terminalTheme, s.customThemes)?.background ?? "var(--term-bg)" }}
        >
          {image && (
            <div
              aria-hidden
              className="absolute inset-0 bg-cover bg-center pointer-events-none"
              style={{ backgroundImage: `url("${image}")`, opacity: (s.backgroundOpacity || 30) / 100 }}
            />
          )}
          <div ref={hostRef} className="relative h-full" />
        </div>
        {zmodem && (
          <ZmodemStrip
            view={zmodem}
            onDismiss={() => {
              setZmodem(null);
              termRef.current?.focus();
            }}
          />
        )}
      </div>
    </div>
  );
}

export function TerminalTab({ conn }: { conn: Connection }) {
  const { push } = useToast();
  const [tabs, setTabs] = useState<{ id: string }[]>(() => [{ id: makeId() }]);
  const [active, setActive] = useState<string>(() => tabs[0].id);

  const addTab = () => {
    if (!conn.sessionId) {
      push("error", "No active session");
      return;
    }
    const t = { id: makeId() };
    setTabs((prev) => [...prev, t]);
    setActive(t.id);
  };

  const closeTab = (id: string) => {
    setTabs((prev) => {
      const next = prev.filter((t) => t.id !== id);
      if (id === active && next.length) setActive(next[next.length - 1].id);
      return next;
    });
  };

  return (
    <div className="flex flex-col h-full p-4 gap-3">
      <div className="flex items-center gap-2">
        {/* The session toolbar shows the host and address. */}
        {conn.term?.kind === "serial" && <span className="text-xs text-muted">{serialLine(conn.term.serial)}</span>}
        {conn.host.kind === "telnet" && (
          <Badge tone="warn">Telnet is unencrypted: everything you type, passwords included, is sent in clear text</Badge>
        )}
        {conn.host.kind === "tcp" && <Badge tone="warn">{t("hostForm.tcpWarning")}</Badge>}
        {conn.host.kind === "mosh" && <Badge tone="neutral">Mosh</Badge>}
        <div className="flex-1" />
        {/* A serial port opens once; a second pane would find it busy. */}
        {conn.term?.kind !== "serial" && (
          <Button variant="primary" icon={AddIcon} onClick={addTab}>
            New tab
          </Button>
        )}
      </div>

      <Tabs
        label="Shells"
        value={active}
        onChange={setActive}
        items={tabs.map((t, i) => ({ id: t.id, label: `Shell ${i + 1}`, onClose: () => closeTab(t.id) }))}
      />

      <div className="flex-1 min-h-0 panel rounded-card p-1">
        {tabs.length === 0 ? (
          <div className="flex items-center justify-center h-full text-sm opacity-60">
            No terminals. Click “New tab”.
          </div>
        ) : (
          // Keep every pane mounted (each owns its pty); only show the active one.
          tabs.map((t) => (
            <div key={t.id} className={t.id === active ? "h-full" : "hidden"}>
              <TermPane conn={conn} />
            </div>
          ))
        )}
      </div>
    </div>
  );
}
