import { describe, expect, it } from "vitest";
import type { Host } from "../api";
import { initialSessions, sessionsReducer, type Connection, type SessionsAction } from "../state/sessions";
import { fromSaved, openInOrder, toSaved, waiting } from "./saved";

const host = (id: string): Host => ({ id, alias: id, hostname: `${id}.lan`, port: 22, user: "u", identityFile: "", tags: [] });
const tick = () => new Promise((r) => setTimeout(r, 0));

describe("saved workspaces", () => {
  it("saves each pane as its host's ID, leaving out panes with no saved host", () => {
    const hostOf = (id: string) => ({ s1: "web", s2: "db" })[id];
    const tree = { dir: "row" as const, sizes: [0.5, 0.5], children: ["s1", { dir: "col" as const, sizes: [0.5, 0.5], children: ["shell", "s2"] }] };
    expect(toSaved(tree, hostOf)).toEqual({ dir: "row", sizes: [0.5, 0.5], children: [{ hostId: "web" }, { hostId: "db" }] });
    // A tab of local shells only saves nothing: no empty layout is saved.
    expect(toSaved("shell", hostOf)).toBeNull();
  });

  it("skips and lists a host deleted since the workspace was saved", () => {
    const saved = { dir: "row", sizes: [0.5, 0.5], children: [{ hostId: "web" }, { dir: "col", sizes: [0.5, 0.5], children: [{ hostId: "gone" }, { hostId: "db" }] }] };
    expect(fromSaved(saved, [host("web"), host("db")])).toEqual({
      tree: { dir: "row", sizes: [0.5, 0.5], children: ["web", "db"] },
      missing: ["gone"],
    });
    expect(fromSaved({ hostId: "gone" }, [])).toEqual({ tree: null, missing: ["gone"] });
  });

  // A workspace with a host needing a trust prompt and one needing a
  // password: the prompts come one at a time in pane order, and a
  // cancelled prompt leaves its pane on Retry while the rest opens.
  it("connects its panes one at a time, waiting on each prompt", async () => {
    let state = ["trust", "secret"].reduce(
      (s, id) => sessionsReducer(s, { type: "open", conn: { sessionId: id, host: host(id), dialing: true } }),
      initialSessions
    );
    state = sessionsReducer(state, { type: "layout", tree: { dir: "row", sizes: [0.5, 0.5], children: ["trust", "secret"] } });
    const listeners: (() => void)[] = [];
    const dispatch = (a: SessionsAction) => {
      state = sessionsReducer(state, a);
      listeners.splice(0).forEach((l) => l());
    };
    const conn = (id: string): Connection | undefined => state.sessions.find((c) => c.sessionId === id);
    const settled = (id: string) =>
      new Promise<void>((done) => {
        const check = () => (waiting(conn(id)) ? listeners.push(check) : done());
        check();
      });
    const dialed: string[] = [];
    const dial = (id: string) => {
      dialed.push(id);
      const step =
        id === "trust"
          ? { kind: "trust" as const, message: "unknown key", prompt: { requestId: "r1" } as never }
          : { kind: "secret" as const, message: "password needed", host: host(id) };
      dispatch({ type: "step", sessionId: id, step });
      return true;
    };

    const done = openInOrder(["trust", "secret"], dial, settled);
    await tick();
    // The first pane's trust prompt is up; the second pane has not dialled.
    expect(dialed).toEqual(["trust"]);
    expect(conn("secret")?.step).toBeUndefined();
    // The user cancels it: that pane shows Retry and the next one dials.
    dispatch({ type: "step", sessionId: "trust", step: { kind: "failure", message: "Cancelled" } });
    await tick();
    expect(dialed).toEqual(["trust", "secret"]);
    expect(conn("trust")).toMatchObject({ dialing: true, step: { kind: "failure" } });
    expect(conn("secret")?.step?.kind).toBe("secret");
    // The password goes in and that pane connects.
    dispatch({ type: "dialed", from: "secret", sessionId: "s-live" });
    await done;
    expect(state.layouts[0].tree).toMatchObject({ children: ["trust", "s-live"] });
  });
});
