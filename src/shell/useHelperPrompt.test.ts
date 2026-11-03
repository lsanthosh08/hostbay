import { describe, expect, it } from "vitest";
import { askDue, helperAsk } from "./useHelperPrompt";

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

describe("helperAsk", () => {
  it("asks to reinstall an out-of-date helper whatever the PAC needs", () => {
    expect(helperAsk("reinstall")).toBe("reinstall");
  });
  it("asks to install a missing helper only once a connected VPN's sites wait on it", () => {
    expect(helperAsk("missing", "helper")).toBe("install");
    expect(helperAsk("missing", "off")).toBeNull();
    expect(helperAsk("ready", "set")).toBeNull();
  });
});
