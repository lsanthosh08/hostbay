import { useState } from "react";
import * as api from "../../api";
import type { ClusterInfo, K8sApplied } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { controlCls } from "../../ui/Field";
import { Spinner } from "../../ui/kit";

// Go refuses more; the dialog says so before sending.
const MAX_BYTES = 1 << 20;

/**
 * Creates or updates resources from YAML (R57). A server-side dry run runs
 * first and shows what would change; Apply is offered only for the text
 * that dry run checked (KTD28).
 */
export function ApplyDialog({ info, namespace, onClose, onApplied }: { info: ClusterInfo; namespace: string; onClose: () => void; onApplied: () => void }) {
  const [text, setText] = useState("");
  const [checked, setChecked] = useState<{ text: string; results: K8sApplied[] } | null>(null);
  const [applied, setApplied] = useState<K8sApplied[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const tooBig = new TextEncoder().encode(text).length > MAX_BYTES;

  const run = async (dryRun: boolean) => {
    setBusy(true);
    setError(null);
    try {
      const results = await api.kubernetesApply(info.clusterId, namespace, text, dryRun);
      if (dryRun) setChecked({ text, results });
      else {
        setApplied(results);
        setChecked(null);
        onApplied();
      }
    } catch (e) {
      setError(api.errorInfo(e).message);
    } finally {
      setBusy(false);
    }
  };

  const list = (rs: K8sApplied[], dry: boolean) => (
    <ul className="text-xs font-mono allow-select">
      {rs.map((r) => (
        <li key={`${r.kind}/${r.namespace}/${r.name}`}>
          {r.kind} {r.namespace ? `${r.namespace}/` : ""}
          {r.name}: {dry ? t(r.outcome === "created" ? "kubernetes.outcome.created" : "kubernetes.outcome.configured") : r.outcome}
        </li>
      ))}
    </ul>
  );

  return (
    <Dialog
      title={t("kubernetes.applyTitle", { cluster: info.context })}
      onClose={onClose}
      dismissible={!text}
      size="xl"
      footer={
        <>
          {busy && <Spinner />}
          <Button type="button" onClick={onClose}>
            {t("kubernetes.close")}
          </Button>
          <Button type="button" disabled={busy || !text.trim() || tooBig} onClick={() => run(true)}>
            {t("kubernetes.dryRun")}
          </Button>
          <Button type="button" variant="primary" disabled={busy || !checked || checked.text !== text} onClick={() => run(false)}>
            {t("kubernetes.applyNow")}
          </Button>
        </>
      }
    >
      <p className="text-xs text-muted mb-2">{t("kubernetes.applyHint", { namespace: namespace || "default" })}</p>
      <textarea
        aria-label={t("kubernetes.apply")}
        className={`w-full h-72 px-3 py-2 font-mono text-xs ${controlCls}`}
        spellCheck={false}
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      {tooBig && <p className="text-xs text-danger mt-1">{t("kubernetes.tooBig")}</p>}
      <div role="status" aria-live="polite" className="mt-2 space-y-1">
        {error && <p className="text-sm text-danger allow-select break-words">{error}</p>}
        {checked && checked.text === text && (
          <>
            <p className="text-sm">{t("kubernetes.dryRunOk")}</p>
            {list(checked.results, true)}
          </>
        )}
        {applied && (
          <>
            <p className="text-sm">{t("kubernetes.applied")}</p>
            {list(applied, false)}
          </>
        )}
      </div>
    </Dialog>
  );
}
