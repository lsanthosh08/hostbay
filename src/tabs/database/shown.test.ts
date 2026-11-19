import { describe, expect, it } from "vitest";
import { isRunning, type Shown } from "./shown";

const shown = (over: Partial<Shown> = {}): Shown => ({ queryId: "q", table: null, sets: [], done: false, loading: false, ...over });

describe("isRunning", () => {
  it("is false for a cursor paused after a page, though Go still says running", () => {
    expect(isRunning(shown({ state: { queryId: "q", state: "running", rows: 500 } }))).toBe(false);
  });
  it("is true while a page is being read", () => {
    expect(isRunning(shown({ loading: true }))).toBe(true);
  });
  it("is false once cancelled or failed", () => {
    expect(isRunning(shown({ loading: true, state: { queryId: "q", state: "cancelled", rows: 0 } }))).toBe(false);
    expect(isRunning(shown({ loading: true, error: "boom" }))).toBe(false);
    expect(isRunning(null)).toBe(false);
  });
});
