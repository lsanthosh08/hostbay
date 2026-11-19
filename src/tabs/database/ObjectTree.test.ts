import { expect, it } from "vitest";
import { matches, shouldLoad } from "./ObjectTree";

it("loads a level again on expand after its load failed", () => {
  expect(shouldLoad(undefined)).toBe(true);
  expect(shouldLoad({ nodes: [], more: false, loading: false, error: "timeout" })).toBe(true);
  expect(shouldLoad({ nodes: [], more: false, loading: true, error: "timeout" })).toBe(false);
  expect(shouldLoad({ nodes: [], more: false, loading: false })).toBe(false);
});

it("filters by name through the levels loaded so far", () => {
  const node = (name: string, path: string[]) => ({ kind: "schema", name, path, hasChildren: true });
  const sales = node("sales", ["sales"]);
  const hr = node("hr", ["hr"]);
  const levels = {
    [JSON.stringify(["sales"])]: { nodes: [node("Orders", ["sales", "Orders"])], more: false, loading: false },
  };
  expect(matches(levels, sales, "order")).toBe(true);
  expect(matches(levels, hr, "order")).toBe(false);
  expect(matches(levels, hr, "")).toBe(true);
  expect(matches(levels, hr, "hr")).toBe(true);
});
