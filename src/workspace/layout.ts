// A workspace tab's split panes as a pure tree (KTD40). A leaf is a session
// ID; a split lays its children out in a row or a column, each with its
// share of the split in sizes (they sum to 1). The views only render it.

export type Split = { dir: "row" | "col"; sizes: number[]; children: Pane[] };
export type Pane = string | Split;
export type Side = "left" | "right" | "top" | "bottom";

/** The smallest share a resize leaves a pane. */
export const MIN_SIZE = 0.1;

/** The tree's sessions in pane order. */
export const leaves = (n: Pane): string[] => (typeof n === "string" ? [n] : n.children.flatMap(leaves));

/** Puts session `id` beside pane `target`, taking it out of wherever it was.
 *  Dropping a session onto its own pane, or beside a pane not in the tree,
 *  returns the tree unchanged. */
export function split(n: Pane, target: string, id: string, side: Side): Pane {
  if (id === target || !leaves(n).includes(target)) return n;
  const dir = side === "left" || side === "right" ? "row" : "col";
  const before = side === "left" || side === "top";
  const insert = (p: Pane): Pane => {
    if (typeof p === "string")
      return p === target ? { dir, sizes: [0.5, 0.5], children: before ? [id, p] : [p, id] } : p;
    const i = p.children.indexOf(target);
    if (i >= 0 && p.dir === dir) {
      // Joins this split: the target gives the new pane half its share.
      const children = [...p.children];
      const sizes = [...p.sizes];
      sizes.splice(i, 1, sizes[i] / 2, sizes[i] / 2);
      children.splice(before ? i : i + 1, 0, id);
      return { dir, sizes, children };
    }
    return { ...p, children: p.children.map(insert) };
  };
  return insert(remove(n, id) ?? target);
}

/** Takes session `id` out of the tree. A split left with one child becomes
 *  that child, and one inside a split of the same direction joins it; the
 *  last pane gone leaves null. */
export function remove(n: Pane, id: string): Pane | null {
  if (!leaves(n).includes(id)) return n;
  if (typeof n === "string") return null;
  const children: Pane[] = [];
  const sizes: number[] = [];
  n.children.forEach((c, i) => {
    const r = remove(c, id);
    if (r === null) return;
    if (typeof r !== "string" && r.dir === n.dir) {
      children.push(...r.children);
      sizes.push(...r.sizes.map((s) => s * n.sizes[i]));
    } else {
      children.push(r);
      sizes.push(n.sizes[i]);
    }
  });
  if (children.length < 2) return children[0] ?? null;
  const total = sizes.reduce((a, b) => a + b, 0);
  return { dir: n.dir, sizes: sizes.map((s) => s / total), children };
}

/** The edge of a pane's rect nearest the point, where a drop splits it. */
export function sideAt(r: { left: number; top: number; width: number; height: number }, x: number, y: number): Side {
  const dx = (x - r.left) / r.width;
  const dy = (y - r.top) / r.height;
  const near: [number, Side][] = [[dx, "left"], [1 - dx, "right"], [dy, "top"], [1 - dy, "bottom"]];
  return near.reduce((a, b) => (b[0] < a[0] ? b : a))[1];
}

/** The tree with session `from` now called `to`. */
export const rename = (n: Pane, from: string, to: string): Pane =>
  typeof n === "string" ? (n === from ? to : n) : { ...n, children: n.children.map((c) => rename(c, from, to)) };

/** Moves the border between children i and i+1 of the split at `path`
 *  (child indexes from the root) by `delta` of that split, keeping both at
 *  MIN_SIZE or more. */
export function resize(n: Pane, path: number[], i: number, delta: number): Pane {
  if (typeof n === "string") return n;
  if (path.length) {
    const child = resize(n.children[path[0]], path.slice(1), i, delta);
    return child === n.children[path[0]] ? n : { ...n, children: n.children.map((c, k) => (k === path[0] ? child : c)) };
  }
  const sizes = [...n.sizes];
  const pair = sizes[i] + sizes[i + 1];
  if (!(pair >= 2 * MIN_SIZE)) return n;
  sizes[i] = Math.min(pair - MIN_SIZE, Math.max(MIN_SIZE, sizes[i] + delta));
  sizes[i + 1] = pair - sizes[i];
  return sizes[i] === n.sizes[i] ? n : { ...n, sizes };
}
