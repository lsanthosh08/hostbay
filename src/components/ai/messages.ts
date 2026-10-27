// The AI sidebar's transcript as a pure reducer over Go's ai.Event stream
// (internal/ai/agent.go), plus the code-fence split that renders model text.
// Everything the model or a terminal sent is shown as text, never as HTML.
import { createElement, Fragment, type ReactNode } from "react";
import type { AIEvent } from "../../api";
import { t } from "../../i18n";

export type CommandResult = { state: string; text: string };
export type Item =
  | { kind: "user"; text: string }
  | { kind: "assistant"; text: string }
  | { kind: "command"; toolId: string; command: string; host: string; result?: CommandResult }
  | { kind: "skill"; toolId: string; name: string; body: string; replaces: boolean; result?: CommandResult }
  | { kind: "error"; text: string; code?: string }
  | { kind: "notice"; text: "stopped" | "targetClosed" };

export type Chat = {
  items: Item[];
  busy: "thinking" | "working" | "idle";
  /** The last error can be retried. */
  retry: boolean;
  /** The provider whose key must be typed again in Settings > AI. */
  retype: string;
  /** The pinned terminal closed: start a new conversation. */
  closed: boolean;
};

export const initialChat: Chat = { items: [], busy: "idle", retry: false, retype: "", closed: false };

export type ChatAction = AIEvent | { type: "user"; text: string };

export function reduce(c: Chat, e: ChatAction): Chat {
  const items = c.items;
  const last = items[items.length - 1];
  switch (e.type) {
    case "user":
      return { ...c, items: [...items, { kind: "user", text: e.text }], retry: false, retype: "" };
    case "text":
      if (last?.kind === "assistant") return { ...c, items: [...items.slice(0, -1), { kind: "assistant", text: last.text + (e.text ?? "") }] };
      return { ...c, items: [...items, { kind: "assistant", text: e.text ?? "" }] };
    case "command":
      return { ...c, items: [...items, { kind: "command", toolId: e.toolId ?? "", command: e.command ?? "", host: e.host ?? "" }] };
    case "skill":
      return { ...c, items: [...items, { kind: "skill", toolId: e.toolId ?? "", name: e.name ?? "", body: e.text ?? "", replaces: !!e.replaces }] };
    case "result": {
      const result = { state: e.state ?? "", text: e.text ?? "" };
      const ours = (i: Item) => (i.kind === "command" || i.kind === "skill") && i.toolId === e.toolId;
      let at = items.length - 1;
      while (at >= 0 && !ours(items[at])) at--;
      if (at < 0) return { ...c, items: [...items, { kind: "command", toolId: e.toolId ?? "", command: e.command ?? "", host: "", result }] };
      return { ...c, items: items.map((i, n) => (n === at && (i.kind === "command" || i.kind === "skill") ? { ...i, result } : i)) };
    }
    case "error":
      return {
        ...c,
        items: [...items, { kind: "error", text: e.text ?? "", code: e.code }],
        busy: "idle",
        retry: !!e.retry,
        retype: e.code === "SecretNeeded" && e.details?.reason === "retype" ? e.details.provider ?? "" : "",
      };
    case "state":
      return { ...c, busy: e.state === "thinking" || e.state === "working" ? e.state : "idle" };
    case "stopped":
      return { ...c, items: [...items, { kind: "notice", text: "stopped" }], busy: "idle" };
    case "targetClosed":
      return { ...c, items: [...items, { kind: "notice", text: "targetClosed" }], busy: "idle", closed: true };
  }
  return c;
}

export type Segment = { code: boolean; text: string };

/** Splits text on ``` fences; a fence not yet closed (still streaming) runs
 *  to the end as code. A fence's info string (```sh) is dropped. */
export function splitFences(text: string): Segment[] {
  const out: Segment[] = [];
  let code = false;
  let rest = text;
  while (rest) {
    const at = rest.indexOf("```");
    if (at < 0) {
      out.push({ code, text: rest });
      break;
    }
    if (at > 0) out.push({ code, text: rest.slice(0, at) });
    rest = rest.slice(at + 3);
    if (!code) {
      const nl = rest.indexOf("\n");
      rest = nl < 0 ? "" : rest.slice(nl + 1);
    }
    code = !code;
  }
  return out;
}

