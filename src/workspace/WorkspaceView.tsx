import { Fragment, useEffect, useLayoutEffect, useRef } from "react";
import { layoutOf, useSessions } from "../state/sessions";
import type { Slots } from "../shell/SessionLayer";
import { DND_TAB } from "../ui/Tabs";
import { Divider } from "./Divider";
import { sideAt, type Pane } from "./layout";

/** The active tab's body: one pane per session of its layout, or one for a
 *  plain tab. Each pane is an empty host the session's node moves into
 *  (src/shell/slots.ts), so a split never unmounts a session (KTD40). A
 *  session tab dropped on a pane splits it at the nearest edge. */
export function WorkspaceView({ slots, className }: { slots: Slots; className: string }) {
  const s = useSessions();
  const layout = layoutOf(s, s.activeId);
  const tree: Pane | null = layout?.tree ?? s.active?.sessionId ?? null;
  const root = useRef<HTMLDivElement>(null);
  const park = useRef<HTMLDivElement>(null);
  const live = useRef(s);
  live.current = s;

  useLayoutEffect(() => {
    const panes = new Map<string, HTMLElement>();
    root.current!.querySelectorAll<HTMLElement>("[data-pane]").forEach((el) => panes.set(el.dataset.pane!, el));
    slots.place(panes, park.current!);
  });

  // A pane focused by keyboard takes the keys too, unless focus is already
  // in it.
  const focused = layout && s.activeId;
  useEffect(() => {
    if (!focused) return;
    const pane = root.current!.querySelector<HTMLElement>(`[data-pane="${CSS.escape(focused)}"]`);
    if (pane && !pane.contains(document.activeElement)) pane.querySelector<HTMLElement>("textarea, input, [tabindex]")?.focus();
  }, [focused]);

  // Session content is portalled from the session layer, so React events
  // from it never reach a pane; these listen on the DOM instead. Nothing
  // React renders changes during a drag (WKWebView cancels it): the drop
  // hint is a data attribute only.
  useEffect(() => {
    const el = root.current!;
    const paneOf = (e: Event) => (e.target instanceof Element ? e.target.closest<HTMLElement>("[data-pane]") : null);
    const hint = (pane: HTMLElement | null, side?: string) => {
      el.querySelectorAll<HTMLElement>("[data-drop]").forEach((p) => {
        if (p !== pane) delete p.dataset.drop;
      });
      if (pane && side) pane.dataset.drop = side;
    };
    const focus = (e: Event) => {
      const id = paneOf(e)?.dataset.pane;
      if (id && id !== live.current.activeId) live.current.activate(id);
    };
    const over = (e: DragEvent) => {
      const pane = paneOf(e);
      if (!pane || !e.dataTransfer?.types.includes(DND_TAB)) return;
      e.preventDefault();
      hint(pane, sideAt(pane.getBoundingClientRect(), e.clientX, e.clientY));
    };
    const leave = (e: DragEvent) => {
      if (!(e.relatedTarget instanceof Node) || !el.contains(e.relatedTarget)) hint(null);
    };
    const drop = (e: DragEvent) => {
      const pane = paneOf(e);
      const from = e.dataTransfer?.getData(DND_TAB);
      hint(null);
      if (!pane || !from) return;
      e.preventDefault();
      live.current.split(from, pane.dataset.pane!, sideAt(pane.getBoundingClientRect(), e.clientX, e.clientY));
    };
    const clear = () => hint(null);
    el.addEventListener("pointerdown", focus, true);
    el.addEventListener("focusin", focus);
    el.addEventListener("dragover", over);
    el.addEventListener("dragleave", leave);
    el.addEventListener("drop", drop);
    window.addEventListener("dragend", clear);
    return () => {
      el.removeEventListener("pointerdown", focus, true);
      el.removeEventListener("focusin", focus);
      el.removeEventListener("dragover", over);
      el.removeEventListener("dragleave", leave);
      el.removeEventListener("drop", drop);
      window.removeEventListener("dragend", clear);
    };
  }, []);

  const view = (n: Pane, path: number[]) => {
    if (typeof n === "string")
      return (
        <div
          data-pane={n}
          className={
            "flex-1 min-w-0 min-h-0 flex flex-col relative " +
            (layout && n === s.activeId ? "outline outline-1 -outline-offset-1 outline-accent" : "")
          }
        />
      );
    return (
      <div className={`flex-1 min-w-0 min-h-0 flex ${n.dir === "row" ? "flex-row" : "flex-col"}`}>
        {n.children.map((c, i) => (
          <Fragment key={typeof c === "string" ? c : `split-${i}`}>
            {i > 0 && (
              <Divider dir={n.dir} share={n.sizes[i - 1]} onResize={(d) => s.resizePanes(layout!.id, path, i - 1, d)} />
            )}
            <div className="min-w-0 min-h-0 flex" style={{ flex: `${n.sizes[i]} 1 0` }}>
              {view(c, [...path, i])}
            </div>
          </Fragment>
        ))}
      </div>
    );
  };

  return (
    <div ref={root} className={className}>
      {tree !== null && view(tree, [])}
      <div ref={park} className="hidden" />
    </div>
  );
}
