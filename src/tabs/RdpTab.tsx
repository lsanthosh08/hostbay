import { useEffect, useRef, useState } from "react";
import * as api from "../api";
import type { Host } from "../api";
import { useConnect } from "../connect/useConnect";
import { t } from "../i18n";
import { useToast } from "../ui/toast";
import { Button } from "../ui/Button";
import { Spinner } from "../ui/kit";
import { Select } from "../ui/Select";
import { StatusDot } from "../ui/StatusDot";

// Map a browser KeyboardEvent.code to a PC set-1 scancode. Covers the common
// keys; extend as needed. ponytail: partial table — add rows when a key is
// missing rather than pulling a full keymap dependency.
const SCAN: Record<string, number> = {
  Escape: 0x01, Digit1: 0x02, Digit2: 0x03, Digit3: 0x04, Digit4: 0x05,
  Digit5: 0x06, Digit6: 0x07, Digit7: 0x08, Digit8: 0x09, Digit9: 0x0a,
  Digit0: 0x0b, Minus: 0x0c, Equal: 0x0d, Backspace: 0x0e, Tab: 0x0f,
  KeyQ: 0x10, KeyW: 0x11, KeyE: 0x12, KeyR: 0x13, KeyT: 0x14, KeyY: 0x15,
  KeyU: 0x16, KeyI: 0x17, KeyO: 0x18, KeyP: 0x19, BracketLeft: 0x1a,
  BracketRight: 0x1b, Enter: 0x1c, ControlLeft: 0x1d, KeyA: 0x1e, KeyS: 0x1f,
  KeyD: 0x20, KeyF: 0x21, KeyG: 0x22, KeyH: 0x23, KeyJ: 0x24, KeyK: 0x25,
  KeyL: 0x26, Semicolon: 0x27, Quote: 0x28, Backquote: 0x29, ShiftLeft: 0x2a,
  Backslash: 0x2b, KeyZ: 0x2c, KeyX: 0x2d, KeyC: 0x2e, KeyV: 0x2f, KeyB: 0x30,
  KeyN: 0x31, KeyM: 0x32, Comma: 0x33, Period: 0x34, Slash: 0x35,
  ShiftRight: 0x36, AltLeft: 0x38, Space: 0x39, CapsLock: 0x3a,
  F1: 0x3b, F2: 0x3c, F3: 0x3d, F4: 0x3e, F5: 0x3f, F6: 0x40, F7: 0x41,
  F8: 0x42, F9: 0x43, F10: 0x44, F11: 0x57, F12: 0x58,
  // Extended keys: IronRDP's Scancode::from_u16 treats a scancode as extended
  // only when (code & 0xE000) == 0xE000, so these MUST carry the 0xE000 prefix
  // — not 0x100. With 0x1xx the high bits were dropped and e.g. ArrowRight
  // (0x14d -> 0x4d) landed as numpad-6, inserting "6".
  ArrowUp: 0xe048, ArrowLeft: 0xe04b, ArrowRight: 0xe04d, ArrowDown: 0xe050,
  Delete: 0xe053, Home: 0xe047, End: 0xe04f, PageUp: 0xe049, PageDown: 0xe051,
  Insert: 0xe052,
  // Extended control/nav keys.
  ControlRight: 0xe01d, AltRight: 0xe038,
  MetaLeft: 0xe05b, MetaRight: 0xe05c,
  NumpadEnter: 0xe01c, NumpadDivide: 0xe035,
};

