import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Vpn } from "../api";
import { en } from "../i18n/en";
import type { VpnDraft } from "../importApi";
import { decide, VpnDraftView, type Review } from "./VpnImport";

const profile = (over: Partial<Vpn> = {}): Vpn => ({
  id: "d1",
  name: "office",
  type: "openvpn",
  tier: "system",
  gateway: "vpn1.example.test",
  port: 1195,
  auth: "password",
  routes: ["10.1.0.0/16"],
  hasSecret: true,
  ...over,
});
const draft = (over: Partial<VpnDraft> = {}): VpnDraft => ({ profile: profile(), ignored: ["up /tmp/x.sh", "script-security 2"], askPassword: true, ...over });
const render = (d: VpnDraft) => renderToStaticMarkup(createElement(VpnDraftView, { draft: d, value: d.profile, tried: false, onChange: () => {} }));

describe("VPN import review", () => {
  it("saves nothing for a skipped profile and the edited one for a saved one", () => {
    let r: Review = { at: 0, kept: [] };
    r = decide(r, null);
    expect(r).toEqual({ at: 1, kept: [] });
    const edited = profile({ id: "d2", tags: ["atlas"] });
    r = decide(r, edited);
    expect(r).toEqual({ at: 2, kept: [edited] });
  });

  it("shows the gateway, sign-in, routes and every dropped directive", () => {
    const html = render(draft());
    expect(html).toContain("vpn1.example.test:1195");
    expect(html).toContain(en["vpnForm.auth.password"]);
    expect(html).toContain("10.1.0.0/16");
    expect(html).toContain("up /tmp/x.sh");
    expect(html).toContain("script-security 2");
  });

  it("says a password missing from the file is asked for on first connect", () => {
    expect(render(draft())).toContain(en["vpnImport.askPassword"]);
    expect(render(draft({ askPassword: false, ignored: [] }))).not.toContain(en["vpnImport.askPassword"]);
    expect(render(draft({ ignored: null }))).not.toContain(en["vpnImport.ignoredHint"]);
  });
});
