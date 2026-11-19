import { expect, it } from "vitest";
import { shouldLoad } from "./ObjectTree";

it("loads a level again on expand after its load failed", () => {
  expect(shouldLoad(undefined)).toBe(true);
  expect(shouldLoad({ nodes: [], more: false, loading: false, error: "timeout" })).toBe(true);
  expect(shouldLoad({ nodes: [], more: false, loading: true, error: "timeout" })).toBe(false);
  expect(shouldLoad({ nodes: [], more: false, loading: false })).toBe(false);
});
