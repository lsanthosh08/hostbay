import { useState } from "react";
import type { K8sItem, K8sUsage } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Select } from "../../ui/Select";
import { cpu, memory, type K8sKind } from "./clusters";

const th = "text-left font-medium px-3 py-2 opacity-70";
const td = "px-3 py-2 border-t border-line align-top";

/** What a row can do; the tab confirms the destructive ones. */
export type RowAction =
  | { kind: "view" | "delete" | "restart" | "scale"; item: K8sItem }
  | { kind: "logs" | "shell"; item: K8sItem; container: string }
  | { kind: "reveal"; item: K8sItem; key: string };

const WORKLOADS: K8sKind[] = ["deployments", "statefulsets", "daemonsets"];

/** A page of objects of one kind as a table, with their metrics when the
 *  cluster has them. Secrets show their key names only. */
export function ResourceList({
  kind,
  items,
  allNamespaces,
  usage,
  onAction,
}: {
  kind: K8sKind;
  items: K8sItem[];
  allNamespaces: boolean;
  usage: Map<string, K8sUsage>;
  onAction: (a: RowAction) => void;
}) {
  if (!items.length) return <p className="text-sm text-muted p-4">{t("kubernetes.empty")}</p>;
  const metrics = (kind === "pods" || kind === "nodes") && usage.size > 0;
  return (
    <table className="w-full text-sm">
      <thead>
        <tr>
          <th className={th}>{t("kubernetes.name")}</th>
          {allNamespaces && <th className={th}>{t("kubernetes.namespace")}</th>}
          <th className={th}>{t("kubernetes.status")}</th>
          {metrics && <th className={th}>{t("kubernetes.cpu")}</th>}
          {metrics && <th className={th}>{t("kubernetes.memory")}</th>}
          <th className={th}>{t("kubernetes.age")}</th>
          <th className={th}>{t("kubernetes.actions")}</th>
        </tr>
      </thead>
      <tbody>
        {items.map((it) => (
          <Row key={`${it.namespace ?? ""}/${it.name}`} kind={kind} it={it} allNamespaces={allNamespaces} usage={metrics ? usage : null} onAction={onAction} />
        ))}
      </tbody>
    </table>
  );
}

function Row({
  kind,
  it,
  allNamespaces,
  usage,
  onAction,
}: {
  kind: K8sKind;
  it: K8sItem;
  allNamespaces: boolean;
  usage: Map<string, K8sUsage> | null;
  onAction: (a: RowAction) => void;
}) {
  const containers = it.containers ?? [];
  const [container, setContainer] = useState(containers[0] ?? "");
  const u = usage?.get(`${it.namespace ?? ""}/${it.name}`);
  const status = [it.status, it.ready && `${t("kubernetes.ready")} ${it.ready}`, it.restarts ? `${t("kubernetes.restarts")} ${it.restarts}` : "", it.type]
    .filter(Boolean)
    .join(" · ");
  return (
    <tr>
      <td className={`${td} font-mono allow-select break-all`}>{it.name}</td>
      {allNamespaces && <td className={td}>{it.namespace}</td>}
      <td className={td}>
        {status}
        {it.detail && <div className="text-xs text-muted allow-select break-words">{it.detail}</div>}
        {it.keys && it.keys.length > 0 && <div className="text-xs text-muted font-mono">{it.keys.join(", ")}</div>}
      </td>
      {usage && <td className={td}>{u ? cpu(u.cpu) : ""}</td>}
      {usage && <td className={td}>{u ? memory(u.memory) : ""}</td>}
      <td className={`${td} whitespace-nowrap text-muted`}>{it.created ? new Date(it.created).toLocaleString() : ""}</td>
      <td className={td}>
        <div className="flex flex-wrap gap-1">
          {kind === "pods" && containers.length > 1 && (
            <Select aria-label={t("kubernetes.container")} className="h-7 text-xs" value={container} onChange={(e) => setContainer(e.target.value)}>
              {containers.map((c) => (
                <option key={c}>{c}</option>
              ))}
            </Select>
          )}
          {kind === "pods" && container && (
            <>
              <Button size="sm" onClick={() => onAction({ kind: "logs", item: it, container })}>
                {t("kubernetes.logs")}
              </Button>
              <Button size="sm" onClick={() => onAction({ kind: "shell", item: it, container })}>
                {t("kubernetes.shell")}
              </Button>
            </>
          )}
          {kind === "secrets" &&
            (it.keys ?? []).map((key) => (
              <Button key={key} size="sm" onClick={() => onAction({ kind: "reveal", item: it, key })}>
                {t("kubernetes.reveal", { key })}
              </Button>
            ))}
          {(kind === "deployments" || kind === "statefulsets") && (
            <Button size="sm" onClick={() => onAction({ kind: "scale", item: it })}>
              {t("kubernetes.scale")}
            </Button>
          )}
          {WORKLOADS.includes(kind) && (
            <Button size="sm" onClick={() => onAction({ kind: "restart", item: it })}>
              {t("kubernetes.restart")}
            </Button>
          )}
          <Button size="sm" onClick={() => onAction({ kind: "view", item: it })}>
            {t("kubernetes.view")}
          </Button>
          {kind !== "nodes" && kind !== "namespaces" && kind !== "events" && (
            <Button size="sm" variant="danger" onClick={() => onAction({ kind: "delete", item: it })}>
              {t("kubernetes.delete")}
            </Button>
          )}
        </div>
      </td>
    </tr>
  );
}
