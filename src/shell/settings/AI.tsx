import { useEffect, useState } from "react";
import * as api from "../../api";
import type { AIAllowGroup, AIExecutionMode, AIMode, AIProtocol, AIProvider, HostKeyPrompt, Proxy } from "../../api";
import { TrustDialog } from "../../components/TrustDialog";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { AddIcon, DeleteIcon, EditIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { Select } from "../../ui/Select";
import { useAsyncToast, useToast } from "../../ui/toast";
import { Group, Row } from "./Group";
import { ClaudeCodeCard } from "./ClaudeCodeCard";
import { Suggestions } from "./Skills";

const BASE_URL: Record<AIProtocol, string> = { anthropic: "https://api.anthropic.com", "openai-chat": "https://api.openai.com/v1", "claude-code": "" };
const NEW: AIProvider = { id: "", name: "", protocol: "anthropic", baseUrl: BASE_URL.anthropic, model: "claude-opus-5-5", proxyId: "", unencrypted: false, keyState: "none", execPath: "", consented: false };
const NEW_CLAUDE_CODE: AIProvider = { ...NEW, name: "Claude Code", protocol: "claude-code", baseUrl: "", model: "sonnet" };
const PROTOCOL_LABEL: Record<AIProtocol, string> = {
  anthropic: t("ai.protocol.anthropic"),
  "openai-chat": t("ai.protocol.openai"),
  "claude-code": t("ai.protocol.claudeCode"),
};
const MODES: { value: AIExecutionMode; label: string; hint: string }[] = [
  { value: "confirm-all", label: t("ai.mode.confirmAll"), hint: t("ai.mode.confirmAllHint") },
  { value: "dangerous-write", label: t("ai.mode.dangerousWrite"), hint: t("ai.mode.dangerousWriteHint") },
  { value: "dangerous-only", label: t("ai.mode.dangerousOnly"), hint: t("ai.mode.dangerousOnlyHint") },
];

const KEY_STATE = { none: t("ai.key.none"), saved: t("ai.key.saved"), retype: t("ai.key.retype") };

// A cancelled Go dialog or verification changes nothing and needs no toast.
export function useWiden() {
  const { push } = useToast();
  return (e: unknown) => {
    const info = api.errorInfo(e);
    if (info.code !== "VerifyFailed") push("error", info.message);
  };
}

/** Settings > AI (R60): providers, the execution mode and this computer's
 *  allowlist. A saved key is never shown; widening what runs without asking
 *  goes through a dialog Go opens and OS verification. */
export function AI() {
  return (
    <>
      <Providers />
      <Mode />
      <Allowlist />
      <Suggestions />
    </>
  );
}

function Providers() {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [providers, setProviders] = useState<AIProvider[]>([]);
  const [editing, setEditing] = useState<AIProvider | null>(null);
  const [claude, setClaude] = useState<AIProvider | null>(null);
  const [adding, setAdding] = useState(false);
  const reload = () => api.aiListProviders().then(setProviders).catch(() => {});
  useEffect(() => {
    reload();
  }, []);

  const remove = async (p: AIProvider) => {
    if (!(await confirm(t("ai.provider.deleteConfirm", { name: p.name })))) return;
    await run(() => api.aiDeleteProvider(p.id), t("ai.provider.deleted"));
    reload();
  };

  // The provider is saved first, with no program, so Go has a record to point at one.
  const addClaude = async () => {
    if (adding) return;
    setAdding(true);
    const saved = await run(() => api.aiSaveProvider(NEW_CLAUDE_CODE, ""));
    setAdding(false);
    if (saved !== undefined) {
      reload();
      setClaude(saved as AIProvider);
    }
  };

  return (
    <Group title={t("ai.providers")} description={t("ai.providersHint")}>
      <div className="flex flex-wrap gap-2">
        <Button className="w-fit" icon={AddIcon} onClick={() => setEditing({ ...NEW })}>
          {t("ai.provider.add")}
        </Button>
        <Button className="w-fit" variant="ghost" icon={AddIcon} disabled={adding} onClick={addClaude}>
          {t("ai.cc.add")}
        </Button>
      </div>
      {providers.length === 0 ? (
        <p className="text-sm text-muted">{t("ai.provider.empty")}</p>
      ) : (
        <ul className="flex flex-col max-w-xl">
          {providers.map((p) => (
            <Row
              key={p.id}
              actions={
                <>
                  <IconButton
                    label={t("ai.provider.edit", { name: p.name })}
                    icon={EditIcon}
                    onClick={() => (p.protocol === "claude-code" ? setClaude(p) : setEditing(p))}
                  />
                  <IconButton label={t("ai.provider.delete", { name: p.name })} icon={DeleteIcon} tone="danger" onClick={() => remove(p)} />
                </>
              }
            >
              <span className="font-medium">{p.name}</span>
              <span className="text-muted allow-select">
                {" "}
                {PROTOCOL_LABEL[p.protocol] ?? p.protocol} · {p.model}
                {p.protocol === "claude-code" ? "" : ` · ${p.baseUrl}`}
              </span>
              {p.protocol === "claude-code" ? (
                <span className={`block text-xs ${p.execPath && p.consented ? "text-muted" : "text-warning"}`}>
                  {t(p.execPath && p.consented ? "ai.cc.rowReady" : "ai.cc.rowSetup")}
                </span>
              ) : (
                <span className={`block text-xs ${p.keyState === "retype" ? "text-warning" : "text-muted"}`}>{KEY_STATE[p.keyState]}</span>
              )}
            </Row>
          ))}
        </ul>
      )}
      {claude && (
        <ClaudeCodeCard
          initial={claude}
          onClose={() => setClaude(null)}
          onChanged={reload}
        />
      )}
      {editing && (
        <ProviderForm
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reload();
          }}
        />
      )}
    </Group>
  );
}

