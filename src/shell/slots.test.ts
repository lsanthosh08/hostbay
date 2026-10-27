import { expect, it } from "vitest";
import { createSlots } from "./slots";

// Stand-ins for DOM nodes: the node env has no document. A node counts its
// removals, which is what unmounts a portal's content.
class Node {
  parentNode: Host | null = null;
  className = "";
  removed = 0;
  remove() {
    this.parentNode = null;
    this.removed++;
  }
}
class Host {
  appendChild(n: Node) {
    n.parentNode = this;
  }
}

it("keeps one portal target per session while it moves from a tab into a split pane", () => {
  const slots = createSlots(() => new Node());
  const top = slots.slot("top");
  const park = new Host();
  const tab = new Host();
  slots.place(new Map([["top", tab]]), park);
  expect(top.parentNode).toBe(tab);
  // Dragged into a split: two panes now, the same node in the second.
  const left = new Host();
  const right = new Host();
  slots.slot("web");
  slots.place(
    new Map([
      ["web", left],
      ["top", right],
    ]),
    park
  );
  // Same node, never removed: React keeps the portal (and the PTY it owns).
  expect(slots.slot("top")).toBe(top);
  expect(top.parentNode).toBe(right);
  expect(top.removed).toBe(0);
  expect(top.className).not.toBe("hidden");
});

it("shows only the sessions in a visible pane, and parks the rest", () => {
  const slots = createSlots(() => new Node());
  const park = new Host();
  const pane = new Host();
  slots.slot("a");
  slots.slot("b");
  slots.place(new Map([["b", pane]]), park);
  expect(slots.slot("a")).toMatchObject({ className: "hidden", parentNode: park });
  expect(slots.slot("b").className).not.toBe("hidden");
  slots.place(new Map(), park);
  expect(slots.slot("b")).toMatchObject({ className: "hidden", parentNode: park });
});

it("drops the targets of closed sessions", () => {
  const slots = createSlots(() => new Node());
  const park = new Host();
  const a = slots.slot("a");
  slots.slot("b");
  slots.place(new Map([["a", new Host()]]), park);
  slots.prune(["a"]);
  expect(a.parentNode).toBeNull();
  expect(slots.slot("a")).not.toBe(a);
});
