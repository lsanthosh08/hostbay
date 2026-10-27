import { expect, it } from "vitest";
import type { MonitorProc } from "../../api";
import { sortProcs, sparkPoints } from "./monitor";

const p = (pid: number, cpu: number, rss: number, user = "u", command = "x"): MonitorProc => ({ pid, user, cpu, rss, command });

it("sorts processes by the parsed number, not its text", () => {
  const procs = [p(1, 9.5, 100), p(2, 10.2, 50), p(3, 0.4, 2000)];
  expect(sortProcs(procs, "cpu", "desc").map((x) => x.pid)).toEqual([2, 1, 3]);
  expect(sortProcs(procs, "rss", "asc").map((x) => x.pid)).toEqual([2, 1, 3]);
  expect(sortProcs(procs, "pid", "desc").map((x) => x.pid)).toEqual([3, 2, 1]);
  expect(procs.map((x) => x.pid)).toEqual([1, 2, 3]); // the input is left alone
});

it("sorts text columns by text", () => {
  const procs = [p(1, 0, 0, "zed"), p(2, 0, 0, "amy")];
  expect(sortProcs(procs, "user", "asc").map((x) => x.pid)).toEqual([2, 1]);
});

it("scales sparkline points into the box, highest at the top", () => {
  expect(sparkPoints([0, 50, 100], 100, 10, 100)).toBe("0,10 50,5 100,0");
  // With no fixed top the largest value fills the box; all zeros stay flat.
  expect(sparkPoints([2, 4], 10, 10)).toBe("0,5 10,0");
  expect(sparkPoints([0, 0], 10, 10)).toBe("0,10 10,10");
  expect(sparkPoints([7], 10, 10)).toBe("");
});
