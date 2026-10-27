import { useEffect, useState, type ReactNode } from "react";
import * as api from "../api";
import type { MonitorEvent, MonitorSample } from "../api";
import { t, type MessageKey } from "../i18n";
import type { Connection } from "../state/sessions";
import { EmptyState } from "../ui/EmptyState";
import { MonitorIcon } from "../ui/icons";
import { Badge, Panel, Spinner } from "../ui/kit";
import { Select } from "../ui/Select";
import { useToast } from "../ui/toast";
import { humanSize } from "./files/paths";
import { Sparkline } from "./monitor/Sparkline";
import { sortProcs, type ProcKey } from "./monitor/monitor";

const INTERVALS = [2, 3, 5, 10, 30];
const KEEP = 300; // the backend keeps as many (monitor.HistoryLen)

const th = "text-left font-medium px-3 py-2 opacity-70";
const td = "px-3 py-2 border-t border-line align-top";

const STATE_TONE = { starting: "neutral", live: "ok", error: "warn", disconnected: "bad", unsupported: "neutral" } as const;
const STATE_LABEL: Record<api.MonitorState, MessageKey> = {
  starting: "monitor.state.starting",
  live: "monitor.state.live",
  error: "monitor.state.error",
  disconnected: "monitor.state.disconnected",
  unsupported: "monitor.state.unsupported",
};

/** Live figures for an SSH host (R59). The tab starts its monitor the first
 *  time it shows, so a session whose Monitor view is never opened samples
 *  nothing, and reports when it is hidden so the sampler can pause. Every
 *  string here came from the host and renders as text. */
