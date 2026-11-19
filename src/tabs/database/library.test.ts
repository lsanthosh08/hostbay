import { expect, it } from "vitest";
import { firstLine, matching } from "./library";

it("filters by every word, in any order and case", () => {
  const qs = ["SELECT * FROM users", "select name from orders", "DELETE FROM users"];
  expect(matching(qs, "users select", (q) => q)).toEqual(["SELECT * FROM users"]);
  expect(matching(qs, "", (q) => q)).toEqual(qs);
});

it("shows a request by its first line", () => {
  expect(firstLine("\n  SELECT 1\nFROM t")).toBe("SELECT 1");
});
