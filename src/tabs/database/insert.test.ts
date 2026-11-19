import { expect, it } from "vitest";
import { insertSet } from "./insert";

it("sets only the columns given a value or NULL", () => {
  const cols = [
    { name: "id", type: "int" },
    { name: "name", type: "text" },
    { name: "note", type: "text" },
  ];
  const set = insertSet(cols, [
    { mode: "default", value: "7" },
    { mode: "value", value: "" },
    { mode: "null", value: "x" },
  ]);
  expect(set).toEqual({ name: "", note: null });
});