export function MonitorTab({ conn, visible }: { conn: Connection; visible: boolean }) {
  const { push } = useToast();
  const [seen, setSeen] = useState(visible);
  const [id, setId] = useState<string | null>(null);
  const [history, setHistory] = useState<MonitorSample[]>([]);
  const [ev, setEv] = useState<MonitorEvent>({ state: "starting" });
  const [secs, setSecs] = useState(3);
  const [sort, setSort] = useState<{ key: ProcKey; dir: "asc" | "desc" }>({ key: "cpu", dir: "desc" });

  useEffect(() => {
    if (visible) setSeen(true);
  }, [visible]);

  useEffect(() => {
    if (!seen) return;
    let live = true;
    let stop: (() => void) | undefined;
    (async () => {
      try {
        const m = await api.monitorStart(conn.sessionId, conn.host.id);
        setHistory(m.history);
        setEv(m.state);
        setSecs(m.interval);
        // shortcut: a sample that lands before this subscribes is missed;
        // the next tick brings one.
        const off = await api.onMonitorSample(m.id, (e) => {
          setEv(e);
          if (e.sample) setHistory((h) => [...h.slice(-(KEEP - 1)), e.sample!]);
        });
        stop = () => {
          off();
          api.monitorStop(m.id);
        };
        if (!live) return stop();
        setId(m.id);
      } catch (e) {
        if (live) push("error", api.errorInfo(e).message);
      }
    })();
    return () => {
      live = false;
      stop?.();
    };
  }, [seen, conn.sessionId, conn.host.id, push]);

  useEffect(() => {
    if (id) api.monitorVisible(id, visible).catch(() => {});
  }, [id, visible]);

  if (!seen) return null;
  if (ev.state === "unsupported")
    return (
      <EmptyState icon={MonitorIcon} title={t("monitor.unsupported.title")}>
        {t("monitor.unsupported.body", { os: ev.os ?? "" })}
      </EmptyState>
    );

  const latest = history[history.length - 1];
  const changeInterval = (n: number) => {
    if (id) api.monitorInterval(id, n).then(setSecs, (e) => push("error", api.errorInfo(e).message));
  };
  const toggleSort = (key: ProcKey) =>
    setSort((s) => ({ key, dir: s.key === key && s.dir === "desc" ? "asc" : "desc" }));
  const procs = sortProcs(latest?.procs ?? [], sort.key, sort.dir);
  const sortTh = (key: ProcKey, label: MessageKey) => (
    <th className={th} aria-sort={sort.key === key ? (sort.dir === "asc" ? "ascending" : "descending") : "none"}>
      <button type="button" className="hover:underline" onClick={() => toggleSort(key)}>
        {t(label)}
        {sort.key === key ? (sort.dir === "asc" ? " ↑" : " ↓") : ""}
      </button>
    </th>
  );

  return (
    <div className="h-full p-6 overflow-auto space-y-4">
      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold flex items-center gap-2">
          {conn.host.alias}
          <Badge tone={STATE_TONE[ev.state]}>{t(STATE_LABEL[ev.state])}</Badge>
        </h1>
        <label className="flex items-center gap-2 text-sm">
          <span className="text-muted">{t("monitor.interval")}</span>
          <Select compact value={secs} disabled={!id} onChange={(e) => changeInterval(Number(e.target.value))}>
            {INTERVALS.map((n) => (
              <option key={n} value={n}>
                {t("monitor.seconds", { n })}
              </option>
            ))}
          </Select>
        </label>
      </div>

      {ev.state === "disconnected" && <Notice>{t("monitor.disconnected")}</Notice>}
      {ev.state === "error" && <Notice>{t("monitor.error", { error: ev.error ?? "" })}</Notice>}
      {latest?.partial && <Notice>{t("monitor.partial")}</Notice>}

      {!latest ? (
        <EmptyState icon={MonitorIcon} title={t("monitor.waiting")}>
          <Spinner />
        </EmptyState>
      ) : (
        <>
          <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4">
            <Stat
              label={t("monitor.cpu")}
              value={`${latest.cpu.toFixed(1)}%`}
              note={t("monitor.cores", { n: latest.cores })}
            >
              <Sparkline values={history.map((s) => s.cpu)} top={100} label={t("monitor.cpu")} />
            </Stat>
            <Stat
              label={t("monitor.memory")}
              value={t("monitor.ofTotal", { used: humanSize(latest.memUsed), total: humanSize(latest.memTotal) })}
              note={
                latest.swapTotal > 0
                  ? `${t("monitor.swap")} ${t("monitor.ofTotal", { used: humanSize(latest.swapUsed), total: humanSize(latest.swapTotal) })}`
                  : undefined
              }
            >
              <Sparkline values={history.map((s) => s.memUsed)} top={latest.memTotal} label={t("monitor.memory")} />
            </Stat>
            <Stat
              label={t("monitor.network")}
              value={t("monitor.rate", { rx: humanSize(Math.round(latest.rxRate)), tx: humanSize(Math.round(latest.txRate)) })}
            >
              <Sparkline values={history.map((s) => s.rxRate + s.txRate)} label={t("monitor.network")} />
            </Stat>
            <Stat label={t("monitor.load")} value={latest.load.map((l) => l.toFixed(2)).join("  ")}>
              <Sparkline values={history.map((s) => s.load[0])} label={t("monitor.load")} />
            </Stat>
          </div>

          <Panel title={t("monitor.disks")} bodyClassName="overflow-auto">
            <table className="w-full text-sm">
              <thead>
                <tr>
                  <th className={th}>{t("monitor.col.mount")}</th>
                  <th className={th}>{t("monitor.col.device")}</th>
                  <th className={th}>{t("monitor.col.used")}</th>
                </tr>
              </thead>
              <tbody>
                {latest.disks.map((d) => (
                  <tr key={`${d.fs} ${d.mount}`}>
                    <td className={`${td} font-mono text-xs`}>{d.mount}</td>
                    <td className={`${td} font-mono text-xs`}>{d.fs}</td>
                    <td className={td}>
                      <div className="flex items-center gap-2">
                        <div className="h-1.5 w-24 rounded-full bg-control-bg overflow-hidden">
                          <div className="h-full bg-accent" style={{ width: `${(100 * d.used) / d.size}%` }} />
                        </div>
                        {t("monitor.ofTotal", { used: humanSize(d.used), total: humanSize(d.size) })}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Panel>

          <Panel
            title={t("monitor.processes")}
            actions={<span className="text-xs text-muted">{t("monitor.topOf", { shown: procs.length, total: latest.procCount })}</span>}
            bodyClassName="overflow-auto max-h-[28rem]"
          >
            <table className="w-full text-sm">
              <thead>
                <tr>
                  {sortTh("pid", "monitor.col.pid")}
                  {sortTh("user", "monitor.col.user")}
                  {sortTh("cpu", "monitor.col.cpu")}
                  {sortTh("rss", "monitor.col.memory")}
                  {sortTh("command", "monitor.col.command")}
                </tr>
              </thead>
              <tbody>
                {procs.map((p) => (
                  <tr key={p.pid}>
                    <td className={`${td} tabular-nums`}>{p.pid}</td>
                    <td className={td}>{p.user}</td>
                    <td className={`${td} tabular-nums`}>{p.cpu < 0 ? "—" : p.cpu.toFixed(1)}</td>
                    <td className={`${td} tabular-nums`}>{humanSize(p.rss)}</td>
                    <td className={`${td} font-mono text-xs break-all`}>{p.command}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Panel>

          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
            <Panel title={t("monitor.ports")} bodyClassName="overflow-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr>
                    <th className={th}>{t("monitor.col.proto")}</th>
                    <th className={th}>{t("monitor.col.address")}</th>
                    <th className={th}>{t("monitor.col.port")}</th>
                    <th className={th}>{t("monitor.col.process")}</th>
                  </tr>
                </thead>
                <tbody>
                  {(latest.ports ?? []).map((p, i) => (
                    <tr key={i}>
                      <td className={td}>{p.proto}</td>
                      <td className={`${td} font-mono text-xs`}>{p.addr}</td>
                      <td className={`${td} tabular-nums`}>{p.port}</td>
                      <td className={td}>{p.process || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Panel>
            <Panel title={t("monitor.interfaces")} bodyClassName="overflow-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr>
                    <th className={th}>{t("monitor.col.name")}</th>
                    <th className={th}>{t("monitor.col.state")}</th>
                    <th className={th}>{t("monitor.col.addresses")}</th>
                    <th className={th}>{t("monitor.network")}</th>
                  </tr>
                </thead>
                <tbody>
                  {latest.ifaces.map((i) => (
                    <tr key={i.name}>
                      <td className={td}>
                        {i.name}
                        <div className="text-xs text-muted font-mono">
                          {i.mac} · {t("monitor.col.mtu")} {i.mtu}
                        </div>
                      </td>
                      <td className={td}>{i.state}</td>
                      <td className={`${td} font-mono text-xs`}>
                        {i.addrs.map((a) => (
                          <div key={a}>{a}</div>
                        ))}
                      </td>
                      <td className={`${td} tabular-nums text-xs`}>
                        {t("monitor.rate", { rx: humanSize(Math.round(i.rxRate)), tx: humanSize(Math.round(i.txRate)) })}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Panel>
          </div>
        </>
      )}
    </div>
  );
}

function Stat({ label, value, note, children }: { label: string; value: string; note?: string; children: ReactNode }) {
  return (
    <Panel bodyClassName="p-4 space-y-2">
      <div className="text-xs text-muted">{label}</div>
      <div className="text-lg font-semibold tabular-nums">{value}</div>
      {children}
      {note && <div className="text-xs text-muted">{note}</div>}
    </Panel>
  );
}

function Notice({ children }: { children: ReactNode }) {
  return <div className="rounded-control border border-warning/40 bg-warning-soft p-3 text-sm">{children}</div>;
}
