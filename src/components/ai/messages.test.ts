import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { AIEvent, AIPending, AISkill } from "../../api";
import { ApprovalCard } from "./ApprovalCard";
import { errorHint, initialChat, reduce, RichText, splitFences, type Chat } from "./messages";
import { SlashMenu, slashMatches } from "./SlashMenu";

const run = (...evs: (AIEvent | { type: "user"; text: string })[]): Chat => evs.reduce(reduce, initialChat);

describe("the AI conversation reducer", () => {
  it("joins streamed text into one reply, after the user's message", () => {
    const c = run({ type: "user", text: "disk?" }, { type: "state", state: "thinking" }, { type: "text", text: "Let me " }, { type: "text", text: "check." });
    expect(c.items).toEqual([
      { kind: "user", text: "disk?" },
      { kind: "assistant", text: "Let me check." },
    ]);
    expect(c.busy).toBe("thinking");
  });

  it("puts a command's result on its own card, and text after it in a new reply", () => {
    const c = run(
      { type: "text", text: "Running:" },
      { type: "command", toolId: "t1", command: "df -h", host: "web (u@web)" },
      { type: "result", toolId: "t1", state: "done", text: "/dev/sda1 40%" },
      { type: "text", text: "40% used." },
      { type: "state", state: "idle" }
    );
    expect(c.items).toEqual([
      { kind: "assistant", text: "Running:" },
      { kind: "command", toolId: "t1", command: "df -h", host: "web (u@web)", result: { state: "done", text: "/dev/sda1 40%" } },
      { kind: "assistant", text: "40% used." },
    ]);
    expect(c.busy).toBe("idle");
  });

  it("keeps a refusal that came without a command card", () => {
    const c = run({ type: "result", toolId: "t2", command: "ls", state: "refused", text: "no terminal to run it in" });
    expect(c.items).toEqual([{ kind: "command", toolId: "t2", command: "ls", host: "", result: { state: "refused", text: "no terminal to run it in" } }]);
  });

  it("marks an outcome that is unknown after a drop", () => {
    const c = run({ type: "command", toolId: "t1", command: "apt upgrade", host: "h" }, { type: "result", toolId: "t1", state: "unknown", text: "" });
    expect(c.items[0]).toMatchObject({ kind: "command", result: { state: "unknown" } });
  });

  it("offers Retry only on the last error, and a stop ends the turn", () => {
    let c = run({ type: "error", text: "overloaded", retry: true });
    expect(c.retry).toBe(true);
    c = reduce(c, { type: "user", text: "again" });
    expect(c.retry).toBe(false);
    c = reduce(reduce(c, { type: "state", state: "working" }), { type: "stopped" });
    expect(c.busy).toBe("idle");
    expect(c.items[c.items.length - 1]).toEqual({ kind: "notice", text: "stopped" });
  });

  it("names the provider whose key must be typed again", () => {
    const c = run({ type: "error", text: "type it again", code: "SecretNeeded", details: { reason: "retype", provider: "Claude" } });
    expect(c.retype).toBe("Claude");
  });

  it("shows a save_skill proposal with its name and body, then its outcome", () => {
    const c = run(
      { type: "skill", toolId: "t3", name: "deploy-check", text: "Step 1", replaces: true },
      { type: "result", toolId: "t3", state: "saved" }
    );
    expect(c.items).toEqual([{ kind: "skill", toolId: "t3", name: "deploy-check", body: "Step 1", replaces: true, result: { state: "saved", text: "" } }]);
  });

  it("ends a pinned conversation when its terminal closes", () => {
    const c = run({ type: "state", state: "working" }, { type: "targetClosed" });
    expect(c.closed).toBe(true);
    expect(c.busy).toBe("idle");
  });
});

