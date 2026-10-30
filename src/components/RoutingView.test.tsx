import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { HelperStatus, RoutePlan, RoutingReport, RoutingState, Vpn } from "../api";
import { en } from "../i18n/en";
import { RoutingPanel, type RoutingPanelProps } from "./RoutingView";

const noop = () => {};
const harmony: Vpn = {
  id: "h",
  name: "Harmony SASE",
  type: "perimeter81",
  tier: "external",
  routes: ["10.1.0.0/16", "10.2.0.0/16"],
};
const plan: RoutePlan = {
  changes: [
    {
      kind: "add",
      op: "add",
      dst: "10.1.0.0/16",
      iface: "utun4",
      profile: "h",
      reason: "10.1.0.0/16 through Harmony SASE",
    },
    {
      kind: "add",
      op: "add",
      dst: "10.2.0.0/16",
      iface: "utun4",
      profile: "h",
      reason: "10.2.0.0/16 through Harmony SASE",
    },
  ],
  skipped: [],
  greedy: [],
  unlabeled: [],
};

function render(over: Partial<RoutingPanelProps> = {}) {
  const props: RoutingPanelProps = {
    helper: { state: "ready" },
    vpns: [harmony],
    profile: "h",
    mode: "",
    plan,
    report: null,
    state: { profiles: {}, reset: false },
    local: [],
    busy: false,
    onProfile: noop,
    onMode: noop,
    onLocal: noop,
    onApply: noop,
    onReset: noop,
    onInstall: noop,
    onNarrow: noop,
    onPin: noop,
    ...over,
  };
  return renderToStaticMarkup(createElement(RoutingPanel, props));
}

// renderToStaticMarkup escapes apostrophes.
const esc = (s: string) => s.replace(/'/g, "&#x27;");
const button = (html: string, label: string) => [...html.matchAll(/<button([^>]*)>(.*?)<\/button>/g)].find((m) => m[2].replace(/<[^>]+>/g, "") === label);

describe("Routing view", () => {
  it("shows each planned change with its interface and VPN", () => {
    const html = render();
    expect(html).toContain("10.1.0.0/16 on utun4");
    expect(html).toContain("Harmony SASE");
    expect(button(html, en["routing.apply"])?.[1]).not.toContain('disabled=""');
  });

  it("disables Apply with the reason, and offers Install, when the helper is missing", () => {
    const helper: HelperStatus = { state: "missing" };
    const html = render({ helper });
    expect(button(html, en["routing.apply"])?.[1]).toContain('disabled=""');
    expect(html).toContain(esc(en["routing.helper.missing"]));
    expect(button(html, en["routing.install"])).toBeTruthy();
  });

  it("shows why the AppImage cannot route, with nothing to install", () => {
    const html = render({
      helper: {
        state: "unavailable",
        reason: "System-wide routing cannot run from the AppImage",
      },
    });
    expect(html).toContain("System-wide routing cannot run from the AppImage");
    expect(button(html, en["routing.install"])).toBeUndefined();
    expect(button(html, en["routing.apply"])?.[1]).toContain('disabled=""');
  });

  it("marks a failed change and shows the profile partly applied", () => {
    const report: RoutingReport = {
      results: [{ ...plan.changes[0] }, { ...plan.changes[1], error: "route: File exists" }],
      skipped: [],
      partly: ["h"],
    };
    const state: RoutingState = {
      profiles: { h: "partly applied" },
      reset: false,
    };
    const html = render({ report, state });
    expect(html).toContain(en["routing.failed"]);
    expect(html).toContain("route: File exists");
    expect(html).toContain("Harmony SASE: partly applied");
  });

  it("offers narrowing for a greedy route and pinning for an unlabeled tunnel", () => {
    const html = render({
      plan: {
        ...plan,
        greedy: [
          {
            dst: "10.0.0.0/8",
            iface: "utun4",
            profile: "h",
            keep: ["10.1.0.0/16"],
          },
        ],
        unlabeled: ["utun6"],
      },
    });
    expect(button(html, en["routing.narrow"])).toBeTruthy();
    expect(html).toContain("10.0.0.0/8 on utun4 is broad");
    expect(html).toContain("Pin utun6 to");
  });
});
