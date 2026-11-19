import { describe, expect, it } from "vitest";
import { importedRdpSecurity, parseRdpFile } from "./rdpFile";

const file = (...lines: string[]) => lines.join("\r\n");

describe("parseRdpFile", () => {
  it("reads the address with and without a port", () => {
    expect(parseRdpFile(file("full address:s:win.example.test:3390"))).toMatchObject({ hostname: "win.example.test", port: 3390 });
    expect(parseRdpFile(file("full address:s:192.0.2.10"))).toMatchObject({ hostname: "192.0.2.10", port: undefined });
  });

  it("keeps the domain of the account", () => {
    expect(parseRdpFile(file("username:s:CORP\\user")).user).toBe("CORP\\user");
    expect(parseRdpFile(file("username:s:user", "domain:s:CORP")).user).toBe("CORP\\user");
    expect(parseRdpFile(file("username:s:user@example.com", "domain:s:CORP")).user).toBe("user@example.com");
    expect(parseRdpFile(file("username:s:.\\admin", "domain:s:CORP")).user).toBe(".\\admin");
    expect(parseRdpFile(file("username:s:user")).user).toBe("user");
  });

  it("turns NLA off only when the file does", () => {
    expect(parseRdpFile(file("enablecredsspsupport:i:0")).rdpSecurity).toBe("tls");
    expect(parseRdpFile(file("enablecredsspsupport:i:1")).rdpSecurity).toBeUndefined();
    expect(parseRdpFile(file("full address:s:win")).rdpSecurity).toBeUndefined();
    // A certificate-warning policy, not NLA: it changes nothing here.
    expect(parseRdpFile(file("authentication level:i:0")).rdpSecurity).toBeUndefined();
  });
});

describe("importedRdpSecurity", () => {
  it("never lowers a host set to NLA only", () => {
    expect(importedRdpSecurity(undefined, "tls")).toBe("tls");
    expect(importedRdpSecurity("auto", "tls")).toBe("tls");
    expect(importedRdpSecurity("nla", "tls")).toBe("nla");
    expect(importedRdpSecurity("nla", undefined)).toBe("nla");
    expect(importedRdpSecurity(undefined, undefined)).toBeUndefined();
  });
});
