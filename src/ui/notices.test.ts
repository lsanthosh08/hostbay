import { beforeEach, expect, it } from "vitest";
import { notices } from "./notices";

beforeEach(() => notices.clear(true));

it("keeps every notice newest first and counts the unread", () => {
  notices.add({ id: "a", kind: "info", msg: "one" });
  notices.add({ id: "b", kind: "error", msg: "two" });
  const s = notices.get();
  expect(s.list.map((n) => n.msg)).toEqual(["two", "one"]);
  expect(s.unread).toBe(2);
  notices.read();
  expect(notices.get().unread).toBe(0);
});

// An approval stays marked waiting until it leaves the tray; Clear keeps
// what still waits, so a pending request is never lost from the list.
it("settles an approval and keeps waiting ones on clear", () => {
  notices.add({ id: "x", kind: "approval", msg: "run uptime", pending: true });
  notices.add({ id: "y", kind: "approval", msg: "run df", pending: true });
  notices.add({ id: "z", kind: "info", msg: "saved" });
  notices.settle("x");
  expect(notices.get().list.filter((n) => n.pending).map((n) => n.id)).toEqual(["y"]);
  notices.clear();
  expect(notices.get().list.map((n) => n.id)).toEqual(["y"]);
});

it("drops the oldest past the cap", () => {
  for (let i = 0; i < 205; i++) notices.add({ id: String(i), kind: "info", msg: String(i) });
  const l = notices.get().list;
  expect(l.length).toBe(200);
  expect(l[0].id).toBe("204");
});
