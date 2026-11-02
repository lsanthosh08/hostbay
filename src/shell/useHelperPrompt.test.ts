import { describe, expect, it } from "vitest";
import { askDue } from "./useHelperPrompt";

describe("askDue", () => {
  it("asks at start, since nothing was declined", () => {
    expect(askDue(false, 0, Date.now())).toBe(true);
  });
  it("does not stack a second question on an open one", () => {
    expect(askDue(true, 0, Date.now())).toBe(false);
  });
  it("waits after a Not now, then asks again", () => {
    const t0 = 1_000_000;
    expect(askDue(false, t0, t0 + 60_000)).toBe(false);
    expect(askDue(false, t0, t0 + 10 * 60_000)).toBe(true);
  });
});
