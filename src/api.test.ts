import { beforeEach, describe, expect, it, vi } from "vitest";

// Record what api.ts sends to the Wails runtime instead of a real backend.
const sent = vi.hoisted(() => ({ calls: [] as number[], events: [] as string[], rejects: [] as unknown[] }));
vi.mock("@wailsio/runtime", () => ({
  CancellablePromise: Promise,
  Call: {
    ByID: (id: number) => {
      sent.calls.push(id);
      // Queued rejections fail the next calls, in order.
      return sent.rejects.length ? Promise.reject(sent.rejects.shift()) : Promise.resolve(null);
    },
  },
  Events: {
    On: (name: string) => {
      sent.events.push(name);
      return () => {};
    },
  },
  Stream: (name: string) => {
    sent.events.push(`stream:${name}`);
    return { close: () => {} };
  },
}));

import * as api from "./api";
import * as bindings from "./bindings/hostbay/internal/app";

// Every #[tauri::command] the Tauri app registered (src-tauri/src/lib.rs).
const TAURI_COMMANDS = `list_hosts save_host delete_host import_ssh_config export_store export_host
write_text_file read_text_file import_store reveal_password wol_wake wol_wake_via list_ssh_keys
save_identity_key list_identity_keys delete_identity_key list_scripts save_scripts list_bookmarks
save_bookmarks list_folders save_folder delete_folder list_trash clone_host restore_host purge_host
connect connect_adhoc trust_host_key disconnect exec_command pty_open pty_write pty_resize pty_close
sftp_list sftp_read sftp_write sftp_mkdir sftp_rename sftp_remove sftp_copy sftp_chmod sftp_download
sftp_upload docker_ps docker_action docker_images docker_pull docker_rmi docker_prune docker_stats
docker_system_df compose_projects compose_action docker_fix_permission nginx_test nginx_reload
systemd_list systemd_action dashboard_info ppk_to_pem rdp_connect rdp_input rdp_close
rdp_set_clipboard iis_sites iis_pools iis_site_action iis_pool_action
windows_dashboard`.split(/\s+/);

const pascal = (s: string) => s.replace(/(^|_)(\w)/g, (_, __, c: string) => c.toUpperCase());

// rdp_set_clipboard_files is gone on purpose: the RDP helper reads the
// local clipboard itself, and a bound call would let a page script send any
// readable file to the server.

// Exports of api.ts that are helpers, not backend calls.
const HELPERS = new Set(["errorInfo", "isFileHost", "isDatabaseHost", "hostKind"]);

beforeEach(() => {
  sent.calls.length = 0;
  sent.events.length = 0;
});

