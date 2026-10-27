import { describe, expect, it } from "vitest";
import { conversations, isSuperseded } from "./conversation";

// A fake backend whose starts the test resolves by hand.
function backend() {
  const starts: ((id: string) => void)[] = [];
  const closed: string[] = [];
  const listening: string[] = [];
  return {
    starts,
    closed,
    listening,
    b: {
      start: () => new Promise<string>((res) => starts.push(res)),
      listen: async (id: string) => {
        listening.push(id);
        return () => listening.splice(listening.indexOf(id), 1);
      },
      close: (id: string) => closed.push(id),
    },
  };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

describe("the sidebar's conversation", () => {
  // Enter twice, or Enter then Send, while the first start is in flight.
  it("starts once for overlapping sends", async () => {
    const f = backend();
    const c = conversations(f.b);
    const a = c.ensure("p1", "");
    const b = c.ensure("p1", "");
    await tick();
    expect(f.starts).toHaveLength(1);
    f.starts[0]("conv1");
    expect(await a).toBe("conv1");
    expect(await b).toBe("conv1");
    expect(f.listening).toEqual(["conv1"]);
    expect(await c.ensure("p1", "")).toBe("conv1");
    expect(f.starts).toHaveLength(1);
  });

  // New, a provider change or unmount while the start is in flight.
  it("closes a conversation that was ended while it started", async () => {
    const f = backend();
    const c = conversations(f.b);
    const a = c.ensure("p1", "");
    await tick();
    c.end();
    f.starts[0]("conv1");
    await expect(a).rejects.toSatisfy(isSuperseded);
    expect(f.closed).toEqual(["conv1"]);
    expect(f.listening).toEqual([]);
    expect(c.current()).toBeNull();
  });

  it("ends the open one before starting for another provider", async () => {
    const f = backend();
    const c = conversations(f.b);
    const a = c.ensure("p1", "");
    await tick();
    f.starts[0]("conv1");
    await a;
    const b = c.ensure("p2", "");
    expect(f.closed).toEqual(["conv1"]);
    await tick();
    f.starts[1]("conv2");
    expect(await b).toBe("conv2");
    expect(f.listening).toEqual(["conv2"]);
  });
});
