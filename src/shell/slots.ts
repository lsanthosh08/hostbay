// Portal targets for the session layer (KTD16). A terminal's PTY and an RDP
// session live as long as their component, so each session renders once into
// its own detached node, and the tab body only moves that node. React never
// sees the move, so no shell change unmounts a session.

type SlotNode = { className: string; remove(): void };
type SlotHost<N> = { appendChild(n: N): unknown };

const SHOWN = "flex-1 min-h-0 flex flex-col";

export function createSlots<N extends SlotNode & { parentNode: unknown }>(make: () => N) {
  const slots = new Map<string, N>();
  return {
    /** The session's node, made on first use and the same one after that. */
    slot(id: string): N {
      let n = slots.get(id);
      if (!n) {
        n = make();
        n.className = "hidden";
        slots.set(id, n);
      }
      return n;
    },
    /** Moves each shown session's node into its pane, and every other node
     *  into `park`, hidden (KTD40). */
    place(panes: Map<string, SlotHost<N>>, park: SlotHost<N>) {
      for (const [id, n] of slots) {
        const host = panes.get(id) ?? park;
        if (n.parentNode !== host) host.appendChild(n);
        n.className = panes.has(id) ? SHOWN : "hidden";
      }
    },
    /** Forgets closed sessions, once React has unmounted their portals. */
    prune(closed: Iterable<string>) {
      for (const id of closed) {
        slots.get(id)?.remove();
        slots.delete(id);
      }
    },
    ids: () => [...slots.keys()],
  };
}
