import type { AppError, AwsDatabase, AwsInstance, AwsInstanceAction, Host } from "../../api";

/** Whether a row matches the tab's filter: any of its text fields contains
 *  the filter, ignoring case. A blank filter matches every row. */
export function matches(row: object, filter: string): boolean {
  const f = filter.trim().toLowerCase();
  if (!f) return true;
  return Object.values(row).some((v) => typeof v === "string" && v.toLowerCase().includes(f));
}

/** What an instance in a state offers. Only a running instance has a shell,
 *  stop and reboot, and only a stopped one starts; an instance changing
 *  state offers nothing until the list is refreshed. */
export function instanceActions(state: string): ("shell" | AwsInstanceAction)[] {
  if (state === "running") return ["shell", "stop", "reboot"];
  if (state === "stopped") return ["start"];
  return [];
}

/** The profile to sign in when an error is an expired or missing AWS SSO
 *  sign-in, else "". */
export const signInNeeded = (e: AppError): string => (e.code === "AWSSignIn" ? e.details?.profile ?? "" : "");

/** A host to open in the host editor for an instance: SSH on its public
 *  address (else its private one), or RDP for a Windows instance. Nothing
 *  is saved until the user saves the form. */
export function instanceDraft(i: AwsInstance): Partial<Host> {
  const base = { alias: i.name || i.id, hostname: i.publicIp || i.privateIp };
  return i.platform === "windows"
    ? { kind: "rdp", ...base, rdpPort: 3389, tags: ["aws"] }
    : { kind: "ssh", ...base, port: 22, tags: ["aws"] };
}

/** A database host for an RDS instance, of the kind Go chose, on the
 *  address Go chose (an Aurora member's cluster writer endpoint); null when
 *  Hostbay has no kind that opens its engine. */
export function databaseDraft(d: AwsDatabase): Partial<Host> | null {
  if (!d.kind) return null;
  return {
    kind: d.kind as Host["kind"],
    alias: d.id,
    hostname: d.saveHost,
    port: d.port,
    user: d.user,
    ...(d.dbName ? { database: d.dbName } : {}),
    tags: ["aws"],
  };
}