describe("code fences", () => {
  it("splits text and code, and treats a fence still streaming as code", () => {
    expect(splitFences("Run:\n```sh\ndf -h\n```\nDone.")).toEqual([
      { code: false, text: "Run:\n" },
      { code: true, text: "df -h\n" },
      { code: false, text: "\nDone." },
    ]);
    expect(splitFences("a\n```\nb")).toEqual([
      { code: false, text: "a\n" },
      { code: true, text: "b" },
    ]);
    expect(splitFences("")).toEqual([]);
  });

  it("renders model text as text, never as HTML", () => {
    const html = renderToStaticMarkup(createElement(RichText, { text: "<img src=x onerror=alert(1)>\n```\n<script>x</script>\n```" }));
    expect(html).not.toContain("<img");
    expect(html).not.toContain("<script");
    expect(html).toContain("&lt;img src=x onerror=alert(1)&gt;");
    expect(html).toContain("<pre");
  });

  it("renders bold, inline code, lists and headings, and keeps a mark still streaming as typed", () => {
    const html = renderToStaticMarkup(createElement(RichText, { text: "# Title\nUse **bold** and `ls -la`.\n\n- one\n- two\n1. first\nhalf **open" }));
    expect(html).toContain("<strong>bold</strong>");
    expect(html).toContain("ls -la</code>");
    expect(html).toContain("<ul");
    expect(html).toContain("<li>two</li>");
    expect(html).toContain("<ol");
    expect(html).toContain("half **open");
    expect(html).not.toContain("# Title");
  });
});

const skill = (name: string, kind: AISkill["kind"], description = ""): AISkill => ({ id: name, name, description, body: "<script>alert(1)</script>", kind, agentAuthored: false });

describe("the slash menu", () => {
  const skills = [skill("deploy-check", "skill"), skill("explain", "template"), skill("disk", "skill")];

  it("lists the saved names the draft starts to type, and closes after a space", () => {
    expect(slashMatches("/", skills).map((s) => s.name)).toEqual(["deploy-check", "disk", "explain"]);
    expect(slashMatches("/d", skills).map((s) => s.name)).toEqual(["deploy-check", "disk"]);
    expect(slashMatches("/deploy-check ", skills)).toEqual([]);
    expect(slashMatches("see /d", skills)).toEqual([]);
    expect(slashMatches("", skills)).toEqual([]);
  });

  it("renders a skill's text as text", () => {
    const html = renderToStaticMarkup(
      createElement(SlashMenu, { matches: [skill("x", "skill", "<img src=x onerror=alert(1)>")], active: 0, onPick: () => {} })
    );
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img src=x onerror=alert(1)&gt;");
  });
});

describe("a save_skill card", () => {
  it("shows the full name and body as text, and says when it replaces one", () => {
    const p: AIPending = {
      id: "r1", principal: "c1", target: "", host: "Hostbay", tool: "save_skill",
      bytes: "Run <script>alert(1)</script>", size: 29, expires: new Date(Date.now() + 60000).toISOString(), name: "deploy-check", replaces: true,
    };
    const html = renderToStaticMarkup(createElement(ApprovalCard, { p, onAnswer: () => {} }));
    expect(html).not.toContain("<script");
    expect(html).toContain("Run &lt;script&gt;alert(1)&lt;/script&gt;");
    expect(html).toContain("/deploy-check");
    expect(html).toContain("Replace");
  });
});

describe("Claude Code errors", () => {
  it("keeps Go's fixed message and its code, and adds a line for a plan limit", () => {
    const c = run({ type: "error", text: "Your Claude plan's usage limit was reached. Try again after it resets.", code: "ClaudeCodeLimit" });
    expect(c.items).toEqual([{ kind: "error", text: "Your Claude plan's usage limit was reached. Try again after it resets.", code: "ClaudeCodeLimit" }]);
    expect(errorHint("ClaudeCodeLimit")).toContain("plan limits");
  });

  it("points setup problems at Settings and adds nothing for other errors", () => {
    for (const code of ["ClaudeCodeMissing", "ClaudeCodeTooOld", "ClaudeCodeSignedOut", "ClaudeCodeAccount", "ClaudeCodeChanged", "ClaudeCodeConfigured"]) {
      expect(errorHint(code)).toContain("Settings > AI");
    }
    expect(errorHint("ClaudeCodeStopped")).toBe("");
    expect(errorHint(undefined)).toBe("");
  });
});
