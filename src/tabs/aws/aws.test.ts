import { describe, expect, it } from "vitest";
import type { AwsDatabase, AwsInstance } from "../../api";
import { databaseDraft, instanceActions, instanceDraft, matches, signInNeeded } from "./aws";

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

describe("instanceDraft", () => {
  it("drafts an SSH host on the public address, named by the Name tag", () => {
    expect(instanceDraft(inst())).toEqual({ kind: "ssh", alias: "web-1", hostname: "198.51.100.7", port: 22, tags: ["aws"] });
  });
  it("falls back to the private address and the instance ID", () => {
    const d = instanceDraft(inst({ name: "", publicIp: "" }));
    expect(d.hostname).toBe("192.0.2.10");
    expect(d.alias).toBe("i-0123456789abcdef0");
  });
  it("drafts an RDP host for a Windows instance", () => {
    expect(instanceDraft(inst({ platform: "windows" }))).toMatchObject({ kind: "rdp", rdpPort: 3389, hostname: "198.51.100.7" });
  });
});

describe("databaseDraft", () => {
  const db = (over: Partial<AwsDatabase> = {}): AwsDatabase => ({
    id: "db1",
    engine: "aurora-postgresql",
    status: "available",
    address: "db1.abc.us-east-1.rds.example.test",
    port: 5432,
    user: "admin",
    dbName: "app",
    cluster: "main",
    kind: "postgres",
    saveHost: "main.cluster-abc.us-east-1.rds.example.test",
    ...over,
  });
  it("drafts the host kind Go chose, on the cluster writer endpoint", () => {
    expect(databaseDraft(db())).toEqual({
      kind: "postgres",
      alias: "db1",
      hostname: "main.cluster-abc.us-east-1.rds.example.test",
      port: 5432,
      user: "admin",
      database: "app",
      tags: ["aws"],
    });
  });
  it("leaves the database unset when RDS names none", () => {
    expect(databaseDraft(db({ dbName: "" }))?.database).toBeUndefined();
  });
  it("drafts nothing for an engine Hostbay cannot open", () => {
    expect(databaseDraft(db({ kind: "" }))).toBeNull();
  });
});
