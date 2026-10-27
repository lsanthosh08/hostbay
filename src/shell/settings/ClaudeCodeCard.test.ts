import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { ClaudeCodeCandidate, ClaudeCodeState, ClaudeCodeStatus } from "../../api";
import { en } from "../../i18n/en";
import { ClaudeCodeView } from "./ClaudeCodeCard";

const noop = () => {};
const found: ClaudeCodeCandidate[] = [{ index: 0, path: "/usr/local/bin/claude", source: "path" }];

function status(state: ClaudeCodeState, over: Partial<ClaudeCodeStatus> = {}): ClaudeCodeStatus {
  return { state, message: "", path: "/usr/local/bin/claude", target: "/opt/claude/2.1.310/claude", version: "2.1.310", authMethod: "claude.ai", consented: false, ...over };
}

function render(s: ClaudeCodeStatus | null, candidates: ClaudeCodeCandidate[] = []) {
  return renderToStaticMarkup(
    createElement(ClaudeCodeView, { status: s, candidates, busy: false, onSelect: noop, onPick: noop, onRefresh: noop, onConsent: noop })
  );
}

/** The labels of the buttons in rendered markup. */
function buttons(html: string): string[] {
  return [...html.matchAll(/<button[^>]*>(.*?)<\/button>/g)].map((m) => m[1]);
}

describe("Claude Code setup states", () => {
  it("says it is checking before Go answers", () => {
    expect(render(null)).toContain(en["ai.cc.checking"]);
  });

  it("not found: lists what Go found and offers the file dialog and a recheck", () => {
    const html = render(status("notFound", { path: "", target: "", version: "", authMethod: "" }), found);
    expect(html).toContain(en["ai.cc.notFound"]);
    expect(html).toContain("/usr/local/bin/claude");
    expect(buttons(html)).toEqual([en["ai.cc.use"], en["ai.cc.pick"], en["ai.cc.recheck"]]);
  });

  it("too old: says to update Claude Code and shows the version", () => {
    const html = render(status("tooOld", { version: "2.1.1" }), found);
    expect(html).toContain(en["ai.cc.tooOld"]);
    expect(html).toContain("2.1.1");
    expect(buttons(html)).not.toContain(en["ai.cc.review"]);
  });

  it("signed out: tells the person to run claude auth login themselves and has no sign-in action", () => {
    const html = render(status("signedOut", { authMethod: "" }));
    expect(html).toContain("claude auth login");
    expect(html).toContain("in your own terminal");
    // Only choosing a program and checking again: nothing that starts or completes a sign-in.
    expect(buttons(html)).toEqual([en["ai.cc.pick"], en["ai.cc.recheck"]]);
    expect(html.toLowerCase()).not.toMatch(/sign in with|log in with|>sign in<|>log in</);
  });

  it("another provider or a changed Claude Code: shows Go's own fixed message", () => {
    const html = render(status("unsupported", { message: "Claude Code changed in a way this Hostbay does not support; update Hostbay." }));
    expect(html).toContain("update Hostbay");
  });

  it("consent needed: names the path, the resolved program, the version and the sign-in, and what is sent", () => {
    const html = render(status("consent"));
    for (const want of ["/usr/local/bin/claude", "/opt/claude/2.1.310/claude", "2.1.310", "claude.ai", en["ai.cc.sentTitle"], en["ai.cc.sent1"], en["ai.cc.sent2"], en["ai.cc.sent3"]]) {
      expect(html).toContain(want.replace(/'/g, "&#x27;"));
    }
    expect(html).toContain("not through Hostbay&#x27;s proxies");
    expect(buttons(html)).toEqual([en["ai.cc.review"], en["ai.cc.recheck"]]);
  });

  it("ready: shows the version and sign-in and no consent button", () => {
    const html = render(status("ready", { consented: true }));
    expect(html).toContain("Claude Code 2.1.310 is signed in with claude.ai");
    expect(buttons(html)).not.toContain(en["ai.cc.review"]);
  });
});
