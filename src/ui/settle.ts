import { useLayoutEffect, useRef, useState, type RefObject } from "react";

/** How long a confirm button must sit still before it takes a click. A
 *  button that appears, or moves, under a click aimed at something else
 *  (the next card in the AI approval tray, the next queued trust prompt)
 *  would otherwise confirm what the user never read. */
export const SETTLE_MS = 600;
export const settled = (since: number, now: number) => now - since >= SETTLE_MS;
/** How long until the button takes a click, 0 once it does. */
export const waitLeft = (since: number, now: number) => Math.max(0, SETTLE_MS - (now - since));

/** How a waiting confirm button looks: in the tab order as usual (so focus
 *  does not jump), announced as unavailable, and muted. */
export const waitProps = (waiting: boolean) =>
  waiting ? { "aria-disabled": true as const, className: "opacity-50 cursor-not-allowed" } : {};

type Mark = { top: number | null; key: unknown; since: number };

/** Restarts the wait when the element sits somewhere new on screen or the
 *  thing it confirms changed. */
export function track(m: Mark, top: number, key: unknown, now: number) {
  if (top !== m.top || key !== m.key) Object.assign(m, { top, key, since: now });
}

/** ready() is false until `ref`'s element (or, without one, the component)
 *  has stayed where it is, showing the same `key`, for SETTLE_MS. Checked on
 *  every render, so a list shrinking around it re-arms it whatever moved.
 *  `waiting` mirrors it for drawing the button (see waitProps) and turns
 *  false, re-rendering, when the wait ends. */
export function useSettled(ref?: RefObject<HTMLElement | null>, key?: unknown) {
  const mark = useRef<Mark>({ top: null, key: undefined, since: 0 });
  const [waiting, setWaiting] = useState(true);
  useLayoutEffect(() => {
    const now = Date.now();
    track(mark.current, ref?.current?.getBoundingClientRect().top ?? 0, key, now);
    const left = waitLeft(mark.current.since, now);
    setWaiting(left > 0);
    if (!left) return;
    const id = setTimeout(() => setWaiting(false), left);
    return () => clearTimeout(id);
  });
  return { ready: () => settled(mark.current.since, Date.now()), waiting };
}
