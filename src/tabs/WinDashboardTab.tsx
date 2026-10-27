import { useEffect, useState } from "react";
import * as api from "../api";
import type { WinDashboard } from "../api";
import { t } from "../i18n";
import type { Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { Panel, Spinner, Badge } from "../ui/kit";
import { useToast } from "../ui/toast";

export function WinDashboardTab({ conn }: { conn: Connection }) {
  const { push } = useToast();
  const [info, setInfo] = useState<WinDashboard | null>(null);
  const [loading, setLoading] = useState(false);

  const load = async () => {
    setLoading(true);
    try {
      setInfo(await api.windowsDashboard(conn.sessionId));
    } catch (e) {
      push("error", e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conn.sessionId]);

  return (
    <div className="h-full p-6 overflow-auto space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">
          {conn.host.alias}{" "}
          <span className="opacity-50 text-sm">
            ({conn.host.user}@{conn.host.hostname})
          </span>
        </h1>
        <Button variant="ghost" onClick={load} disabled={loading}>
          {loading ? <Spinner /> : t("winDash.refresh")}
        </Button>
      </div>

      {!info && loading && <EmptyState>{t("winDash.loading")}</EmptyState>}

      {info && (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <Panel title={t("winDash.system")}>
            <dl className="text-sm space-y-2">
              <Row k={t("winDash.os")} v={info.os} />
              <Row k={t("winDash.uptime")} v={info.uptime} />
              <Row k={t("winDash.cpu")} v={info.cpu} />
              <Row k={t("winDash.memory")} v={info.memory} />
              <Row k={t("winDash.disk")} v={info.disk} />
            </dl>
          </Panel>
          <Panel title={t("winDash.roles")}>
            <div className="flex items-center justify-between py-1.5 text-sm">
              <span className="opacity-70">IIS (W3SVC)</span>
              <Badge tone={info.iisInstalled ? "ok" : "neutral"}>
                {info.iisInstalled ? t("winDash.installed") : t("winDash.notInstalled")}
              </Badge>
            </div>
          </Panel>
        </div>
      )}
    </div>
  );
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex justify-between gap-4">
      <dt className="opacity-60">{k}</dt>
      <dd className="text-right">{v || "—"}</dd>
    </div>
  );
}
