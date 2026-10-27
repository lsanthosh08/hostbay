import { useEffect, useRef, useState } from "react";
import * as api from "../api";
import { t } from "../i18n";
import { useSessions, type Connection, type ConnStatus } from "../state/sessions";
import { Button } from "../ui/Button";
import { KeyboardIcon, ScaleIcon } from "../ui/icons";
import { RemoteScreen, type ScreenPhase } from "./RemoteScreen";
import { channelSocket, loadSpice, type SpiceMain } from "./spice";

/**
 * A SPICE host's screen (R46). Go has already signed in to the main channel
 * (KTD19): the tab's session ID is the spiceId. spice-html5 opens one
 * "spice" GoStream per channel, main first, each sending the spiceId first,
 * and links every channel with an empty password; Go signs each one in with
 * the real one, so it never reaches the page. Keys reach the machine only
 * while its screen has focus (spice-html5 focuses it under the pointer). A
 * closed screen reconnects by signing in again (the provider redials).
 */
export function SpiceTab({ conn, onStatus }: { conn: Connection; onStatus: (s: ConnStatus) => void }) {
  const spiceId = conn.sessionId;
  const { reconnect } = useSessions();
  const areaRef = useRef<HTMLDivElement>(null);
  const screenRef = useRef<HTMLDivElement>(null);
  const ctrlAltDel = useRef<(() => void) | null>(null);
  const [phase, setPhase] = useState<ScreenPhase>("opening");
  const [problem, setProblem] = useState("");
  const [scale, setScale] = useState(true);
  const [focused, setFocused] = useState(false);
  // spice-html5 finds its screen by element ID.
  const screenId = `spice-screen-${spiceId}`;
  const live = useRef({ onStatus });
  live.current = { onStatus };

  useEffect(() => {
    let gone = false;
    let ended = false;
    let sc: SpiceMain | null = null;
    const stop = () => {
      ctrlAltDel.current = null;
      try {
        sc?.stop(); // closes every channel's stream; Go ends the session with main's
      } catch {
        /* a half-built display has nothing to tear down */
      }
      sc = null;
    };
    const close = (message: string) => {
      if (gone || ended) return;
      ended = true;
      stop();
      setPhase("closed");
      setProblem(message);
      live.current.onStatus("error");
    };
    setPhase("opening");
    live.current.onStatus("connecting");
    loadSpice()
      .then(({ SpiceMainConn, sendCtrlAltDel }) => {
        if (gone) return;
        const main = new SpiceMainConn({
          uri: "spice://hostbay", // required, but every channel uses the socket below
          password: "",
          screen_id: screenId,
          socket: channelSocket(spiceId, () => api.spiceStream() as unknown as WebSocket),
          onsuccess: () => {
            if (gone) return;
            setPhase("live");
            live.current.onStatus("live");
          },
          onerror: (e) => close(e.message),
        });
        sc = main;
        ctrlAltDel.current = () => sendCtrlAltDel(main);
      })
      .catch((e) => close(api.errorInfo(e).message));
    return () => {
      gone = true;
      stop();
    };
  }, [spiceId, screenId]);

  // Scale to fit: the screen keeps the machine's resolution and is scaled
  // as a whole, so spice-html5's pointer positions stay in screen pixels.
  useEffect(() => {
    const area = areaRef.current!;
    const screen = screenRef.current!;
    const fit = () => {
      const canvas = screen.querySelector("canvas");
      if (!scale || !canvas?.width || !canvas.height) {
        screen.style.transform = "";
        return;
      }
      const s = Math.min(area.clientWidth / canvas.width, area.clientHeight / canvas.height);
      const x = (area.clientWidth - canvas.width * s) / 2;
      const y = (area.clientHeight - canvas.height * s) / 2;
      screen.style.transform = `translate(${x}px, ${y}px) scale(${s})`;
    };
    // The screen grows when spice-html5 adds or resizes its canvas.
    const ro = new ResizeObserver(fit);
    ro.observe(area);
    ro.observe(screen);
    fit();
    return () => ro.disconnect();
  }, [scale]);

  const isLive = phase === "live";
  return (
    <RemoteScreen
      phase={phase}
      problem={problem}
      onReconnect={() => reconnect(spiceId)}
      showKeys={isLive}
      focused={focused}
      tools={
        <>
          <Button size="sm" variant={scale ? "default" : "ghost"} icon={ScaleIcon} aria-pressed={scale} onClick={() => setScale(!scale)}>
            {t("vnc.scale")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon={KeyboardIcon}
            disabled={!isLive}
            onClick={() => {
              ctrlAltDel.current?.();
              screenRef.current?.querySelector("canvas")?.focus();
            }}
          >
            {t("vnc.ctrlAltDel")}
          </Button>
        </>
      }
      // The focus outline is on the area, drawn above the screen; an inset
      // ring on the screen would be painted under its canvas.
      areaRef={areaRef}
      areaClassName={`flex-1 min-h-0 relative bg-ground focus-within:outline-2 focus-within:-outline-offset-2 focus-within:outline-focus ${scale ? "overflow-hidden" : "overflow-auto"}`}
      screen={
        <div
          ref={screenRef}
          id={screenId}
          role="application"
          aria-label={t("vnc.screen", { name: conn.host.alias })}
          className="absolute left-0 top-0 origin-top-left [&_canvas]:block [&_canvas]:outline-none"
          onFocus={() => setFocused(true)}
          onBlur={() => setFocused(false)}
        />
      }
    />
  );
}