export function RdpTab({
  host,
  onStatus,
}: {
  host: Host;
  onStatus?: (s: "connecting" | "live" | "error") => void;
}) {
  const { push } = useToast();
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  // idle = show the pre-connect screen; user picks resolution + clicks Connect.
  const [status, setStatus] = useState<"idle" | "connecting" | "live" | "closed" | "error">("idle");
  const [errMsg, setErrMsg] = useState("");
  const size = useRef({ w: 1280, h: 1024 });
  const unlistenersRef = useRef<(() => void)[]>([]);
  // Backend-made id of the live session; every RDP call and event uses it.
  const rdpIdRef = useRef<string | null>(null);
  // False once the tab unmounts, so a connect still in flight closes its own session.
  const mountedRef = useRef(true);
  // Resolution: common sizes + aspect ratios. "auto" = fit the window box.
  const [resolution, setResolution] = useState<string>("auto");
  // A certificate or jump-host key the user must accept before connecting
  // shows as a dialog; trusting it runs connect again.
  const { fail, dialog } = useConnect();

  const RES: { label: string; value: string }[] = [
    { label: "Fit window (recommended)", value: "auto" },
    { label: "1280 × 720  (16:9)", value: "1280x720" },
    { label: "1366 × 768  (16:9)", value: "1366x768" },
    { label: "1600 × 900  (16:9)", value: "1600x900" },
    { label: "1920 × 1080 (16:9)", value: "1920x1080" },
    { label: "1280 × 800  (16:10)", value: "1280x800" },
    { label: "1440 × 900  (16:10)", value: "1440x900" },
    { label: "1280 × 1024 (5:4)", value: "1280x1024" },
    { label: "1024 × 768  (4:3)", value: "1024x768" },
  ];

  const cleanup = () => {
    unlistenersRef.current.forEach((u) => u());
    unlistenersRef.current = [];
    if (rdpIdRef.current) api.rdpClose(rdpIdRef.current).catch(() => {});
    rdpIdRef.current = null;
  };

  const input = (
    kind: Parameters<typeof api.rdpInput>[1],
    opts: Parameters<typeof api.rdpInput>[2]
  ) => {
    if (rdpIdRef.current) api.rdpInput(rdpIdRef.current, kind, opts);
  };

  const connect = async () => {
    setStatus("connecting");
    onStatus?.("connecting");
    setErrMsg("");
    const canvas = canvasRef.current!;
    const ctx = canvas.getContext("2d")!;

    // Resolution: fixed choice, or "auto" = the window box in logical px.
    let w: number, h: number;
    const fixed = resolution.match(/^(\d+)x(\d+)$/);
    if (fixed) {
      w = Number(fixed[1]);
      h = Number(fixed[2]);
    } else {
      const rect = (canvas.parentElement ?? canvas).getBoundingClientRect();
      w = Math.max(800, Math.min(1920, Math.round(rect.width) || 1280));
      h = Math.max(600, Math.min(1200, Math.round(rect.height) || 800));
    }
    size.current = { w, h };

    try {
      const id = await api.rdpConnect(host.id, w, h);
      if (!mountedRef.current) {
        api.rdpClose(id).catch(() => {});
        return;
      }
      rdpIdRef.current = id;
      canvas.width = w;
      canvas.height = h;
      // Frames sent since connect were held for us; they arrive in order.
      unlistenersRef.current.push(
        api.rdpView(id, {
          onSize: ({ width, height }) => {
            size.current = { w: width, h: height };
            canvas.width = width;
            canvas.height = height;
          },
          onFrame: (f) => ctx.putImageData(new ImageData(f.rgba, f.width, f.height), f.x, f.y),
          // The helper exited: the user closed it (empty reason), the server
          // ended it, or the helper crashed. SSH tabs are unaffected.
          onClosed: (reason) => {
            // The stream can drop while the helper lives on; closing an ended session is a no-op.
            api.rdpClose(id).catch(() => {});
            rdpIdRef.current = null;
            setStatus("closed");
            setErrMsg(reason);
            onStatus?.("error");
            if (reason) push("error", `RDP: ${reason}`);
          },
        })
      );

      // Clipboard text/files are synced natively by the RDP helper (and by
      // the backend on Linux). We only listen for remote-copied files to
      // tell the user where they went.
      const unClip = await api.onRdpClipFiles(id, (paths) => {
        const n = paths.length;
        push("success", `${n} item${n === 1 ? "" : "s"} copied from remote — paste in your file manager`);
      });
      // Unmounted, disconnected or closed while we waited: drop the listener
      // and keep the status cleanup or onClosed already set.
      if (rdpIdRef.current !== id) {
        unClip();
        return;
      }
      unlistenersRef.current.push(unClip);
      setStatus("live");
      onStatus?.("live");
    } catch (e) {
      cleanup();
      setStatus("idle");
      fail(e, connect, {
        host,
        onStep: (step) => {
          setStatus("error");
          onStatus?.("error");
          setErrMsg(step.message);
          push("error", `RDP: ${step.message}`);
        },
      });
    }
  };

  const disconnect = () => {
    cleanup();
    setStatus("idle");
    onStatus?.("error");
  };

  // Close the session if the tab unmounts.
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      cleanup();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Coalesce pointer moves to one send per animation frame — a raw mousemove
  // stream fires dozens of events/sec, and one IPC each makes the session feel
  // laggy. We keep only the latest position.
  const pendingMove = useRef<{ x: number; y: number } | null>(null);
  const moveRaf = useRef<number | null>(null);
  const sendMove = (x: number, y: number) => {
    pendingMove.current = { x, y };
    if (moveRaf.current != null) return;
    moveRaf.current = requestAnimationFrame(() => {
      moveRaf.current = null;
      const p = pendingMove.current;
      if (p) input("move", p);
    });
  };

  // Map a pointer event to remote coordinates. The canvas uses object-contain,
  // so the painted image is letterboxed inside the element (black bars when the
  // element aspect ratio differs from the remote's). Map against that inner
  // image rect, not the full element, or the cursor drifts.
  const toRemote = (e: React.PointerEvent | React.MouseEvent | React.WheelEvent) => {
    const c = canvasRef.current!;
    const r = c.getBoundingClientRect();
    const rw = size.current.w;
    const rh = size.current.h;
    // Scale the remote image to fit inside the element (object-contain).
    const scale = Math.min(r.width / rw, r.height / rh);
    const dispW = rw * scale;
    const dispH = rh * scale;
    const offX = (r.width - dispW) / 2; // letterbox bars
    const offY = (r.height - dispH) / 2;
    const x = Math.round(((e.clientX - r.left - offX) / dispW) * rw);
    const y = Math.round(((e.clientY - r.top - offY) / dispH) * rh);
    return {
      x: Math.max(0, Math.min(rw - 1, x)),
      y: Math.max(0, Math.min(rh - 1, y)),
    };
  };

  const live = status === "live";

  return (
    <div className="h-full flex flex-col bg-black">
      {dialog}
      {/* Thin bar: live shows a Disconnect button; otherwise host + status. */}
      <div className="px-3 py-1.5 text-xs flex items-center gap-2 bg-surface text-fg border-b border-line">
        <StatusDot status={live ? "live" : status === "connecting" ? "connecting" : status === "error" ? "error" : undefined} />
        <span>
          {host.user}@{host.hostname}:{host.port || 3389}
        </span>
        <span className="text-muted">
          {status === "idle" && t("rdp.notConnected")}
          {status === "connecting" && t("rdp.connecting")}
          {status === "live" && t("rdp.connected", { w: size.current.w, h: size.current.h })}
          {status === "closed" && (errMsg ? t("rdp.closedWith", { reason: errMsg }) : t("rdp.closed"))}
          {status === "error" && t("rdp.error", { reason: errMsg })}
        </span>
        {live && (
          <Button size="sm" variant="danger" className="ml-auto" onClick={disconnect}>
            {t("rdp.disconnect")}
          </Button>
        )}
      </div>

      <div className="flex-1 min-h-0 flex items-center justify-center overflow-hidden relative">
        {/* Pre-connect / error / closed screen: resolution picker + Connect. */}
        {status !== "live" && status !== "connecting" && (
          <div className="absolute inset-0 z-10 flex items-center justify-center bg-ground">
            <div className="panel rounded-card p-6 w-96 max-w-[90%] space-y-4">
              <div>
                <div className="font-semibold mb-1">{t("rdp.title")}</div>
                <div className="text-sm opacity-60">
                  {host.user}@{host.hostname}:{host.port || 3389}
                </div>
              </div>
              {(status === "error" || status === "closed") && errMsg && (
                <div className="text-xs text-danger break-words">{errMsg}</div>
              )}
              <label className="block text-sm">
                <span className="opacity-60 text-xs">{t("rdp.resolution")}</span>
                <Select value={resolution} onChange={(e) => setResolution(e.target.value)} className="mt-1 w-full">
                  {RES.map((r) => (
                    <option key={r.value} value={r.value}>
                      {r.label}
                    </option>
                  ))}
                </Select>
                <span className="block mt-1 text-[11px] opacity-40">
                  {t("rdp.resolutionHint")}
                </span>
              </label>
              <Button variant="primary" className="w-full" onClick={connect}>
                {t("rdp.connect")}
              </Button>
            </div>
          </div>
        )}
        {status === "connecting" && (
          <div className="absolute inset-0 flex items-center justify-center gap-2 text-term-fg z-10">
            <Spinner /> {t("rdp.connectingScreen")}
          </div>
        )}
        <canvas
          ref={canvasRef}
          tabIndex={0}
          className="w-full h-full object-contain outline-none"
          style={{ imageRendering: "auto", cursor: "default" }}
          onPointerMove={(e) => {
            if (!live) return;
            const { x, y } = toRemote(e);
            sendMove(x, y);
          }}
          onPointerDown={(e) => {
            if (!live) return;
            canvasRef.current?.focus();
            const { x, y } = toRemote(e);
            input("button", { x, y, button: e.button, down: true });
          }}
          onPointerUp={(e) => {
            if (!live) return;
            const { x, y } = toRemote(e);
            input("button", { x, y, button: e.button, down: false });
          }}
          onContextMenu={(e) => e.preventDefault()}
          onWheel={(e) => {
            if (!live) return;
            const { x, y } = toRemote(e);
            input("wheel", { x, y, delta: e.deltaY > 0 ? -120 : 120 });
          }}
          onKeyDown={(e) => {
            if (!live) return;
            const sc = SCAN[e.code];
            if (sc == null) return;
            e.preventDefault();
            input("key", { scancode: sc, down: true });
          }}
          onKeyUp={(e) => {
            if (!live) return;
            const sc = SCAN[e.code];
            if (sc == null) return;
            e.preventDefault();
            input("key", { scancode: sc, down: false });
          }}
        />
      </div>
    </div>
  );
}
