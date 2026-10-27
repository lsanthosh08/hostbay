import { describe, expect, it } from "vitest";
import type { TransferEvent } from "../api";
import { initialTransfers, summary, transfersReducer, type TransfersState } from "./transfers";

const ev = (over: Partial<TransferEvent>): TransferEvent => ({
  taskId: "t1",
  state: "running",
  bytes: 0,
  total: 100,
  files: 0,
  fileCount: 2,
  ...over,
});
const start = (s: TransfersState, taskId = "t1") =>
  transfersReducer(s, { type: "start", taskId, label: "2 items to /srv", to: "files-1", toDir: "/srv" });

describe("transfers", () => {
  it("keeps the label it started with as events arrive, even an event that came first", () => {
    let s = transfersReducer(initialTransfers, { type: "event", event: ev({ bytes: 40 }) });
    s = start(s);
    s = transfersReducer(s, { type: "event", event: ev({ bytes: 60, current: "/srv/b" }) });
    expect(s.tasks).toHaveLength(1);
    expect(s.tasks[0]).toMatchObject({ label: "2 items to /srv", bytes: 60, current: "/srv/b", to: "files-1" });
  });

  it("counts running tasks and the ones waiting on an answer", () => {
    let s = start(start(initialTransfers), "t2");
    s = transfersReducer(s, { type: "event", event: ev({ taskId: "t2", state: "conflict", conflict: { requestId: "q", path: "/srv/a", size: 1, mtime: 0 } }) });
    expect(summary(s)).toEqual({ active: 2, waiting: 1 });
    s = transfersReducer(s, { type: "event", event: ev({ state: "done", bytes: 100, files: 2, skipped: ["/srv/a"] }) });
    expect(summary(s)).toEqual({ active: 1, waiting: 1 });
    expect(s.tasks[0].skipped).toEqual(["/srv/a"]);
  });

  it("drops a conflict once the task runs again", () => {
    let s = transfersReducer(start(initialTransfers), {
      type: "event",
      event: ev({ state: "conflict", conflict: { requestId: "q", path: "/srv/a", size: 1, mtime: 0 } }),
    });
    s = transfersReducer(s, { type: "event", event: ev({ state: "running" }) });
    expect(s.tasks[0].conflict).toBeUndefined();
  });

  it("clears finished tasks only", () => {
    let s = start(start(initialTransfers), "t2");
    s = transfersReducer(s, { type: "event", event: ev({ state: "cancelled" }) });
    s = transfersReducer(s, { type: "clear" });
    expect(s.tasks.map((x) => x.taskId)).toEqual(["t2"]);
  });

  it("notes each finished task's destination so its pane can list it again", () => {
    let s = start(initialTransfers);
    s = transfersReducer(s, { type: "event", event: ev({ state: "failed", error: "disk full" }) });
    expect(s.landed).toEqual({ to: "files-1", toDir: "/srv", n: 1 });
  });

  it("notes the destination of a task that finished before TransferStart returned", () => {
    let s = transfersReducer(initialTransfers, { type: "event", event: ev({ bytes: 40 }) });
    s = transfersReducer(s, { type: "event", event: ev({ state: "done", bytes: 100, files: 2 }) });
    s = start(s);
    expect(s.landed).toMatchObject({ to: "files-1", toDir: "/srv" });
  });
});
