import { useEffect, useState, type ReactNode } from "react";
import { t } from "../i18n";

const MIN = 180;
const MAX = 480;
const clamp = (w: number) => Math.min(MAX, Math.max(MIN, w));

/** A side panel's title row, with its actions on the right. */
export function PanelHeader({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="h-10 shrink-0 px-3 flex items-center gap-1">
      <h2 className="text-xs font-semibold uppercase tracking-wide text-muted flex-1 truncate">{title}</h2>
      {children}
    </div>
  );
}

/** The resizable column beside the activity rail. Drag its edge, or use the
 *  arrow keys on it, to change the width; the width is remembered. */
export function SidePanel({ children }: { children: ReactNode }) {
  const [width, setWidth] = useState(() => clamp(Number(localStorage.getItem("sidePanelWidth")) || 260));
  useEffect(() => {
    localStorage.setItem("sidePanelWidth", String(width));
  }, [width]);

  const startDrag = (e: React.PointerEvent<HTMLDivElement>) => {
    const from = e.clientX - width;
    const el = e.currentTarget;
    el.setPointerCapture(e.pointerId);
    const move = (ev: PointerEvent) => setWidth(clamp(ev.clientX - from));
    const up = () => {
      el.removeEventListener("pointermove", move);
      el.removeEventListener("pointerup", up);
    };
    el.addEventListener("pointermove", move);
    el.addEventListener("pointerup", up);
  };

  return (
    <aside className="relative shrink-0 panel border-r flex flex-col min-h-0" style={{ width }}>
      {children}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label={t("shell.resizePanel")}
        aria-valuenow={width}
        aria-valuemin={MIN}
        aria-valuemax={MAX}
        tabIndex={0}
        onPointerDown={startDrag}
        onKeyDown={(e) => {
          const step = e.key === "ArrowRight" ? 16 : e.key === "ArrowLeft" ? -16 : 0;
          if (!step) return;
          e.preventDefault();
          setWidth((w) => clamp(w + step));
        }}
        className="absolute top-0 -right-1 w-2 h-full cursor-col-resize hover:bg-accent/30 focus-visible:bg-accent/40 outline-none"
      />
    </aside>
  );
}
