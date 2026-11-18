import { createElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { Vpn } from "../../api";
import { en } from "../../i18n/en";
import { ReachField, VpnForm } from "./Vpns";

const platform = vi.hoisted(() => ({ mac: true }));
vi.mock("../../platform", () => ({ isMac: () => platform.mac }));
vi.mock("../../state/hosts", () => ({ useHosts: () => ({ hosts: [] }) }));
// The dialog portals into document, which a server render has none of.
vi.mock("../../ui/Dialog", () => ({ Dialog: ({ children, footer }: { children: ReactNode; footer?: ReactNode }) => [children, footer] }));
vi.mock("../../state/vpns", () => ({ useVpns: () => ({ vpns: [] }), reloadVpns: () => {} }));

const od = (over: Partial<Vpn> = {}): Vpn => ({ id: "od", name: "Corp Internal", type: "sslvpn", tier: "app", auth: "password", tags: [], ...over });

// The segmented control inside the field, whose props the form wires.
type Option = { value: string; disabled?: boolean };
type Seg = { value: string; options: Option[]; onChange: (tier: string) => void };
const control = (v: Vpn, auth: Vpn["auth"] | "" = v.auth ?? "", set = vi.fn()) => {
  const el = ReachField({ v, auth: auth as never, set }) as ReactElement<{ children: ReactElement<Seg> }> | null;
  return { seg: el?.props.children.props, set };
};

describe("VPN form reach", () => {
  it("offers Whole computer for an OpenConnect profile on macOS", () => {
    platform.mac = true;
    const html = renderToStaticMarkup(createElement(ReachField, { v: od(), auth: "password", set: () => {} }));
    expect(html).toContain(en["vpnForm.scope.app"]);
    expect(html).toContain(en["vpnForm.scope.system"]);
  });

  it("choosing Whole computer makes the profile system-wide and applies its routes", () => {
    platform.mac = true;
    const { seg, set } = control(od());
    expect(seg?.value).toBe("app");
    seg!.onChange("system");
    expect(set).toHaveBeenCalledWith({ tier: "system", autoApply: true });
  });

  it("keeps Whole computer off for a sign-in it cannot run", () => {
    platform.mac = true;
    for (const auth of ["otp", "browser"] as const) {
      const { seg } = control(od({ auth }), auth);
      expect(seg?.options.find((o) => o.value === "system")?.disabled).toBe(true);
    }
  });

  it("is absent off macOS, for other types and for an external profile", () => {
    platform.mac = false;
    expect(ReachField({ v: od(), auth: "password", set: () => {} })).toBeNull();
    platform.mac = true;
    expect(ReachField({ v: od({ type: "wireguard" }), auth: "", set: () => {} })).toBeNull();
    expect(ReachField({ v: od({ tier: "external" }), auth: "", set: () => {} })).toBeNull();
  });
});

describe("VPN form for AWS Client VPN SSO", () => {
  it("shows the browser sign-in hint, with no sign-in picker or password field", () => {
    const aws: Vpn = { id: "aws", name: "AWS", type: "openvpn", tier: "system", auth: "browser", gateway: "cvpn-endpoint-0.example", tags: [], hasSecret: true };
    const html = renderToStaticMarkup(createElement(VpnForm, { initial: aws, onClose: () => {}, onSaved: () => {} }));
    expect(html).toContain(en["vpnForm.browserHint"]);
    expect(html).not.toContain(en["vpnForm.password"]);
    expect(html).not.toContain(`>${en["vpnForm.auth"]}<`);
  });
});