describe("api bridge", () => {
  it("has a generated binding for every Tauri command", () => {
    const bound = new Set(Object.values(bindings).flatMap((svc) => Object.keys(svc)));
    const missing = TAURI_COMMANDS.map(pascal).filter((name) => !bound.has(name));
    expect(missing).toEqual([]);
  });

  it("routes every exported function to exactly one binding call or event", async () => {
    const fns = Object.entries(api).filter(
      ([name, v]) => typeof v === "function" && !HELPERS.has(name)
    ) as [string, (...a: unknown[]) => unknown][];
    expect(fns.length).toBeGreaterThan(70);

    for (const [name, fn] of fns) {
      sent.calls.length = 0;
      sent.events.length = 0;
      await fn(...Array.from({ length: Math.max(fn.length, 1) }, (_, i) => `a${i}`));
      expect(sent.calls.length + sent.events.length, name).toBe(1);
    }
  });

  it("subscribes to events keyed by the id the backend returned", async () => {
    await api.onPtyData("p1", () => {});
    api.rdpView("r1", { onFrame: () => {}, onSize: () => {}, onClosed: () => {} });
    await api.onRdpClipboard("r1", () => {});
    await api.onRdpClipFiles("r1", () => {});
    await api.onMonitorSample("m1", () => {});
    expect(sent.events).toEqual([
      "pty://p1",
      "stream:rdp",
      "rdp-clipboard://r1",
      "rdp-clip-files://r1",
      "monitor-sample://m1",
    ]);
  });

  it("hears every transfer task on the one app-wide event", async () => {
    await api.onTransfer(() => {});
    expect(sent.events).toEqual(["transfer"]);
  });

  // KTD37: the page reaches settings only through these; Go refuses the ai
  // group and every local setting but AI suggestions and per-host logging.
  // The data-folder calls take no path: Go asks for the folder (R72).
  it("offers the page no other settings call", () => {
    expect(Object.keys(bindings.SettingsService).sort()).toEqual([
      "DataFolder",
      "DataFolderMove",
      "Get",
      "GetLocal",
      "ImportLegacy",
      "Set",
      "SetLocal",
    ]);
  });

  it("hears an AI conversation on its own event and the tray on one app-wide event", async () => {
    await api.onAIEvent("c1", () => {});
    await api.onAIApprovals(() => {});
    expect(sent.events).toEqual(["ai://c1", "ai-approvals"]);
  });

  // KTD31, KTD32: no AI call hands a key back or writes to a terminal; the
  // tray answers a request by its ID and Go runs its own copy of the bytes.
  it("gives the page no AI call that reads a key or runs a command", () => {
    const names = Object.keys(bindings.AIService).filter((n) => !/AllowEntry$/.test(n));
    expect(names.filter((n) => /key|secret|exec|write|run/i.test(n))).toEqual([]);
  });

  // KTD35, KTD36: the page can neither read a token nor run anything over
  // MCP; widening calls go through Go's dialog, and the folder through Go's.
  it("offers the page only these MCP calls", () => {
    expect(Object.keys(bindings.MCPService).sort()).toEqual([
      "AddAllowEntry",
      "AddVPNAllow",
      "CreateToken",
      "ListAllowlist",
      "ListTokens",
      "ListVPNAllow",
      "PickFolder",
      "RemoveAllowEntry",
      "RemoveVPNAllow",
      "RevokeToken",
      "SetAutoReadOnly",
      "SetEnabled",
      "SetPort",
      "Status",
    ]);
  });

  it("turns a null list from Go into an empty array", async () => {
    await expect(api.listHosts()).resolves.toEqual([]);
    await expect(api.containerInfo("s1")).resolves.toEqual({ engines: [] });
  });
});

describe("errorInfo", () => {
  it("reads code and details from a rejected backend call", () => {
    // Wails rejects with a RuntimeError whose cause is MarshalError's JSON.
    const e = Object.assign(new Error("The authenticity of fe80::1 cannot be established"), {
      cause: {
        code: "HostKeyUnknown",
        message: "The authenticity of fe80::1 cannot be established",
        details: { host: "fe80::1", port: "22", keyType: "ssh-ed25519", fingerprint: "SHA256:x", requestId: "abc" },
      },
    });
    const info = api.errorInfo(e);
    expect(info.code).toBe("HostKeyUnknown");
    expect(info.details?.host).toBe("fe80::1");
    expect(info.details?.requestId).toBe("abc");
  });

  it("passes a plain error through as its message", () => {
    expect(api.errorInfo(new Error("disk full"))).toEqual({ message: "disk full" });
    expect(api.errorInfo("boom")).toEqual({ message: "boom" });
  });
});

describe("a vault locked after a restart", () => {
  const fail = (code: string, details: Record<string, string> = {}) => ({ cause: { code, message: code, details } });
  beforeEach(() => {
    sent.calls.length = 0;
    sent.rejects.length = 0;
  });

  it("unlocks through the prompt and connects again", async () => {
    sent.rejects.push(fail("SecretNeeded", { reason: "locked" }));
    await api.connect("web");
    const [first, unlock, again] = sent.calls;
    expect(sent.calls).toHaveLength(3);
    expect(again).toBe(first);
    // MasterPasswordService.UnlockVault's binding ID (bindings/.../masterpasswordservice.ts).
    expect(unlock).toBe(3716859897);
  });

  it("stops when the prompt is cancelled", async () => {
    sent.rejects.push(fail("SecretNeeded", { reason: "locked" }), fail("VerifyFailed", { result: "cancelled" }));
    await expect(api.saveHost({} as api.Host)).rejects.toMatchObject({ cause: { code: "VerifyFailed" } });
    expect(sent.calls).toHaveLength(2);
  });

  it("leaves other missing secrets to the caller", async () => {
    sent.rejects.push(fail("SecretNeeded", { reason: "missing" }));
    await expect(api.connect("web")).rejects.toMatchObject({ cause: { details: { reason: "missing" } } });
    expect(sent.calls).toHaveLength(1);
  });
});
