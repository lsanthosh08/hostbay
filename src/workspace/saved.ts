// Saved workspaces (KTD40): a split tab's layout with each pane as its
// host's ID, and nothing else. Opening one connects its panes one at a
// time, so trust and password prompts never overlap; nothing connects at
// launch.
import type { Host, WorkspaceNode } from "../api";
import type { Step } from "../connect/router";
import type { Connection } from "../state/sessions";
import { leaves, remove, type Pane } from "./layout";

/** A tab's layout as a saved one. Panes with no saved host (a local shell,
 *  the Files tab) are left out; null when none is left. */
export function toSaved(tree: Pane, hostOf: (sessionId: string) => string | undefined): WorkspaceNode | null {
  const kept = leaves(tree).reduce<Pane | null>((t, id) => (t && !hostOf(id) ? remove(t, id) : t), tree);
  const out = (p: Pane): WorkspaceNode =>
    typeof p === "string" ? { hostId: hostOf(p) } : { dir: p.dir, sizes: p.sizes, children: p.children.map(out) };
  return kept === null ? null : out(kept);
}

/** A saved layout with each pane as a host ID, leaving out (and listing in
 *  missing) hosts deleted since it was saved. */
export function fromSaved(saved: WorkspaceNode, hosts: Host[]): { tree: Pane | null; missing: string[] } {
  const missing: string[] = [];
  const seen = new Set<string>();
  // A host missing here, a pane twice or a split of the wrong shape (from a
  // newer Hostbay) becomes a blank leaf, and blanks are removed below.
  const read = (n: WorkspaceNode): Pane => {
    const kids = n.children ?? [];
    if (!kids.length) {
      const id = n.hostId ?? "";
      if (id && !hosts.some((h) => h.id === id)) missing.push(id);
      if (!id || seen.has(id) || missing.includes(id)) return "";
      seen.add(id);
      return id;
    }
    const sizes = n.sizes?.length === kids.length ? n.sizes : kids.map(() => 1 / kids.length);
    return { dir: n.dir === "col" ? "col" : "row", sizes, children: kids.map(read) };
  };
  let tree: Pane | null = read(saved);
  while (tree !== null && leaves(tree).includes("")) tree = remove(tree, "");
  return { tree, missing };
}

const PROMPTS: Step["kind"][] = ["trust", "vpnCert", "secret", "proxySecret", "unlock"];

/** True while a pane's connect needs the user: it is still dialling, or a
 *  trust, password or unlock prompt is up. */
export const waiting = (c?: Connection) => !!c?.dialing && (!c.step || PROMPTS.includes(c.step.kind));

/** Connects panes in pane order, each once the one before has settled:
 *  live, failed, closed, or its prompt answered or cancelled. dial returns
 *  false for a pane closed before its turn. */
export async function openInOrder(ids: string[], dial: (id: string) => boolean, settled: (id: string) => Promise<void>) {
  for (const id of ids) if (dial(id)) await settled(id);
}
