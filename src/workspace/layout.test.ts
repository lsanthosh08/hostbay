import { describe, expect, it } from "vitest";
import { leaves, remove, rename, resize, sideAt, split, type Pane } from "./layout";

describe("layout tree", () => {
  it("splits a pane beside its target, in the side's direction", () => {
    expect(split("a", "a", "b", "right")).toEqual({ dir: "row", sizes: [0.5, 0.5], children: ["a", "b"] });
    expect(split("a", "a", "b", "top")).toEqual({ dir: "col", sizes: [0.5, 0.5], children: ["b", "a"] });
  });

  it("joins a split of the same direction, halving the target's share", () => {
    const row: Pane = { dir: "row", sizes: [0.6, 0.4], children: ["a", "b"] };
    expect(split(row, "b", "c", "right")).toEqual({ dir: "row", sizes: [0.6, 0.2, 0.2], children: ["a", "b", "c"] });
    // Across the direction, the target becomes a split of its own.
    expect(split(row, "a", "c", "bottom")).toEqual({
      dir: "row",
      sizes: [0.6, 0.4],
      children: [{ dir: "col", sizes: [0.5, 0.5], children: ["a", "c"] }, "b"],
    });
  });

  it("moves a pane that is already in the tree, so it sits in one pane only", () => {
    const row: Pane = { dir: "row", sizes: [1 / 3, 1 / 3, 1 / 3], children: ["a", "b", "c"] };
    const moved = split(row, "a", "c", "bottom");
    expect(leaves(moved).sort()).toEqual(["a", "b", "c"]);
    expect(moved).toEqual({
      dir: "row",
      sizes: [0.5, 0.5],
      children: [{ dir: "col", sizes: [0.5, 0.5], children: ["a", "c"] }, "b"],
    });
  });

  it("changes nothing when a session is dropped onto the pane it already fills", () => {
    const row: Pane = { dir: "row", sizes: [0.5, 0.5], children: ["a", "b"] };
    expect(split(row, "a", "a", "left")).toBe(row);
    expect(split(row, "missing", "a", "left")).toBe(row);
  });

  it("collapses a split left with one pane, and empties at the last", () => {
    const row: Pane = { dir: "row", sizes: [0.5, 0.5], children: ["a", "b"] };
    expect(remove(row, "b")).toBe("a");
    expect(remove("a", "a")).toBeNull();
    expect(remove(row, "missing")).toBe(row);
  });

  it("flattens a collapsed split into a parent of the same direction", () => {
    const tree: Pane = {
      dir: "row",
      sizes: [0.5, 0.5],
      children: ["a", { dir: "col", sizes: [0.5, 0.5], children: [{ dir: "row", sizes: [0.5, 0.5], children: ["b", "c"] }, "d"] }],
    };
    expect(remove(tree, "d")).toEqual({ dir: "row", sizes: [0.5, 0.25, 0.25], children: ["a", "b", "c"] });
  });

  it("resizes two neighbours, keeping each above the minimum", () => {
    const row: Pane = { dir: "row", sizes: [0.5, 0.5], children: ["a", "b"] };
    const sizes = (n: Pane) => (n as { sizes: number[] }).sizes.map((s) => Math.round(s * 100) / 100);
    expect(sizes(resize(row, [], 0, 0.2))).toEqual([0.7, 0.3]);
    expect(sizes(resize(row, [], 0, 0.9))).toEqual([0.9, 0.1]);
    expect(sizes(resize(row, [], 0, -0.9))).toEqual([0.1, 0.9]);
    const nested: Pane = { dir: "row", sizes: [0.5, 0.5], children: ["a", { dir: "col", sizes: [0.5, 0.5], children: ["b", "c"] }] };
    expect(resize(nested, [1], 0, -0.25)).toEqual({ ...nested, children: ["a", { dir: "col", sizes: [0.25, 0.75], children: ["b", "c"] }] });
  });

  it("drops beside the pane edge nearest the pointer", () => {
    const r = { left: 0, top: 0, width: 200, height: 100 };
    expect(sideAt(r, 10, 50)).toBe("left");
    expect(sideAt(r, 190, 40)).toBe("right");
    expect(sideAt(r, 100, 5)).toBe("top");
    expect(sideAt(r, 120, 95)).toBe("bottom");
  });

  it("renames a pane's session and lists panes in order", () => {
    const row: Pane = { dir: "row", sizes: [0.5, 0.5], children: ["a", { dir: "col", sizes: [0.5, 0.5], children: ["b", "c"] }] };
    expect(leaves(rename(row, "b", "B"))).toEqual(["a", "B", "c"]);
  });
});
