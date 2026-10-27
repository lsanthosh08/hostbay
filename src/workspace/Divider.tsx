import { useRef, type KeyboardEvent, type PointerEvent } from "react";
import { t } from "../i18n";

// How far one arrow key press moves a divider, as a share of its split.
const STEP = 0.05;

/** The border between two panes of a split: drag it, or focus it and use
 *  the arrow keys (KTD40). `onResize` takes the move as a share of the
 *  split; a drag reports at most once a frame, so a terminal behind it is
 *  resized once a frame, not once per pointer event. */
export function Divider({ dir, share, onResize }: { dir: "row" | "col"; share: number; onResize: (delta: number) => void }) {
  const drag = useRef<{ at: number; size: number; pending: number; frame: number } | null>(null);
  const row = dir === "row";
  const pos = (e: PointerEvent) => (row ? e.clientX : e.clientY);

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    const r = e.currentTarget.parentElement!.getBoundingClientRect();
    e.currentTarget.setPointerCapture(e.pointerId);
    drag.current = { at: pos(e), size: row ? r.width : r.height, pending: 0, frame: 0 };
  };
  const onPointerMove = (e: PointerEvent) => {
    const d = drag.current;
    if (!d || !d.size) return;
    d.pending += (pos(e) - d.at) / d.size;
    d.at = pos(e);
    d.frame ||= requestAnimationFrame(() => {
      onResize(d.pending);
      d.pending = 0;
      d.frame = 0;
    });
  };
  const end = () => (drag.current = null);
  const onKeyDown = (e: KeyboardEvent) => {
    const back = row ? "ArrowLeft" : "ArrowUp";
    const fwd = row ? "ArrowRight" : "ArrowDown";
    if (e.key !== back && e.key !== fwd) return;
    e.preventDefault();
    onResize(e.key === fwd ? STEP : -STEP);
  };

  return (
    <div
      role="separator"
      tabIndex={0}
      aria-label={t("pane.resize")}
      aria-orientation={row ? "vertical" : "horizontal"}
      aria-valuenow={Math.round(share * 100)}
      aria-valuemin={0}
      aria-valuemax={100}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={end}
      onPointerCancel={end}
      onKeyDown={onKeyDown}
      className={
        "shrink-0 bg-line hover:bg-accent focus-visible:bg-accent outline-none touch-none " +
        (row ? "w-1 cursor-col-resize" : "h-1 cursor-row-resize")
      }
    />
  );
}
