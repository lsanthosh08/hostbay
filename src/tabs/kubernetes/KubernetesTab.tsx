import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import * as api from "../../api";
import type { K8sItem, K8sMetrics } from "../../api";
import { t } from "../../i18n";
import { useSessions, type Connection } from "../../state/sessions";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Spinner } from "../../ui/kit";
import { usePrompt } from "../../ui/prompt";
import { Select } from "../../ui/Select";
import { useToast } from "../../ui/toast";
import { ApplyDialog } from "./ApplyDialog";
import { addListPage, clusters, clusterScoped, KINDS, listKey, rowsFor, type K8sKind, type ListPage } from "./clusters";
import { LogView } from "./LogView";
import { ResourceList, type RowAction } from "./ResourceList";

// Shells a pod shell can run, as an argument list: never a command line.
const SHELLS = ["sh", "bash", "ash"];

/**
 * A Kubernetes cluster's tab (R57): browse a kind of object in a
 * namespace, follow a pod's logs, open a shell in it, apply YAML, and
 * delete, scale or restart with a confirmation naming the cluster,
 * context and namespace. The session ID is the clusterId.
 */
export function KubernetesTab({ conn }: { conn: Connection }) {
  const clusterId = conn.sessionId;
  const info = clusters.get(clusterId) ?? { clusterId, context: conn.host.alias, namespace: "", server: conn.host.url ?? "", version: "" };
  const { openPodTerm } = useSessions();
  const { confirm, prompt } = usePrompt();
  const { push } = useToast();
  const [kind, setKind] = useState<K8sKind>("pods");
  const [namespace, setNamespace] = useState(info.namespace);
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [shell, setShell] = useState(SHELLS[0]);
  const [page, setPage] = useState<ListPage>({ key: "", items: [], cont: "", loading: true });
  const [metrics, setMetrics] = useState<K8sMetrics | null>(null);
  const [logs, setLogs] = useState<{ pod: string; container: string; namespace: string } | null>(null);
  const [shown, setShown] = useState<{ title: string; text: string } | null>(null);
  const [applying, setApplying] = useState(false);
  const ns = clusterScoped(kind) ? "" : namespace;
  const key = listKey(kind, ns);
  const rows = rowsFor(page, key);
  // Bumped per load; an answer that is no longer the latest is dropped, so
  // a slow list of the kind or namespace just left never replaces this one.
  const seq = useRef(0);

  const load = useCallback(
    async (cont: string) => {
      const mine = ++seq.current;
      setPage((p) => ({ ...p, loading: true, error: undefined }));
      try {
        const r = await api.kubernetesList(clusterId, kind, ns, cont);
        if (mine === seq.current) setPage((p) => addListPage(p, listKey(kind, ns), cont, r));
      } catch (e) {
        if (mine === seq.current) setPage((p) => ({ ...p, loading: false, error: api.errorInfo(e).message }));
      }
      if (mine === seq.current && (kind === "pods" || kind === "nodes")) {
        api.kubernetesMetrics(clusterId, kind === "pods" ? ns : "").then(
          (m) => mine === seq.current && setMetrics(m),
          () => mine === seq.current && setMetrics(null)
        );
      }
    },
    [clusterId, kind, ns]
  );

  useEffect(() => {
    load("");
  }, [load]);

  useEffect(() => {
    api
      .kubernetesList(clusterId, "namespaces", "", "")
      .then((r) => setNamespaces(r.items.map((i) => i.name)))
      .catch(() => setNamespaces([]));
  }, [clusterId]);

  const usage = useMemo(() => {
    const m = new Map<string, api.K8sUsage>();
    for (const u of kind === "nodes" ? metrics?.nodes ?? [] : metrics?.pods ?? []) m.set(`${u.namespace ?? ""}/${u.name}`, u);
    return m;
  }, [metrics, kind]);

  // What a confirmation names, so the user sees which cluster it touches.
  const where = (it: K8sItem) => ({ kind, name: it.name, namespace: it.namespace || "-", cluster: info.server, context: info.context });

  const act = async (a: RowAction) => {
    const it = a.item;
    const itemNs = it.namespace ?? "";
    try {
      switch (a.kind) {
        case "view":
          setShown({ title: it.name, text: await api.kubernetesGet(clusterId, kind, itemNs, it.name) });
          return;
        case "reveal":
          setShown({ title: it.name, text: t("kubernetes.revealed", { key: a.key, value: await api.kubernetesRevealSecret(clusterId, itemNs, it.name, a.key) }) });
          return;
        case "logs":
          setLogs({ pod: it.name, container: a.container, namespace: itemNs });
          return;
        case "shell":
          openPodTerm(clusterId, itemNs, it.name, a.container, [shell]);
          return;
        case "delete":
          if (!(await confirm(t("kubernetes.confirmDelete", where(it))))) return;
          await api.kubernetesDelete(clusterId, kind, itemNs, it.name);
          break;
        case "restart":
          if (!(await confirm(t("kubernetes.confirmRestart", where(it))))) return;
          await api.kubernetesRestart(clusterId, kind, itemNs, it.name);
          break;
        case "scale": {
          const v = await prompt(t("kubernetes.scalePrompt", where(it)), it.ready?.split("/")[1] ?? "1");
          if (v == null) return;
          const n = Number(v);
          if (!Number.isInteger(n) || n < 0) return push("error", t("kubernetes.scale"));
          await api.kubernetesScale(clusterId, kind, itemNs, it.name, n);
          break;
        }
      }
      push("success", t("kubernetes.done"));
      load("");
    } catch (e) {
      push("error", api.errorInfo(e).message);
    }
  };

  return (
    <div className="flex flex-col flex-1 h-full min-h-0">
      <div className="flex flex-wrap items-center gap-2 px-3 py-2 border-b border-line">
        <Select aria-label={t("kubernetes.kind")} value={kind} onChange={(e) => setKind(e.target.value as K8sKind)}>
          {KINDS.map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </Select>
        <Select aria-label={t("kubernetes.namespace")} value={namespace} disabled={clusterScoped(kind)} onChange={(e) => setNamespace(e.target.value)}>
          <option value="">{t("kubernetes.allNamespaces")}</option>
          {[...new Set([info.namespace, ...namespaces].filter(Boolean))].map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </Select>
        <Select aria-label={t("kubernetes.shellPick")} value={shell} onChange={(e) => setShell(e.target.value)}>
          {SHELLS.map((s) => (
            <option key={s}>{s}</option>
          ))}
        </Select>
        <Button size="sm" onClick={() => load("")}>
          {t("kubernetes.refresh")}
        </Button>
        <Button size="sm" variant="primary" onClick={() => setApplying(true)}>
          {t("kubernetes.apply")}
        </Button>
        <span className="text-xs text-muted ml-auto allow-select">
          {info.context} · {info.server} {info.version && `· ${info.version}`}
        </span>
      </div>
      <div role="status" aria-live="polite" className="flex flex-wrap items-center gap-2 px-3 text-xs min-h-6">
        {page.loading && <Spinner />}
        {page.error && <span className="text-danger allow-select break-words">{page.error}</span>}
        {(kind === "pods" || kind === "nodes") && metrics && !metrics.installed && <span className="text-muted">{t("kubernetes.metricsMissing")}</span>}
      </div>
      <div className="flex-1 min-h-0 overflow-auto">
        <ResourceList kind={kind} items={rows} allNamespaces={!clusterScoped(kind) && !namespace} usage={usage} onAction={act} />
        {page.key === key && page.cont && !page.loading && (
          <div className="p-3">
            <Button size="sm" onClick={() => load(page.cont)}>
              {t("kubernetes.loadMore")}
            </Button>
          </div>
        )}
      </div>
      {logs && <LogView clusterId={clusterId} namespace={logs.namespace} pod={logs.pod} container={logs.container} onClose={() => setLogs(null)} />}
      {shown && (
        <Dialog title={shown.title} onClose={() => setShown(null)} size="xl">
          <pre className="text-xs font-mono whitespace-pre-wrap break-all allow-select">{shown.text}</pre>
        </Dialog>
      )}
      {applying && <ApplyDialog info={info} namespace={namespace} onClose={() => setApplying(false)} onApplied={() => load("")} />}
    </div>
  );
}
