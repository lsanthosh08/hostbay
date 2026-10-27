import { useEffect, useState } from "react";
import * as api from "../api";
import type { DashboardInfo } from "../api";
import type { Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { Panel, Spinner, Badge } from "../ui/kit";
import { useToast } from "../ui/toast";

export function DashboardTab({ conn }: { conn: Connection }) {
  const { push } = useToast();
  const [info, setInfo] = useState<DashboardInfo | null>(null);
  const [loading, setLoading] = useState(false);

  const load = async () => {
    setLoading(true);
    try {
      setInfo(await api.dashboard(conn.sessionId));
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

  const stat = (label: string, ok: boolean, okText = "Yes", badText = "No") => (
    <div className="flex items-center justify-between py-1.5">
      <span className="opacity-70 text-sm">{label}</span>
      <Badge tone={ok ? "ok" : "bad"}>{ok ? okText : badText}</Badge>
    </div>
  );

  return (
    <div className="h-full p-6 overflow-auto space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">
          {conn.host.alias}{" "}
          <span className="opacity-50 text-sm">
            ({conn.host.user}@{conn.host.hostname}:{conn.host.port})
          </span>
        </h1>
        <Button variant="ghost" onClick={load} disabled={loading}>
          {loading ? <Spinner /> : "Refresh"}
        </Button>
      </div>

      {!info && loading && <EmptyState>Loading system info…</EmptyState>}

      {info && (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <Panel title="System">
            <dl className="text-sm space-y-2">
              <Row k="OS" v={info.os} />
              <Row k="Uptime" v={info.uptime} />
              <Row k="Load" v={info.load} />
              <Row k="Disk /" v={info.disk} />
            </dl>
          </Panel>
          <Panel title="Installed">
            {stat("Nginx", info.nginxInstalled, "Installed", "Missing")}
            {stat("Docker", info.dockerInstalled, "Installed", "Missing")}
            {stat("Docker permission", info.dockerPermission, "Granted", "Denied")}
            {stat("Azure agent", info.azureAgentInstalled, "Configured", "No")}
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
