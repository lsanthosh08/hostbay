import type { ClusterInfo, K8sItem, K8sPage } from "../../api";

// Each open cluster tab's context, namespace and server, by clusterId, as
// kubernetesConnect returned them. A tab keeps what it opened with (KTD28).
export const clusters = new Map<string, ClusterInfo>();

/** The kinds the tab browses, in its picker's order; cluster-scoped ones
 *  ignore the namespace. */
export const KINDS = [
  "pods",
  "deployments",
  "statefulsets",
  "daemonsets",
  "jobs",
  "services",
  "ingresses",
  "configmaps",
  "secrets",
  "events",
  "namespaces",
  "nodes",
] as const;
export type K8sKind = (typeof KINDS)[number];
export const clusterScoped = (k: K8sKind) => k === "namespaces" || k === "nodes";

/** Millicores and bytes as kubectl top shows them. */
export const cpu = (m: number) => `${m}m`;
export const memory = (b: number) => `${Math.round(b / (1 << 20))}Mi`;

/** The list on screen, and the kind and namespace (listKey) it was loaded for. */
export type ListPage = { key: string; items: K8sItem[]; cont: string; loading: boolean; error?: string };

export const listKey = (kind: K8sKind, ns: string) => `${kind}\0${ns}`;

/** A page from kubernetesList added to the list: the next page of the same
 *  list joins it, any other starts it over. */
export const addListPage = (p: ListPage, key: string, cont: string, r: K8sPage): ListPage => ({
  key,
  items: cont && p.key === key ? p.items.concat(r.items) : r.items,
  cont: r.continue,
  loading: false,
});

/** The rows to show for the tab's kind and namespace: none from another
 *  list, whose actions would name the wrong kind. */
export const rowsFor = (p: ListPage, key: string) => (p.key === key ? p.items : []);
