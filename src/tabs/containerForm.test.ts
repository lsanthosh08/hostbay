import { expect, it } from "vitest";
import type { DashboardInfo } from "../api";
import { listenAddress, loopback, noEngine, parseMount, parsePort, toCreateOptions } from "./containerForm";

it("gives a port without an address 127.0.0.1", () => {
  expect(parsePort("8080:80")).toEqual({ hostAddress: "127.0.0.1", hostPort: 8080, containerPort: 80, protocol: "tcp" });
  expect(parsePort("0.0.0.0:8443:443")).toEqual({ hostAddress: "0.0.0.0", hostPort: 8443, containerPort: 443, protocol: "tcp" });
  expect(parsePort("[::1]:5353:53/udp")).toEqual({ hostAddress: "::1", hostPort: 5353, containerPort: 53, protocol: "udp" });
  expect(() => parsePort("80")).toThrow();
  expect(() => parsePort("a:b")).toThrow();
});

it("warns only for ports beyond loopback", () => {
  expect(loopback(parsePort("8080:80"))).toBe(true);
  expect(loopback(parsePort("[::1]:1:1"))).toBe(true);
  expect(loopback(parsePort("0.0.0.0:1:1"))).toBe(false);
  expect(loopback(parsePort("192.168.1.5:1:1"))).toBe(false);
});

it("reads mounts, read-only ones and Windows folders", () => {
  expect(parseMount("/srv/site:/usr/share/nginx/html:ro")).toEqual({ source: "/srv/site", target: "/usr/share/nginx/html", readOnly: true });
  expect(parseMount("data:/data")).toEqual({ source: "data", target: "/data", readOnly: false });
  expect(parseMount("C:\\data:/data")).toEqual({ source: "C:\\data", target: "/data", readOnly: false });
  expect(() => parseMount("/data")).toThrow();
});

it("builds create options from the form, one item per line", () => {
  expect(
    toCreateOptions({ image: " nginx ", name: "", ports: "8080:80\n\n", env: "A=1\nB=2", volumes: "", binds: "/:/host", restart: "" })
  ).toEqual({
    image: "nginx",
    name: "",
    ports: [{ hostAddress: "127.0.0.1", hostPort: 8080, containerPort: 80, protocol: "tcp" }],
    env: ["A=1", "B=2"],
    volumes: [],
    binds: [{ source: "/", target: "/host", readOnly: false }],
    restart: "",
  });
});

// The dashboard already knows; without an engine the tab says so instead
// of running docker commands that fail with "sh: docker: not found".
it("knows a host with no container engine", () => {
  const d = { dockerInstalled: false, podmanInstalled: false, nerdctlInstalled: false } as DashboardInfo;
  expect(noEngine(d)).toBe(true);
  expect(noEngine({ ...d, podmanInstalled: true })).toBe(false);
  expect(noEngine({ ...d, dockerInstalled: true })).toBe(false);
});

it("writes an IPv6 listen address in brackets", () => {
  expect(listenAddress(parsePort("[::]:58081:80"))).toBe("[::]:58081");
  expect(listenAddress(parsePort("0.0.0.0:8080:80"))).toBe("0.0.0.0:8080");
});
