import { expect, it } from "vitest";
import type { K8sItem } from "../../api";
import { addListPage, listKey, rowsFor, type ListPage } from "./clusters";

const item = (name: string) => ({ name }) as K8sItem;
const empty: ListPage = { key: "", items: [], cont: "", loading: true };

// A row's actions use the tab's current kind, so a pod row left on screen
// under "deployments" would delete or scale a deployment of the pod's name.
it("shows no rows loaded for another kind or namespace", () => {
  const pods = addListPage(empty, listKey("pods", "web"), "", { items: [item("api-0")], continue: "" });
  expect(rowsFor(pods, listKey("pods", "web"))).toEqual([item("api-0")]);
  expect(rowsFor(pods, listKey("deployments", "web"))).toEqual([]);
  expect(rowsFor(pods, listKey("pods", "db"))).toEqual([]);
});

it("adds a next page to the same list, and starts over for another", () => {
  const key = listKey("pods", "");
  const first = addListPage(empty, key, "", { items: [item("a")], continue: "c1" });
  expect(addListPage(first, key, "c1", { items: [item("b")], continue: "" })).toMatchObject({ items: [item("a"), item("b")], cont: "" });
  expect(addListPage(first, listKey("nodes", ""), "c1", { items: [item("n1")], continue: "" }).items).toEqual([item("n1")]);
});
