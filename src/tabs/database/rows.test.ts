import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { DbPage, DbTable } from "../../api";
import { addPage, canEdit, CellText, cellText, editable, indentJSON, rowKey } from "./rows";

const page = (over: Partial<DbPage>): DbPage => ({ result: 0, row: 0, rows: [], done: false, ...over });

describe("database rows", () => {
  it("marks NULL, a cut cell and a binary one", () => {
    expect(cellText({ t: "", n: true })).toBe("NULL");
    expect(cellText({ t: "abc", c: true, s: 204800 })).toBe("abc… (200 KiB)");
    expect(cellText({ t: "deadbeef", b: true, s: 4 })).toBe("0xdeadbeef (4 bytes)");
    expect(cellText({ t: "plain" })).toBe("plain");
  });

  it("renders a cell's markup as text, never as HTML", () => {
    const html = renderToStaticMarkup(createElement(CellText, { cell: { t: `<img src=x onerror=alert(1)>` } }));
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img src=x onerror=alert(1)&gt;");
  });

  it("gathers pages into result sets, columns from each set's first page", () => {
    let sets = addPage([], page({ columns: [{ name: "i", type: "INT" }], rows: [[{ t: "1" }]] }));
    sets = addPage(sets, page({ row: 1, rows: [[{ t: "2" }]] }));
    sets = addPage(sets, page({ result: 1, columns: [], rows: [], done: true }));
    expect(sets).toEqual([
      { columns: [{ name: "i", type: "INT" }], rows: [[{ t: "1" }], [{ t: "2" }]] },
      { columns: [], rows: [] },
    ]);
  });

  it("identifies a row only by the table's whole key, from cells shown in full", () => {
    const table: DbTable = { path: ["public", "t"], key: ["id"] };
    const columns = [
      { name: "id", type: "INT" },
      { name: "v", type: "TEXT" },
    ];
    expect(rowKey(table, columns, [{ t: "7" }, { t: "x" }])).toEqual({ id: "7" });
    expect(rowKey(table, columns, [{ t: "7", c: true }, { t: "x" }])).toBeNull();
    expect(rowKey({ ...table, key: ["missing"] }, columns, [{ t: "7" }, { t: "x" }])).toBeNull();
    expect(editable({ t: "x" })).toBe(true);
    expect(editable({ t: "", n: true })).toBe(true);
    expect(editable({ t: "ab", b: true, s: 1 })).toBe(false);
    expect(editable({ t: "ab", c: true, s: 70000 })).toBe(false);
  });

  // The engine names the only columns it changes (a Redis key's value,
  // never the row's own key); a table naming none edits every full cell.
  it("edits only the columns the table names, or every one when it names none", () => {
    const sql: DbTable = { path: ["public", "t"], key: ["id"] };
    const redis: DbTable = { path: ["h"], key: ["field"], edit: ["value"], noNull: true };
    expect(canEdit(sql, "id", { t: "7" })).toBe(true);
    expect(canEdit(sql, "v", { t: "x", c: true, s: 70000 })).toBe(false);
    expect(canEdit(redis, "field", { t: "f1" })).toBe(false);
    expect(canEdit(redis, "value", { t: "a" })).toBe(true);
    expect(canEdit(redis, "value", { t: "a", c: true, s: 70000 })).toBe(false);
  });

  // A document is laid out for editing without being parsed, so a number
  // too large for JavaScript keeps every digit when it is saved back.
  it("indents a document's JSON without touching its values", () => {
    const doc = `{"n":12345678901234567890,"s":"a, {b}: [\\"c\\"]","list":[1,{"x":null}],"empty":{}}`;
    const out = indentJSON(doc);
    expect(out).toBe(
      `{\n  "n": 12345678901234567890,\n  "s": "a, {b}: [\\"c\\"]",\n  "list": [\n    1,\n    {\n      "x": null\n    }\n  ],\n  "empty": {}\n}`
    );
    expect(out.replace(/\s+(?=([^"]*"[^"]*")*[^"]*$)/g, "")).toBe(doc);
  });
});
