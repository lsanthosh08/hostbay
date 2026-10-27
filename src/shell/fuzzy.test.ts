import { expect, it } from "vitest";
import { fuzzyRank, fuzzyScore } from "./fuzzy";

it('ranks "web-prod" above "network" for "wp"', () => {
  expect(fuzzyRank(["network", "web-prod"], "wp", (s) => s)[0]).toBe("web-prod");
});

it("prefers matches at word starts", () => {
  expect(fuzzyRank(["wordpress", "web-prod"], "wp", (s) => s)).toEqual(["web-prod", "wordpress"]);
});

it("drops items that do not contain the query in order, ignoring case", () => {
  expect(fuzzyScore("pw", "web-prod")).toBeNull();
  expect(fuzzyScore("WEB", "web-prod")).not.toBeNull();
  expect(fuzzyRank(["db1", "web-prod"], "wp", (s) => s)).toEqual(["web-prod"]);
});

it("keeps the original order for an empty query", () => {
  expect(fuzzyRank(["b", "a"], "  ", (s) => s)).toEqual(["b", "a"]);
});
