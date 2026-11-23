import { describe, expect, it } from "vitest";
import type { AwsInstance } from "../../api";
import { instanceActions, matches, signInNeeded } from "./aws";

const inst = (over: Partial<AwsInstance> = {}): AwsInstance => ({
  id: "i-0123456789abcdef0",
  name: "web-1",
  state: "running",
  type: "t3.micro",
  privateIp: "192.0.2.10",
  publicIp: "198.51.100.7",
  publicDns: "",
  keyName: "sample-key",
  platform: "",
  ...over,
});

describe("matches", () => {
  it("matches the name, ID and addresses, ignoring case", () => {
    expect(matches(inst(), "WEB")).toBe(true);
    expect(matches(inst(), "abcdef0")).toBe(true);
    expect(matches(inst(), "198.51.100")).toBe(true);
    expect(matches(inst(), "192.0.2.10")).toBe(true);
    expect(matches(inst(), "db-")).toBe(false);
  });

  it("keeps every row for an empty or blank filter", () => {
    expect(matches(inst(), "")).toBe(true);
    expect(matches(inst(), "   ")).toBe(true);
  });

  it("matches a database's identifier, engine and endpoint", () => {
    const db = { id: "orders", engine: "postgres", address: "orders.abc.us-east-1.rds.example.test" };
    expect(matches(db, "ORD")).toBe(true);
    expect(matches(db, "postgres")).toBe(true);
    expect(matches(db, "rds.example")).toBe(true);
    expect(matches(db, "mysql")).toBe(false);
  });
});

describe("instanceActions", () => {
  it("offers shell, stop and reboot on a running instance", () => {
    expect(instanceActions("running")).toEqual(["shell", "stop", "reboot"]);
  });
  it("offers start on a stopped instance", () => {
    expect(instanceActions("stopped")).toEqual(["start"]);
  });
  it("offers nothing while an instance changes state", () => {
    for (const s of ["pending", "stopping", "shutting-down", "terminated"]) expect(instanceActions(s)).toEqual([]);
  });
});

describe("signInNeeded", () => {
  it("is true only for the AWSSignIn code", () => {
    expect(signInNeeded({ code: "AWSSignIn", message: "expired", details: { profile: "sso-base" } })).toBe("sso-base");
    expect(signInNeeded({ code: "DatabaseError", message: "no" })).toBe("");
    expect(signInNeeded({ message: "Profile proc: the credential_process failed" })).toBe("");
  });
});
