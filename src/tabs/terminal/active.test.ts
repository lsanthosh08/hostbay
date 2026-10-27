import { describe, expect, it, vi } from "vitest";

const writes: [string, string][] = [];
vi.mock("../../api", () => ({
  ptyWrite: async (id: string, data: string) => void writes.push([id, data]),
}));

import { clearActiveTerminal, sendToActive, setActiveTerminal, type ActiveTerm } from "./active";

const term = (ptyId: string | null): ActiveTerm => ({ sessionId: "s1", ptyId: () => ptyId, bracketed: () => true, focus: () => {} });

describe("sendToActive", () => {
  it("refuses with no focused terminal, or a disconnected one, and queues nothing", async () => {
    expect(await sendToActive("uptime", "run")).toBe("quick.noTerminal");
    const closed = term(null);
    setActiveTerminal(closed);
    expect(await sendToActive("uptime", "run")).toBe("quick.disconnected");
    clearActiveTerminal(closed);
    setActiveTerminal(term("p1")); // a terminal focused later gets nothing from before
    await Promise.resolve();
    expect(writes).toEqual([]);
  });

  it("sends a multi-line command as one bracketed paste followed by Enter", async () => {
    setActiveTerminal(term("p2"));
    expect(await sendToActive("cd /srv\nls", "run")).toBeNull();
    expect(writes).toEqual([["p2", "\x1b[200~cd /srv\rls\x1b[201~\r"]]);
  });
});
