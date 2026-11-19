import { useLayoutEffect, useRef, type RefObject } from "react";

/** How long a confirm button must sit still before it takes a click. A
 *  button that appears, or moves, under a click aimed at something else
 *  (the next card in the AI approval tray, the next queued trust prompt)
 *  would otherwise confirm what the user never read. */
export const SETTLE_MS = 600;
export const settled = (since: number, now: number) => now - since >= SETTLE_MS;

type Mark = { top: number | null; key: unknown; since: number };

/** Restarts the wait when the element sits somewhere new on screen or the
 *  thing it confirms changed. */
export function track(m: Mark, top: number, key: unknown, now: number) {
  if (top !== m.top || key !== m.key) Object.assign(m, { top, key, since: now });
}

/** ready() is false until `ref`'s element (or, without one, the component)
 *  has stayed where it is, showing the same `key`, for SETTLE_MS. Checked on
 *  every render, so a list shrinking around it re-arms it whatever moved. */
export function useSettled(ref?: RefObject<HTMLElement | null>, key?: unknown) {
  const mark = useRef<Mark>({ top: null, key: undefined, since: 0 });
  useLayoutEffect(() => track(mark.current, ref?.current?.getBoundingClientRect().top ?? 0, key, Date.now()));
  return () => settled(mark.current.since, Date.now());
}
