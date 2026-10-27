import { useEffect, useReducer, useRef, useState } from "react";
import * as api from "../../api";
import type { AIMode, AIProvider, AISkill, AITarget } from "../../api";
import { t } from "../../i18n";
import { PanelHeader } from "../../shell/SidePanel";
import { Button } from "../../ui/Button";
import { EmptyState } from "../../ui/EmptyState";
import { IconButton } from "../../ui/IconButton";
import { AIIcon, CloseIcon, MaximizeIcon, RestoreSizeIcon, StopIcon } from "../../ui/icons";
import { Select } from "../../ui/Select";
import { useToast } from "../../ui/toast";
import { conversations, isSuperseded } from "./conversation";
import { errorHint, initialChat, reduce, RichText, type Chat, type ChatAction, type Item } from "./messages";
import { pickSlash, SlashMenu, isSlashDraft, slashMatches } from "./SlashMenu";

const FOLLOW = ""; // the target picker's "follow the focused terminal"

const MIN_WIDTH = 320;
const DEFAULT_WIDTH = 384;
const KEEP_FOR_TERMINAL = 480; // what the rest of the window keeps when the sidebar is widest
const WIDTH_KEY = "ai.sidebar.width";
const WIDE_KEY = "ai.sidebar.wide";

const maxWidth = () => Math.max(MIN_WIDTH, window.innerWidth - KEEP_FOR_TERMINAL);

// A per-viewer convenience: the page works without it.
function remembered(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
function remember(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // not remembered; the width still applies this session
  }
}

/** The sidebar's width: dragged on its left edge or keyed with the arrow
 *  keys, or "wide" to read a long reply or a review at the most the window
 *  allows. Both are remembered. */
