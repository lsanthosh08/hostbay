import { useEffect, useRef, useState } from "react";
import type RFB from "@novnc/novnc";
import * as api from "../api";
import { t } from "../i18n";
import { useSessions, type Connection, type ConnStatus } from "../state/sessions";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { controlCls } from "../ui/Field";
import { KeyboardIcon, ScaleIcon, SendTextIcon, ViewOnlyIcon } from "../ui/icons";
import { useToast } from "../ui/toast";
import { RemoteScreen, type ScreenPhase } from "./RemoteScreen";

/**
 * A VNC host's screen (R45), or an SSH host's X11 desktop (R47). Go has
 * already signed in (KTD19) or started Xvnc (KTD20): the tab's session ID
 * is the vncId, which opens the "vnc" GoStream once. noVNC runs over that
 * stream against a server that asks for no password, so none ever reaches
 * the page. Keys reach the server only while the screen has focus. A closed
 * screen reconnects by signing in or starting the desktop again (the
 * provider redials). resize asks the server to fit its desktop to the tab,
 * for an Xvnc that started at its default size.
 */
export function VncTab({ conn, resize, onStatus }: { conn: Connection; resize?: boolean; onStatus: (s: ConnStatus) => void }) {
  const vncId = conn.sessionId;
  const { reconnect } = useSessions();
  const { push } = useToast();
  const screenRef = useRef<HTMLDivElement>(null);
  const rfbRef = useRef<RFB | null>(null);
  const [phase, setPhase] = useState<ScreenPhase>("opening");
  const [problem, setProblem] = useState("");
  const [scale, setScale] = useState(true);
  const [viewOnly, setViewOnly] = useState(false);
  const [focused, setFocused] = useState(false);
  const [sending, setSending] = useState(false);
  // The latest values for the stream's callbacks, which outlive renders.
  const live = useRef({ onStatus, push, scale, viewOnly, resize });
  live.current = { onStatus, push, scale, viewOnly, resize };

  useEffect(() => {
    let gone = false;
    let ws: WebSocket | null = null;
    // The first close wins: refuse()'s disconnect fires "disconnect" after it.
    let ended = false;
    const close = (message = "") => {
      if (gone || ended) return;
      ended = true;
      rfbRef.current = null;
      setPhase("closed");
      setProblem(message);
      live.current.onStatus("error");
    };
    live.current.onStatus("connecting");
    import("@novnc/novnc")
      .then(({ default: NoVnc }) => {
        if (gone) return;
        // The Wails stream is WebSocket-shaped; noVNC takes it as its channel.
        const stream = api.vncStream() as unknown as WebSocket;
        ws = stream;
        stream.onclose = () => close(); // Go refused the vncId, or it expired
        stream.onopen = () => {
          stream.send(vncId);
          // In the same handler: Go starts the RFB handshake at once, and
          // noVNC must be listening before its first frame arrives.
          const rfb = new NoVnc(screenRef.current!, stream, {});
          rfbRef.current = rfb;
          rfb.scaleViewport = live.current.scale;
          rfb.viewOnly = live.current.viewOnly;
          rfb.resizeSession = !!live.current.resize;
          rfb.background = "transparent";
          rfb.addEventListener("connect", () => {
            setPhase("live");
            live.current.onStatus("live");
          });
          rfb.addEventListener("disconnect", () => close());
          // Go offers the page security type None only, so neither of these
          // can happen unless something is wrong: never answer one.
          const refuse = () => {
            close(t("vnc.authAsked"));
            rfb.disconnect();
          };
          rfb.addEventListener("credentialsrequired", refuse);
          rfb.addEventListener("securityfailure", refuse);
          rfb.addEventListener("clipboard", (e) => {
            const text = (e as CustomEvent<{ text: string }>).detail.text;
            navigator.clipboard.writeText(text).then(
              () => live.current.push("success", t("vnc.copied")),
              (err: unknown) => live.current.push("error", t("vnc.copyFailed", { reason: api.errorInfo(err).message }))
            );
          });
        };
      })
      .catch((e) => close(api.errorInfo(e).message));
    // Closing the stream hangs up on the server.
    return () => {
      gone = true;
      if (rfbRef.current) rfbRef.current.disconnect();
      else ws?.close();
      rfbRef.current = null;
    };
  }, [vncId]);

  useEffect(() => {
    if (rfbRef.current) rfbRef.current.scaleViewport = scale;
  }, [scale]);
  useEffect(() => {
    if (rfbRef.current) rfbRef.current.viewOnly = viewOnly;
  }, [viewOnly]);

  const isLive = phase === "live";
  const toggle = (pressed: boolean) => (pressed ? "default" : "ghost");

  return (
    <RemoteScreen
      phase={phase}
      problem={problem}
      onReconnect={() => reconnect(vncId)}
      showKeys={isLive && !viewOnly}
      focused={focused}
      tools={
        <>
          <Button size="sm" variant={toggle(scale)} icon={ScaleIcon} aria-pressed={scale} onClick={() => setScale(!scale)}>
            {t("vnc.scale")}
          </Button>
          <Button size="sm" variant={toggle(viewOnly)} icon={ViewOnlyIcon} aria-pressed={viewOnly} onClick={() => setViewOnly(!viewOnly)}>
            {t("vnc.viewOnly")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon={KeyboardIcon}
            disabled={!isLive || viewOnly}
            onClick={() => {
              rfbRef.current?.sendCtrlAltDel();
              rfbRef.current?.focus();
            }}
          >
            {t("vnc.ctrlAltDel")}
          </Button>
          <Button size="sm" variant="ghost" icon={SendTextIcon} disabled={!isLive || viewOnly} onClick={() => setSending(true)}>
            {t("vnc.sendText")}
          </Button>
        </>
      }
      screen={
        <div
          ref={screenRef}
          role="application"
          aria-label={t("vnc.screen", { name: conn.host.alias })}
          className="absolute inset-0 focus-within:ring-2 focus-within:ring-inset focus-within:ring-focus"
          onFocus={() => setFocused(true)}
          onBlur={() => setFocused(false)}
        />
      }
    >
      {sending && (
        <SendTextDialog
          onClose={() => setSending(false)}
          onSend={(text) => {
            rfbRef.current?.clipboardPasteFrom(text);
            setSending(false);
            push("success", t("vnc.sent"));
            rfbRef.current?.focus();
          }}
        />
      )}
    </RemoteScreen>
  );
}

/** Text the user pastes or types here goes to the remote clipboard. Reading
 *  the local clipboard instead would need a permission prompt each time. */
function SendTextDialog({ onClose, onSend }: { onClose: () => void; onSend: (text: string) => void }) {
  const [text, setText] = useState("");
  return (
    <Dialog
      title={t("vnc.sendTextTitle")}
      onClose={onClose}
      dismissible={!text}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" form="vnc-send-text" disabled={!text}>
            {t("vnc.send")}
          </Button>
        </>
      }
    >
      <form
        id="vnc-send-text"
        onSubmit={(e) => {
          e.preventDefault();
          if (text) onSend(text);
        }}
      >
        <textarea
          aria-label={t("vnc.sendTextLabel")}
          placeholder={t("vnc.sendTextLabel")}
          className={`w-full h-32 p-2 resize-y allow-select ${controlCls}`}
          autoFocus
          spellCheck={false}
          value={text}
          onChange={(e) => setText(e.target.value)}
        />
      </form>
    </Dialog>
  );
}
