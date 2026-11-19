import { describe, expect, it } from "vitest";
import { cancelled, clip, failed, isRunning, type Shown } from "./shown";

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

describe("failed", () => {
  it("shows a cancel as cancelled, not as an error", () => {
    const s = failed(shown({ loading: true }), { message: "context canceled", details: { category: "cancelled" } });
    expect(s.error).toBeUndefined();
    expect(s.state?.state).toBe("cancelled");
    expect(isRunning(s)).toBe(false);
  });
  it("keeps any other message", () => {
    expect(failed(shown({ loading: true }), { message: "syntax error", details: { category: "syntax" } }).error).toBe("syntax error");
  });
});

describe("cancelled", () => {
  it("stops a request still opening, before it has a query ID", () => {
    const s = cancelled(shown({ queryId: "", loading: true }));
    expect(isRunning(s)).toBe(false);
    expect(s.state?.state).toBe("cancelled");
  });
});

it("clips a long statement quoted in the confirmation", () => {
  expect(clip("abc", 5)).toBe("abc");
  expect(clip("abcdefgh", 5)).toBe("abcde…");
});
