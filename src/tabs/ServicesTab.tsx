import { useEffect, useState } from "react";
import * as api from "../api";
import type { SystemdUnit, Script } from "../api";
import { t } from "../i18n";
import type { Connection } from "../state/sessions";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { controlCls, Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { AddIcon, DeleteIcon } from "../ui/icons";
import { Panel, Spinner, Badge } from "../ui/kit";
import { useToast, useAsyncToast } from "../ui/toast";

export function ServicesTab({ conn }: { conn: Connection }) {
  const { push } = useToast();
  const run = useAsyncToast();
  const [units, setUnits] = useState<SystemdUnit[]>([]);
  const [filter, setFilter] = useState("");
  const [loading, setLoading] = useState(false);
  const [output, setOutput] = useState("");
  const [scripts, setScripts] = useState<Script[]>([]);
  const [editing, setEditing] = useState(false);

  const loadUnits = async () => {
    setLoading(true);
    try {
      setUnits(await api.systemdList(conn.sessionId));
    } catch (e) {
      push("error", e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };
  const loadScripts = () => api.listScripts().then(setScripts).catch(() => {});

  useEffect(() => {
    loadUnits();
    loadScripts();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conn.sessionId]);

  const systemd = async (unit: string, action: string) => {
    const res = await run(() => api.systemdAction(conn.sessionId, unit, action), `${action} ${unit}`);
    if (res) setOutput((res.stdout + "\n" + res.stderr).trim());
    loadUnits();
  };

  const runScript = async (s: Script) => {
    const res = await run(() => api.exec(conn.sessionId, s.script), s.label);
    if (res) setOutput((res.stdout + "\n" + res.stderr).trim());
  };

  const shown = units.filter((u) => u.unit.toLowerCase().includes(filter.toLowerCase()));

  return (
    <div className="h-full p-6 overflow-auto space-y-4">
      <Panel
        title={t("services.scripts")}
        actions={
          <Button variant="ghost" onClick={() => setEditing((e) => !e)}>
            {editing ? t("services.done") : t("services.edit")}
          </Button>
        }
      >
        {editing ? (
          <ScriptEditor
            scripts={scripts}
            onSave={async (list) => {
              await run(() => api.saveScripts(list), t("services.scriptsSaved"));
              setScripts(list);
              setEditing(false);
            }}
          />
        ) : (
          <div className="flex flex-wrap gap-2">
            {scripts.length === 0 && <span className="text-sm opacity-50">{t("services.noScripts")}</span>}
            {scripts.map((s) => (
              <Button key={s.id} onClick={() => runScript(s)}>
                {s.label}
              </Button>
            ))}
          </div>
        )}
      </Panel>

      {/* Output right below the scripts/nginx actions. */}
      {output && (
        <Panel title={t("services.output")}>
          <pre className="text-xs whitespace-pre-wrap max-h-60 overflow-auto">{output}</pre>
        </Panel>
      )}

      <Panel
        title={t("services.systemd")}
        actions={
          <>
            <Input
              placeholder={t("services.filter")}
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              className="w-40"
            />
            <Button variant="ghost" onClick={loadUnits} disabled={loading}>
              {loading ? <Spinner /> : t("services.refresh")}
            </Button>
          </>
        }
      >
        {shown.length === 0 ? (
          <EmptyState>{t("services.empty")}</EmptyState>
        ) : (
          <div className="max-h-80 overflow-auto text-sm">
            <table className="w-full">
              <thead className="text-left opacity-60 sticky top-0 bg-surface">
                <tr>
                  <th className="py-1">{t("services.col.unit")}</th>
                  <th>{t("services.col.active")}</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {shown.map((u) => (
                  <tr key={u.unit} className="border-t border-line">
                    <td className="py-1 pr-2">
                      <div>{u.unit}</div>
                      <div className="opacity-50 text-xs">{u.description}</div>
                    </td>
                    <td>
                      <Badge tone={u.active === "active" ? "ok" : "neutral"}>
                        {u.active}/{u.sub}
                      </Badge>
                    </td>
                    <td className="text-right whitespace-nowrap">
                      <button
                        className="text-xs px-2 py-0.5 mr-1 rounded bg-control-bg"
                        onClick={() => systemd(u.unit, "restart")}
                      >
                        {t("services.restart")}
                      </button>
                      <button
                        className="text-xs px-2 py-0.5 mr-1 rounded bg-control-bg"
                        onClick={() => systemd(u.unit, u.active === "active" ? "stop" : "start")}
                      >
                        {u.active === "active" ? t("services.stop") : t("services.start")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </div>
  );
}

function ScriptEditor({
  scripts,
  onSave,
}: {
  scripts: Script[];
  onSave: (s: Script[]) => void;
}) {
  const [list, setList] = useState<Script[]>(scripts);
  const update = (i: number, patch: Partial<Script>) =>
    setList((l) => l.map((s, idx) => (idx === i ? { ...s, ...patch } : s)));
  const add = () =>
    setList((l) => [...l, { id: crypto.randomUUID(), label: t("services.newScript"), script: "" }]);
  const remove = (i: number) => setList((l) => l.filter((_, idx) => idx !== i));

  return (
    <div className="space-y-2">
      {list.map((s, i) => (
        <div key={s.id} className="flex gap-2 items-start">
          <Input
            value={s.label}
            onChange={(e) => update(i, { label: e.target.value })}
            className="w-40"
            placeholder={t("services.label")}
          />
          <textarea
            value={s.script}
            onChange={(e) => update(i, { script: e.target.value })}
            className={`flex-1 px-3 py-1.5 font-mono ${controlCls}`}
            rows={1}
            placeholder={t("services.command")}
          />
          <IconButton label={t("services.removeScript")} icon={DeleteIcon} tone="danger" onClick={() => remove(i)} />
        </div>
      ))}
      <div className="flex gap-2">
        <Button variant="ghost" icon={AddIcon} onClick={add}>
          {t("services.add")}
        </Button>
        <Button variant="primary" onClick={() => onSave(list)}>
          {t("services.save")}
        </Button>
      </div>
    </div>
  );
}