function useSidebarWidth() {
  const [saved, setSaved] = useState(() => {
    const n = Number(remembered(WIDTH_KEY));
    return Number.isFinite(n) && n >= MIN_WIDTH ? n : DEFAULT_WIDTH;
  });
  const [wide, setWide] = useState(() => remembered(WIDE_KEY) === "1");
  const [max, setMax] = useState(maxWidth);
  useEffect(() => {
    const onResize = () => setMax(maxWidth());
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  const width = wide ? max : Math.min(Math.max(saved, MIN_WIDTH), max);
  const set = (w: number) => {
    const next = Math.round(Math.min(Math.max(w, MIN_WIDTH), maxWidth()));
    setWide(false);
    setSaved(next);
    remember(WIDE_KEY, "0");
    remember(WIDTH_KEY, String(next));
  };
  const toggleWide = () => {
    setWide(!wide);
    remember(WIDE_KEY, wide ? "0" : "1");
  };
  return { width, max, wide, set, toggleWide };
}

/** The AI sidebar (R60): one conversation with a provider, targeting a
 *  chosen terminal or the focused one. Go runs the loop; every command it
 *  proposes waits in the app-wide approval tray unless an allowlist the
 *  user set lets it run. It stays mounted while hidden, so a conversation
 *  goes on with the sidebar closed. */
export function AISidebar({ hidden, onClose, onSettings }: { hidden: boolean; onClose: () => void; onSettings: () => void }) {
  const { push } = useToast();
  const size = useSidebarWidth();
  const [providers, setProviders] = useState<AIProvider[] | null>(null);
  const [providerId, setProviderId] = useState("");
  const [targets, setTargets] = useState<AITarget[]>([]);
  const [target, setTarget] = useState(FOLLOW);
  const [mode, setMode] = useState<AIMode | null>(null);
  const [chat, dispatch] = useReducer((c: Chat, a: ChatAction | "reset") => (a === "reset" ? initialChat : reduce(c, a)), initialChat);
  const [draft, setDraft] = useState("");
  const [skills, setSkills] = useState<AISkill[]>([]);
  const [slash, setSlash] = useState({ active: 0, closed: "" }); // closed: the draft Escape closed the menu on
  const [conv] = useState(() =>
    conversations({
      start: api.aiStartConversation,
      listen: (id) =>
        api.onAIEvent(id, (e) => {
          dispatch(e);
          if (e.type === "result" && e.state === "saved") api.aiListSkills().then(setSkills).catch(() => {});
        }),
      close: (id) => void api.aiCloseConversation(id).catch(() => {}),
    }),
  );
  const sending = useRef(false);
  const bottom = useRef<HTMLDivElement>(null);

  const fail = (e: unknown) => push("error", api.errorInfo(e).message);
  const reloadProviders = (onError: (e: unknown) => void = fail) =>
    api
      .aiListProviders()
      .then((l) => {
        setProviders(l);
        setProviderId((id) => (l.some((p) => p.id === id) ? id : l[0]?.id ?? ""));
      })
      .catch(onError);
  const reloadMode = () => api.aiGetMode().then(setMode).catch(() => {});

  // Opening the sidebar re-reads what Settings may have changed. While it
  // shows, Settings can be open beside it, so providers, skills, the mode and
  // open terminals are read again every few seconds.
  useEffect(() => {
    if (hidden) return;
    reloadProviders();
    const read = () => {
      reloadMode();
      api.aiListSkills().then(setSkills).catch(() => {});
      api.aiTerminals().then(setTargets).catch(() => {});
    };
    read();
    const id = setInterval(() => {
      reloadProviders(() => {});
      read();
    }, 3000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hidden]);

  // Braced: scrollIntoView returns a Promise in newer engines, and React
  // would call an effect's return value as its cleanup.
  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [chat.items]);

  const endConversation = () => {
    conv.end();
    dispatch("reset");
  };
  useEffect(() => endConversation, []);

  // A pinned target that closed is gone from the picker; the conversation's
  // banner says so.
  const pinnedGone = target !== FOLLOW && !targets.some((x) => x.id === target);
  useEffect(() => {
    if (pinnedGone && !conv.current()) setTarget(FOLLOW);
  }, [pinnedGone]);

  // The conversation is opened on the first message, with the provider and
  // target chosen then; changing either starts a new one. One send at a
  // time, so Enter pressed twice sends the draft once.
  const send = async () => {
    const text = draft.trim();
    if (!text || chat.closed || sending.current) return;
    sending.current = true;
    try {
      const before = conv.current();
      const id = await conv.ensure(providerId, target);
      if (id !== before) dispatch("reset"); // a new conversation
      dispatch({ type: "user", text });
      setDraft("");
      await api.aiSend(id, text);
    } catch (e) {
      if (!isSuperseded(e)) fail(e);
    } finally {
      sending.current = false;
    }
  };
  const onConv = (f: (id: string) => Promise<unknown>) => () => {
    const id = conv.current();
    if (id) f(id).catch(fail);
  };
  const bypassOff = () => api.aiSetBypass(false).then(reloadMode, fail);
  const bypassOn = () => api.aiSetBypass(true).then(reloadMode, (e) => (api.errorInfo(e).code === "VerifyFailed" ? undefined : fail(e)));

  const matches = slash.closed === draft ? [] : slashMatches(draft, skills);
  const pick = (s: AISkill) => {
    setDraft(pickSlash(draft, s.name));
    setSlash({ active: 0, closed: "" });
  };

  const busy = chat.busy !== "idle";
  const last = chat.items[chat.items.length - 1];
  // Done is for a terminal that cannot report a finished command (KTD34).
  const running = busy && last?.kind === "command" && !last.result && !!targets.find((x) => x.label === last.host)?.confirmOnly;
  const provider = providers?.find((p) => p.id === providerId);

  return (
    <aside
      aria-label={t("ai.title")}
      className={hidden ? "hidden" : "relative shrink-0 panel border-l flex flex-col min-h-0"}
      style={hidden ? undefined : { width: size.width }}
    >
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label={t("ai.resize")}
        aria-valuemin={MIN_WIDTH}
        aria-valuemax={size.max}
        aria-valuenow={size.width}
        tabIndex={0}
        className="absolute inset-y-0 -left-1 z-10 w-2 cursor-col-resize hover:bg-accent/30 focus-visible:bg-accent/40 outline-none"
        onDoubleClick={size.toggleWide}
        onKeyDown={(e) => {
          if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
            e.preventDefault();
            size.set(size.width + (e.key === "ArrowLeft" ? 32 : -32));
          }
        }}
        onPointerDown={(e) => {
          e.preventDefault();
          const handle = e.currentTarget;
          handle.setPointerCapture(e.pointerId);
          const startX = e.clientX;
          const startW = size.width;
          const move = (m: PointerEvent) => size.set(startW + (startX - m.clientX));
          const up = () => {
            handle.removeEventListener("pointermove", move);
            handle.removeEventListener("pointerup", up);
            handle.removeEventListener("pointercancel", up);
          };
          handle.addEventListener("pointermove", move);
          handle.addEventListener("pointerup", up);
          handle.addEventListener("pointercancel", up);
        }}
      />
      <PanelHeader title={t("ai.title")}>
        <IconButton label={size.wide ? t("ai.narrower") : t("ai.wider")} icon={size.wide ? RestoreSizeIcon : MaximizeIcon} onClick={size.toggleWide} />
        <IconButton label={t("common.close")} icon={CloseIcon} onClick={onClose} />
      </PanelHeader>
      {providers?.length === 0 ? (
        <EmptyState icon={AIIcon} title={t("ai.noProvider")} actions={<Button onClick={onSettings}>{t("ai.openSettings")}</Button>}>
          {t("ai.noProviderHint")}
        </EmptyState>
      ) : (
        <>
          <div className="px-3 pb-2 flex flex-col gap-2 border-b border-line">
            <div className="flex gap-2">
              <Select
                compact
                aria-label={t("ai.provider")}
                className="flex-1 min-w-0"
                value={providerId}
                onChange={(e) => setProviderId(e.target.value)}
              >
                {providers?.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name} · {p.model}
                  </option>
                ))}
              </Select>
              <Button size="sm" variant="ghost" type="button" onClick={endConversation} disabled={!chat.items.length}>
                {t("ai.new")}
              </Button>
            </div>
            <Select
              compact
              aria-label={t("ai.target")}
              aria-describedby={targets.length ? undefined : "ai-no-target"}
              disabled={targets.length === 0}
              value={pinnedGone ? FOLLOW : target}
              onChange={(e) => setTarget(e.target.value)}
            >
              <option value={FOLLOW}>{t("ai.followActive")}</option>
              {targets.map((x) => (
                <option key={x.id} value={x.id}>
                  {x.confirmOnly ? t("ai.targetConfirmOnly", { label: x.label }) : x.label}
                </option>
              ))}
            </Select>
            {targets.length === 0 && (
              <p id="ai-no-target" className="text-xs text-muted">
                {t("ai.noTarget")}
              </p>
            )}
            {mode?.bypass ? (
              <div role="status" className="flex items-center gap-2 text-xs text-warning">
                <span className="flex-1">{t("ai.bypassOn")}</span>
                <Button size="sm" variant="danger" type="button" onClick={bypassOff}>
                  {t("ai.bypassOff")}
                </Button>
              </div>
            ) : (
              <button type="button" className="self-start text-xs text-muted hover:text-fg underline" onClick={bypassOn}>
                {t("ai.bypassStart")}
              </button>
            )}
          </div>

          <div className="flex-1 min-h-0 overflow-auto px-3 py-2 flex flex-col gap-2 text-sm" aria-live="off">
            {provider?.keyState === "retype" && !chat.retype && <Retype name={provider.name} onSettings={onSettings} />}
            {chat.items.length === 0 && <p className="text-muted">{t("ai.empty")}</p>}
            {chat.items.map((it, i) => (
              <Entry key={i} item={it} />
            ))}
            {chat.retype && <Retype name={chat.retype} onSettings={onSettings} />}
            {chat.retry && (
              <Button className="self-start" size="sm" type="button" onClick={onConv(api.aiRetry)}>
                {t("ai.retry")}
              </Button>
            )}
            {chat.closed && (
              <div role="alert" className="rounded-control border border-warning p-2 flex flex-col gap-2">
                <p>{t("ai.targetClosed")}</p>
                <Button className="self-start" size="sm" type="button" onClick={endConversation}>
                  {t("ai.new")}
                </Button>
              </div>
            )}
            <div ref={bottom} />
          </div>

          <div role="status" className="px-3 text-xs text-muted min-h-5">
            {chat.busy === "thinking" ? t("ai.thinking") : chat.busy === "working" ? t("ai.working") : ""}
          </div>
          <form
            className="p-3 pt-1 flex flex-col gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              send();
            }}
          >
            {matches.length === 0 && slash.closed !== draft && isSlashDraft(draft) && (
              <p role="status" className="text-xs text-muted px-1">
                {skills.length === 0 ? t("ai.slash.none") : t("ai.slash.noMatch")}
              </p>
            )}
            {matches.length > 0 && <SlashMenu matches={matches} active={Math.min(slash.active, matches.length - 1)} onPick={pick} />}
            <textarea
              aria-label={t("ai.message")}
              aria-describedby="ai-slash-hint"
              aria-controls={matches.length ? "ai-slash-menu" : undefined}
              aria-activedescendant={matches.length ? `ai-slash-${matches[Math.min(slash.active, matches.length - 1)].id}` : undefined}
              rows={3}
              value={draft}
              disabled={chat.closed || !providerId}
              onChange={(e) => {
                setDraft(e.target.value);
                setSlash((x) => ({ ...x, active: 0 }));
              }}
              onKeyDown={(e) => {
                if (matches.length && !e.nativeEvent.isComposing) {
                  const at = Math.min(slash.active, matches.length - 1);
                  const move = e.key === "ArrowDown" ? 1 : e.key === "ArrowUp" ? -1 : 0;
                  if (move) {
                    e.preventDefault();
                    setSlash({ active: (at + move + matches.length) % matches.length, closed: "" });
                    return;
                  }
                  if (e.key === "Enter" || e.key === "Tab") {
                    e.preventDefault();
                    pick(matches[at]);
                    return;
                  }
                  if (e.key === "Escape") {
                    e.preventDefault();
                    setSlash({ active: 0, closed: draft });
                    return;
                  }
                }
                if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault();
                  send();
                }
              }}
              className="w-full resize-none rounded-control border border-control bg-control-bg p-2 text-sm"
            />
            <p id="ai-slash-hint" className="text-xs text-muted">
              {t("ai.slash.hint")}
            </p>
            <div className="flex gap-2 justify-end">
              {running && (
                <Button size="sm" variant="ghost" type="button" title={t("ai.doneHint")} onClick={onConv(api.aiJobDone)}>
                  {t("ai.done")}
                </Button>
              )}
              {busy ? (
                <Button size="sm" variant="danger" type="button" icon={StopIcon} onClick={onConv(api.aiStop)}>
                  {t("ai.stop")}
                </Button>
              ) : (
                <Button size="sm" variant="primary" type="submit" disabled={!draft.trim() || chat.closed || !providerId}>
                  {t("ai.send")}
                </Button>
              )}
            </div>
          </form>
        </>
      )}
    </aside>
  );
}

