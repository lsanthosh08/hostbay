import type { AppError, AwsInstanceAction } from "../../api";

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
