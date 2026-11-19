import { expect, it } from "vitest";
import { nextSort, ready } from "./browse";
import browseBar from "./BrowseBar.tsx?raw";

it("cycles a header's sort: ascending, descending, none", () => {
  const a = nextSort({}, "name");
  expect(a).toMatchObject({ sort: "name", desc: false });
  const d = nextSort(a, "name");
  expect(d).toMatchObject({ sort: "name", desc: true });
  expect(nextSort(d, "name").sort).toBeUndefined();
  expect(nextSort(d, "id")).toMatchObject({ sort: "id", desc: false });
});

it("keeps filters on other fields when sorting", () => {
  const f = [{ column: "n", op: "null" as const, value: "" }];
  expect(nextSort({ filters: f }, "n").filters).toBe(f);
});

it("leaves out value filters still empty, but keeps the NULL tests", () => {
  expect(
    ready([
      { column: "a", op: "contains", value: "" },
      { column: "b", op: "null", value: "" },
      { column: "c", op: "equals", value: "0" },
    ]).map((f) => f.column)
  ).toEqual(["b", "c"]);
});

// The browse bar is a <form> whose submit applies the filters, and a
// <button> with no type submits its form: every button names its type,
// so Add filter, Clear, Add row and Count rows do not reload the table.
it("gives every browse bar button a type", () => {
  const tags = browseBar.match(/<Button\b[^>]*>/g) ?? [];
  expect(tags.length).toBeGreaterThan(0);
  expect(tags.filter((tag) => !/\btype="(button|submit)"/.test(tag))).toEqual([]);
});
