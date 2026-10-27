import { useCallback, useEffect, useState } from "react";
import * as api from "../api";
import type { IisSite, IisPool } from "../api";
import type { Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { Panel, Spinner, Badge } from "../ui/kit";
import { useAsyncToast } from "../ui/toast";
import { usePrompt } from "../ui/prompt";

const td = "px-3 py-1.5 border-t border-line align-top";
const th = "px-3 py-2 text-left text-xs opacity-60";

export function IisTab({ conn }: { conn: Connection }) {
  const sid = conn.sessionId;
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [sites, setSites] = useState<IisSite[]>([]);
  const [pools, setPools] = useState<IisPool[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const [s, p] = await Promise.all([api.iisSites(sid), api.iisPools(sid)]);
      setSites(s);
      setPools(p);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [sid]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const siteAct = async (name: string, action: string) => {
    if (action === "stop" && !(await confirm(`Stop site "${name}"?`))) return;
    await run(() => api.iisSiteAction(sid, name, action), `${action} ${name}`);
    refresh();
  };
  const poolAct = async (name: string, action: string) => {
    if (action === "stop" && !(await confirm(`Stop app pool "${name}"?`))) return;
    await run(() => api.iisPoolAction(sid, name, action), `${action} ${name}`);
    refresh();
  };

  const running = (s: string) => /start|running/i.test(s);

  return (
    <div className="h-full flex flex-col overflow-auto p-6 gap-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">IIS Manager</h1>
        <Button variant="ghost" onClick={refresh} disabled={loading}>
          {loading ? <Spinner /> : "Refresh"}
        </Button>
      </div>

      {err && (
        <div className="rounded-control border border-warning/40 bg-warning-soft p-3 text-sm">
          Couldn’t query IIS: {err}
          <div className="opacity-60 text-xs mt-1">
            Needs the IIS role + WebAdministration PowerShell module, and an
            admin SSH user.
          </div>
        </div>
      )}

      <Panel title="Sites">
        {sites.length === 0 && !loading ? (
          <EmptyState>No sites.</EmptyState>
        ) : (
          <div className="overflow-auto">
            <table className="w-full text-sm">
              <thead>
                <tr>
                  <th className={th}>Name</th>
                  <th className={th}>State</th>
                  <th className={th}>Bindings</th>
                  <th className={th}>Pool</th>
                  <th className={th}>Path</th>
                  <th className={th}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {sites.map((s) => (
                  <tr key={s.name}>
                    <td className={td}>{s.name}</td>
                    <td className={td}>
                      <Badge tone={running(s.state) ? "ok" : "neutral"}>{s.state}</Badge>
                    </td>
                    <td className={`${td} font-mono text-xs`}>
                      {s.bindings
                        ? s.bindings.split(",").map((b, i) => <div key={i}>{b.trim()}</div>)
                        : "—"}
                    </td>
                    <td className={td}>{s.appPool}</td>
                    <td className={`${td} font-mono text-xs opacity-60`}>{s.physicalPath}</td>
                    <td className={td}>
                      <div className="flex gap-1.5">
                        {!running(s.state) && (
                          <Button variant="primary" onClick={() => siteAct(s.name, "start")}>
                            Start
                          </Button>
                        )}
                        {running(s.state) && (
                          <Button onClick={() => siteAct(s.name, "stop")}>Stop</Button>
                        )}
                        <Button onClick={() => siteAct(s.name, "restart")}>Restart</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <Panel title="Application Pools">
        {pools.length === 0 && !loading ? (
          <EmptyState>No app pools.</EmptyState>
        ) : (
          <div className="overflow-auto">
            <table className="w-full text-sm">
              <thead>
                <tr>
                  <th className={th}>Name</th>
                  <th className={th}>State</th>
                  <th className={th}>Runtime</th>
                  <th className={th}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {pools.map((p) => (
                  <tr key={p.name}>
                    <td className={td}>{p.name}</td>
                    <td className={td}>
                      <Badge tone={running(p.state) ? "ok" : "neutral"}>{p.state}</Badge>
                    </td>
                    <td className={td}>{p.runtime || "—"}</td>
                    <td className={td}>
                      <div className="flex gap-1.5">
                        {!running(p.state) && (
                          <Button variant="primary" onClick={() => poolAct(p.name, "start")}>
                            Start
                          </Button>
                        )}
                        {running(p.state) && (
                          <Button onClick={() => poolAct(p.name, "stop")}>Stop</Button>
                        )}
                        <Button onClick={() => poolAct(p.name, "recycle")}>Recycle</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
      {loading && sites.length === 0 && (
        <div className="flex items-center gap-2 opacity-60 text-sm">
          <Spinner /> Querying IIS…
        </div>
      )}
    </div>
  );
}
