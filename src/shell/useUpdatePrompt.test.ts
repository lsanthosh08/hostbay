import { describe, expect, it } from "vitest";
import type { UpdateStatus } from "../api";
import { offerUpdate } from "./useUpdatePrompt";

const st = (over: Partial<UpdateStatus> = {}): UpdateStatus => ({
  current: "0.1.2",
  signed: true,
  lastCheck: "2026-11-18T09:00:00Z",
  available: "0.1.3",
  pageUrl: "https://github.com/x/releases/tag/v0.1.3",
  canInstall: true,
  installed: false,
  error: "",
  ...over,
});

describe("offerUpdate", () => {
  it("offers a newer version the last check found", () => {
    expect(offerUpdate(st(), null)).toBe(true);
  });
  it("offers nothing when no newer version is known", () => {
    expect(offerUpdate(st({ available: "" }), null)).toBe(false);
  });
  it("does not offer a version the user put off with Later", () => {
    expect(offerUpdate(st(), "0.1.3")).toBe(false);
    expect(offerUpdate(st({ available: "0.1.4" }), "0.1.3")).toBe(true);
  });
  it("does not offer again once it is installed and waits for a restart", () => {
    expect(offerUpdate(st({ installed: true }), null)).toBe(false);
  });
});