function ProviderForm({ initial, onClose, onSaved }: { initial: AIProvider; onClose: () => void; onSaved: () => void }) {
  const run = useAsyncToast();
  const [p, setP] = useState(initial);
  const [key, setKey] = useState("");
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const [models, setModels] = useState<string[]>([]);
  const [test, setTest] = useState<{ ok: boolean; text: string } | null>(null);
  const [trust, setTrust] = useState<HostKeyPrompt | null>(null);
  const [busy, setBusy] = useState(false);
  const set = (patch: Partial<AIProvider>) => {
    setP((x) => ({ ...x, ...patch }));
    setTest(null);
  };
  useEffect(() => {
    api.listProxies().then(setProxies).catch(() => {});
  }, []);
  useEffect(() => {
    api.aiDefaultModels(p.protocol).then(setModels).catch(() => {});
  }, [p.protocol]);

  // Go reuses the saved key only while the provider points where it did.
  const moved = initial.keyState === "saved" && (p.protocol !== initial.protocol || p.baseUrl.trim() !== initial.baseUrl || p.proxyId !== initial.proxyId);
  const keyHint = moved ? t("ai.key.moved") : initial.keyState === "saved" ? t("ai.key.keep") : initial.keyState === "retype" ? t("ai.key.retypeHint") : t("ai.key.hint");
  const plainHttp = /^http:\/\//i.test(p.baseUrl.trim());

  const runTest = async () => {
    setBusy(true);
    try {
      const ids = await api.aiTestProvider(p, key);
      setModels((m) => [...new Set([...ids, ...m])]);
      setTest({ ok: true, text: t("ai.test.ok", { n: ids.length }) });
    } catch (e) {
      const info = api.errorInfo(e);
      if (info.code === "HostKeyUnknown" && info.details?.requestId) setTrust(info.details as unknown as HostKeyPrompt);
      else setTest({ ok: false, text: info.message });
    } finally {
      setBusy(false);
    }
  };
  const save = async () => {
    setBusy(true);
    const ok = await run(() => api.aiSaveProvider(p, key), t("ai.provider.saved"));
    setBusy(false);
    if (ok !== undefined) onSaved();
  };

  return (
    <Dialog
      title={p.id ? t("ai.provider.editTitle", { name: initial.name }) : t("ai.provider.newTitle")}
      onClose={onClose}
      dismissible={JSON.stringify(p) === JSON.stringify(initial) && !key}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="button" onClick={runTest} disabled={busy}>
            {t("ai.test")}
          </Button>
          <Button variant="primary" type="submit" form="ai-provider-form" disabled={busy}>
            {t("hostForm.save")}
          </Button>
        </>
      }
    >
      <form
        id="ai-provider-form"
        noValidate
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("ai.provider.name")}>
            <Input className="w-full" autoFocus value={p.name} onChange={(e) => set({ name: e.target.value })} />
          </Field>
          <Field label={t("ai.provider.protocol")}>
            <Select
              className="w-full"
              value={p.protocol}
              onChange={(e) => {
                const protocol = e.target.value as AIProtocol;
                // Each protocol's usual address and model, unless the user set others.
                const baseUrl = p.baseUrl === BASE_URL[p.protocol] ? BASE_URL[protocol] : p.baseUrl;
                set({ protocol, baseUrl, model: p.model === NEW.model && protocol !== "anthropic" ? "" : p.model });
              }}
            >
              <option value="anthropic">{PROTOCOL_LABEL.anthropic}</option>
              <option value="openai-chat">{PROTOCOL_LABEL["openai-chat"]}</option>
            </Select>
          </Field>
        </div>
        <Field label={t("ai.provider.baseUrl")} hint={t("ai.provider.baseUrlHint")}>
          <Input className="w-full" value={p.baseUrl} spellCheck={false} onChange={(e) => set({ baseUrl: e.target.value })} />
        </Field>
        {plainHttp && (
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-1" checked={p.unencrypted} onChange={(e) => set({ unencrypted: e.target.checked })} />
            <span>
              {t("ai.provider.unencrypted")}
              <span className="block text-xs text-warning">{t("ai.provider.unencryptedHint")}</span>
            </span>
          </label>
        )}
        <Field label={t("ai.provider.model")}>
          <Input className="w-full" list="ai-models" value={p.model} spellCheck={false} onChange={(e) => set({ model: e.target.value })} />
        </Field>
        <datalist id="ai-models">
          {models.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
        <Field label={t("ai.provider.key")} hint={keyHint}>
          <Input className="w-full" type="password" autoComplete="off" value={key} onChange={(e) => setKey(e.target.value)} />
        </Field>
        <Field label={t("ai.provider.proxy")}>
          <Select className="w-full" value={p.proxyId} onChange={(e) => set({ proxyId: e.target.value })}>
            <option value="">{t("ai.provider.noProxy")}</option>
            {proxies.map((x) => (
              <option key={x.id} value={x.id}>
                {x.name}
              </option>
            ))}
          </Select>
        </Field>
        {test && (
          <p role="status" className={`text-sm ${test.ok ? "text-success" : "text-danger"}`}>
            {test.text}
          </p>
        )}
      </form>
      {trust && (
        <TrustDialog
          prompt={trust}
          onConfirm={() => {
            const prompt = trust;
            setTrust(null);
            api.aiTrustCert(prompt.requestId, true).then(runTest, (e) => setTest({ ok: false, text: api.errorInfo(e).message }));
          }}
          onCancel={() => {
            api.aiTrustCert(trust.requestId, false).catch(() => {});
            setTrust(null);
            setTest({ ok: false, text: t("hostForm.test.trustDeclined") });
          }}
        />
      )}
    </Dialog>
  );
}