const INLINE = /(\*\*[^*\n]+\*\*|`[^`\n]+`|\*[^*\s][^*\n]*[^*\s]\*|\*[^*\s]\*)/;

/** Bold, italic and inline code in one line, as React nodes. A mark not yet
 *  closed (still streaming) stays as typed. */
export function renderInline(line: string): ReactNode[] {
  return line.split(INLINE).map((part, i) => {
    if (part.length > 4 && part.startsWith("**") && part.endsWith("**")) return createElement("strong", { key: i }, part.slice(2, -2));
    if (part.length > 2 && part.startsWith("`") && part.endsWith("`"))
      return createElement("code", { key: i, className: "px-1 rounded-control bg-raised font-mono text-xs" }, part.slice(1, -1));
    if (part.length > 2 && part.startsWith("*") && part.endsWith("*") && !part.startsWith("**")) return createElement("em", { key: i }, part.slice(1, -1));
    return part;
  });
}

type Block = { kind: "p" | "h" | "ul" | "ol"; lines: string[] };

/** Lines grouped into headings, lists and paragraphs: the part of Markdown
 *  an assistant reply uses. Anything else is shown as typed. */
export function splitBlocks(text: string): Block[] {
  const out: Block[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trimEnd();
    if (!line.trim()) {
      out.push({ kind: "p", lines: [] }); // a blank line ends a paragraph
      continue;
    }
    const h = /^#{1,6}\s+(.*)$/.exec(line);
    const ul = /^\s*[-*+]\s+(.*)$/.exec(line);
    const ol = /^\s*\d+[.)]\s+(.*)$/.exec(line);
    const kind = h ? "h" : ul ? "ul" : ol ? "ol" : "p";
    const body = (h ?? ul ?? ol)?.[1] ?? line;
    const last = out[out.length - 1];
    if (last && last.kind === kind && kind !== "h" && (kind !== "p" || last.lines.length > 0)) last.lines.push(body);
    else out.push({ kind, lines: [body] });
  }
  return out.filter((b) => b.lines.length > 0);
}

function renderBlocks(text: string, key: number): ReactNode {
  return createElement(
    "div",
    { key, className: "allow-select break-words flex flex-col gap-1.5" },
    splitBlocks(text).map((b, i) => {
      switch (b.kind) {
        case "h":
          return createElement("p", { key: i, className: "font-semibold" }, renderInline(b.lines[0]));
        case "ul":
        case "ol":
          return createElement(
            b.kind,
            { key: i, className: `${b.kind === "ul" ? "list-disc" : "list-decimal"} pl-5 flex flex-col gap-0.5` },
            b.lines.map((l, j) => createElement("li", { key: j }, renderInline(l)))
          );
        default:
          return createElement(
            "p",
            { key: i },
            b.lines.flatMap((l, j) => (j ? [createElement("br", { key: `b${j}` }), ...renderInline(l)] : renderInline(l)))
          );
      }
    })
  );
}

/** Model text with its code blocks and light Markdown, as React elements
 *  only: nothing the model writes becomes HTML. */
export function RichText({ text }: { text: string }) {
  return createElement(
    Fragment,
    null,
    splitFences(text).map((s, i) =>
      s.code
        ? createElement("pre", { key: i, className: "my-1.5 p-2 rounded-control bg-raised font-mono text-xs whitespace-pre-wrap break-all allow-select" }, s.text)
        : renderBlocks(s.text, i)
    )
  );
}

/** A short line under a Claude Code error: the message itself is fixed text
 *  written in Go, never the program's own output. */
export function errorHint(code: string | undefined): string {
  switch (code) {
    case "ClaudeCodeLimit":
      return t("ai.cc.limitHint");
    case "ClaudeCodeMissing":
    case "ClaudeCodeTooOld":
    case "ClaudeCodeSignedOut":
    case "ClaudeCodeAccount":
    case "ClaudeCodeChanged":
    case "ClaudeCodeConfigured":
      return t("ai.cc.setupHint");
  }
  return "";
}