function Retype({ name, onSettings }: { name: string; onSettings: () => void }) {
  return (
    <div role="alert" className="rounded-control border border-warning p-2 flex flex-col gap-2">
      <p>{t("ai.retype", { name })}</p>
      <Button className="self-start" size="sm" type="button" onClick={onSettings}>
        {t("ai.openSettings")}
      </Button>
    </div>
  );
}

const RESULT: Record<string, Parameters<typeof t>[0]> = {
  done: "ai.result.done",
  interrupted: "ai.result.interrupted",
  ended: "ai.result.ended",
  unknown: "ai.result.unknown",
  refused: "ai.result.refused",
  denied: "ai.result.denied",
  running: "ai.result.running",
  saved: "ai.result.saved",
};

function Entry({ item }: { item: Item }) {
  switch (item.kind) {
    case "user":
      return <p className="self-end max-w-[85%] rounded-card bg-active px-3 py-1.5 whitespace-pre-wrap break-words allow-select">{item.text}</p>;
    case "assistant":
      return (
        <div>
          <RichText text={item.text} />
        </div>
      );
    case "command": {
      const r = item.result;
      return (
        <div className="rounded-control border border-line p-2 flex flex-col gap-1">
          <p className="text-xs text-muted">{item.host ? t("ai.commandOn", { host: item.host }) : t("ai.command")}</p>
          <pre className="font-mono text-xs whitespace-pre-wrap break-all allow-select">{item.command}</pre>
          {r ? (
            <>
              <p className={`text-xs ${r.state === "done" ? "text-muted" : "text-warning"}`}>{t(RESULT[r.state] ?? "ai.result.other", { state: r.state })}</p>
              {r.text && <pre className="max-h-60 overflow-auto font-mono text-xs whitespace-pre-wrap break-all bg-raised rounded-control p-2 allow-select">{r.text}</pre>}
            </>
          ) : (
            <p className="text-xs text-muted">{t("ai.result.waiting")}</p>
          )}
        </div>
      );
    }
    case "skill": {
      const r = item.result;
      return (
        <div className="rounded-control border border-line p-2 flex flex-col gap-1">
          <p className="text-xs text-muted">{t(item.replaces ? "ai.skillReplaces" : "ai.skillProposed", { name: `/${item.name}` })}</p>
          <pre className="max-h-60 overflow-auto font-mono text-xs whitespace-pre-wrap break-words allow-select">{item.body}</pre>
          <p className={`text-xs ${!r || r.state === "saved" ? "text-muted" : "text-warning"}`}>
            {r ? t(RESULT[r.state] ?? "ai.result.other", { state: r.state }) : t("ai.result.waitingSkill")}
          </p>
        </div>
      );
    }
    case "error": {
      const hint = errorHint(item.code);
      return (
        <div className="flex flex-col gap-1">
          <p role="alert" className="text-danger whitespace-pre-wrap break-words">
            {item.text}
          </p>
          {hint && <p className="text-xs text-muted">{hint}</p>}
        </div>
      );
    }
    case "notice":
      return <p className="text-xs text-muted">{item.text === "stopped" ? t("ai.stopped") : t("ai.targetClosedShort")}</p>;
  }
}