function Mode() {
  const failed = useWiden();
  const [mode, setMode] = useState<AIMode | null>(null);
  const reload = () => api.aiGetMode().then(setMode).catch(() => {});
  useEffect(() => {
    reload();
  }, []);
  const change = async (m: AIExecutionMode) => {
    try {
      await api.aiSetMode(m);
    } catch (e) {
      failed(e);
    }
    reload();
  };
  const hint = MODES.find((m) => m.value === mode?.mode)?.hint;

  return (
    <Group title={t("ai.mode")} description={t("ai.modeHint")}>
      <Field label={t("ai.mode")} hint={hint} className="max-w-md">
        <Select className="w-full" value={mode?.mode ?? "confirm-all"} disabled={!mode} onChange={(e) => change(e.target.value as AIExecutionMode)}>
          {MODES.map((m) => (
            <option key={m.value} value={m.value}>
              {m.label}
            </option>
          ))}
        </Select>
      </Field>
      {mode?.bypass && <p className="text-sm text-warning">{t("ai.bypassOn")}</p>}
    </Group>
  );
}

function Allowlist() {
  const run = useAsyncToast();
  const failed = useWiden();
  const { hosts } = useHosts();
  const [groups, setGroups] = useState<AIAllowGroup[]>([]);
  const [hostId, setHostId] = useState("local");
  const [command, setCommand] = useState("");
  const [prefix, setPrefix] = useState(false);
  const reload = () => api.aiListAllowlist().then(setGroups).catch(() => {});
  useEffect(() => {
    reload();
  }, []);

  const add = async () => {
    try {
      await api.aiAddAllowEntry(hostId, command, prefix);
      setCommand("");
      setPrefix(false);
    } catch (e) {
      failed(e);
    }
    reload();
  };
  const remove = async (g: AIAllowGroup, command: string, prefix: boolean) => {
    await run(() => api.aiRemoveAllowEntry(g.hostId, command, prefix));
    reload();
  };

  return (
    <Group title={t("ai.allowlist")} description={t("ai.allowlistHint")}>
      <p className="text-sm text-warning max-w-prose">{t("ai.allowlistAlias")}</p>
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (command.trim()) add();
        }}
      >
        <Field label={t("ai.allow.host")}>
          <Select value={hostId} onChange={(e) => setHostId(e.target.value)}>
            <option value="local">{t("ai.allow.local")}</option>
            {hosts.map((h) => (
              <option key={h.id} value={h.id}>
                {h.alias}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t("ai.allow.command")} className="flex-1 min-w-48">
          <Input className="w-full font-mono" spellCheck={false} value={command} onChange={(e) => setCommand(e.target.value)} />
        </Field>
        <label className="flex items-center gap-2 text-sm h-9">
          <input type="checkbox" checked={prefix} onChange={(e) => setPrefix(e.target.checked)} />
          {t("ai.allow.prefix")}
        </label>
        <Button type="submit" icon={AddIcon} disabled={!command.trim()}>
          {t("ai.allow.add")}
        </Button>
      </form>
      {groups.length === 0 ? (
        <p className="text-sm text-muted">{t("ai.allow.empty")}</p>
      ) : (
        groups.map((g) => (
          <div key={g.hostId} className="flex flex-col gap-1 max-w-xl">
            <h4 className="text-xs font-semibold text-muted">{g.label}</h4>
            <ul className="flex flex-col">
              {g.entries.map((e) => (
                <Row
                  key={`${e.command}-${e.prefix}`}
                  actions={
                    <Button size="sm" variant="ghost" onClick={() => remove(g, e.command, e.prefix)}>
                      {t("ai.allow.remove")}
                    </Button>
                  }
                >
                  <code className="font-mono allow-select">{e.command}</code>
                  {e.prefix && <span className="text-muted"> {t("ai.allow.prefixShort")}</span>}
                </Row>
              ))}
            </ul>
          </div>
        ))
      )}
    </Group>
  );
}
