import { expect, it } from "vitest";
import { addTab, closeTab, completions, renameTab } from "./tabs";

const name = (n: number) => `Query ${n}`;

it("adds tabs by the next free number", () => {
  let { tabs, id } = addTab([], name);
  expect(tabs).toEqual([{ id: "1", name: "Query 1", text: "" }]);
  ({ tabs, id } = addTab(tabs, name));
  expect(id).toBe("2");
  ({ tabs } = closeTab(tabs, "1", "2"));
  expect(addTab(tabs, name).id).toBe("3");
});

it("closes a tab, keeping the last and moving the active one", () => {
  const tabs = [1, 2, 3].map((n) => ({ id: String(n), name: name(n), text: "" }));
  expect(closeTab(tabs, "2", "2")).toMatchObject({ active: "3" });
  expect(closeTab(tabs, "3", "3")).toMatchObject({ active: "2" });
  expect(closeTab(tabs, "1", "3").active).toBe("3");
  expect(closeTab(tabs.slice(0, 1), "1", "1").tabs).toHaveLength(1);
});

it("renames a tab, keeping the old name when the new one is blank", () => {
  const tabs = [{ id: "1", name: "Query 1", text: "" }];
  expect(renameTab(tabs, "1", "  users ")[0].name).toBe("users");
  expect(renameTab(tabs, "1", " ")[0].name).toBe("Query 1");
});

it("completes tables by full and own name, and columns with their tables", () => {
  const items = completions([
    { name: "public.users", columns: ["id", "name"] },
    { name: "public.orders", columns: ["id", "users"] },
  ]);
  expect(items).toContainEqual({ label: "public.users", kind: "table", detail: "public.users" });
  expect(items).toContainEqual({ label: "users", kind: "table", detail: "public.users" });
  expect(items).toContainEqual({ label: "id", kind: "column", detail: "users, orders" });
  // A column named like a table is offered once, as the table.
  expect(items.filter((i) => i.label === "users")).toHaveLength(1);
});
