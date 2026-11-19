// Windows .rdp files are `key:type:value` lines (type s = text, i = number).
// The password is never stored in one.

export type RdpSecurity = "auto" | "tls" | "nla";

export type RdpFile = {
  hostname: string;
  port?: number;
  /** DOMAIN\user, a UPN, or a bare account name. */
  user: string;
  /** "tls" when the file turns NLA off (enablecredsspsupport:i:0). */
  rdpSecurity?: "tls";
};

export function parseRdpFile(text: string): RdpFile {
  const get = (key: string) => text.match(new RegExp(`^${key}:[si]:(.*)$`, "im"))?.[1].trim() ?? "";
  const [hostname, portStr] = get("full address").split(":"); // host or host:port
  // mstsc keeps the domain apart from a bare account name; sign-in needs both.
  let user = get("username");
  const domain = get("domain");
  if (user && domain && !/[\\@]/.test(user)) user = `${domain}\\${user}`;
  return {
    hostname,
    port: portStr ? Number(portStr) : undefined,
    user,
    rdpSecurity: get("enablecredsspsupport") === "0" ? "tls" : undefined,
  };
}

// The security choice after importing a file: a file may turn NLA off for a
// new or Automatic host, but never lowers a host the user set to NLA only.
export function importedRdpSecurity(current: RdpSecurity | null | undefined, fromFile: RdpFile["rdpSecurity"]) {
  return fromFile === "tls" && (current == null || current === "auto") ? "tls" : current;
}
