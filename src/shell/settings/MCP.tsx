import { useEffect, useState } from "react";
import * as api from "../../api";
import { hostKind } from "../../api";
import type { MCPAllowEntry, MCPNewToken, MCPStatus, MCPToken } from "../../api";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { Field, Input } from "../../ui/Field";
import { AddIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { Select } from "../../ui/Select";
import { useAsyncToast, useToast } from "../../ui/toast";
import { useWiden } from "./AI";
import { Group, Row } from "./Group";

const date = (ms?: number) => (ms ? new Date(ms).toLocaleString() : t("mcp.token.never"));

/** Settings > MCP (R63, KTD35): the server switch and port, the tokens,
 *  each token's exact allowlist entries and the MCP folder. All of it is
 *  this computer's own and never syncs. A token is never shown here: Go
 *  copies a new one to the clipboard once. */
export function MCP() {
  const [status, setStatus] = useState<MCPStatus | null>(null);
  const [tokens, setTokens] = useState<MCPToken[]>([]);
  const reloadStatus = () => api.mcpStatus().then(setStatus).catch(() => {});
  const reloadTokens = () => api.mcpListTokens().then(setTokens).catch(() => {});
  useEffect(() => {
    reloadStatus();
    reloadTokens();
  }, []);
  return (
    <>
      <Server status={status} onChange={reloadStatus} />
      <Tokens tokens={tokens} onChange={reloadTokens} />
      <Allowlist tokens={tokens} />
      <Folder status={status} onChange={reloadStatus} />
    </>
  );
}

function Server({ status, onChange }: { status: MCPStatus | null; onChange: () => void }) {
  const run = useAsyncToast();
  const failed = useWiden();
  const [port, setPort] = useState("");
  useEffect(() => {
    if (status) setPort(String(status.port));
  }, [status?.port]);

  const toggle = async (on: boolean) => {
    try {
      await api.mcpSetEnabled(on);
    } catch (e) {
      failed(e);
    }
    onChange();
  };

  return (
    <Group title={t("mcp.server")} description={t("mcp.serverHint")}>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" disabled={!status} checked={!!status?.enabled} onChange={(e) => toggle(e.target.checked)} />
        {t("mcp.enable")}
      </label>
      <form
        className="flex items-end gap-2"
        onSubmit={async (e) => {
          e.preventDefault();
          await run(() => api.mcpSetPort(Number(port)), t("mcp.portSaved"));
          onChange();
        }}
      >
        <Field label={t("mcp.port")}>
          <Input className="w-28" inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ""))} />
        </Field>
        <Button type="submit" disabled={!port || Number(port) === status?.port}>
          {t("mcp.portSave")}
        </Button>
      </form>
      {status && (
        <p className="text-sm">
          <span className={status.running ? "text-success" : "text-muted"}>{status.running ? t("mcp.running") : t("mcp.stopped")}</span>
          {status.running && <code className="ml-2 font-mono allow-select">{status.endpoint}</code>}
        </p>
      )}
      {status?.error && <p className="text-sm text-danger">{status.error}</p>}
    </Group>
  );
}

function Tokens({ tokens, onChange }: { tokens: MCPToken[]; onChange: () => void }) {
  const run = useAsyncToast();
  const failed = useWiden();
  const { confirm } = usePrompt();
  const [name, setName] = useState("");
  const [created, setCreated] = useState<MCPNewToken | null>(null);

  const create = async () => {
    try {
      setCreated(await api.mcpCreateToken(name));
      setName("");
    } catch (e) {
      failed(e);
    }
    onChange();
  };
  const revoke = async (tok: MCPToken) => {
    if (!(await confirm(t("mcp.token.revokeConfirm", { name: tok.name })))) return;
    await run(() => api.mcpRevokeToken(tok.id), t("mcp.token.revoked"));
    if (created?.token.id === tok.id) setCreated(null);
    onChange();
  };

  return (
    <Group title={t("mcp.tokens")} description={t("mcp.tokensHint")}>
      <form
        className="flex items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim()) create();
        }}
      >
        <Field label={t("mcp.token.name")} className="min-w-48">
          <Input className="w-full" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Button type="submit" icon={AddIcon} disabled={!name.trim()}>
          {t("mcp.token.create")}
        </Button>
      </form>
      {created && (
        <div role="status" className="panel rounded-card p-3 flex flex-col gap-2 max-w-xl text-sm">
          <p className={created.copied ? "text-success" : "text-danger"}>
            {created.copied ? t("mcp.token.copied", { name: created.token.name }) : t("mcp.token.notCopied")}
          </p>
          <p className="text-muted">{t("mcp.token.config")}</p>
          <pre tabIndex={0} className="max-h-60 overflow-auto p-2 rounded-control bg-raised font-mono text-xs allow-select">
            {created.config}
          </pre>
          <Button className="w-fit" variant="ghost" size="sm" onClick={() => setCreated(null)}>
            {t("mcp.token.done")}
          </Button>
        </div>
      )}
      {tokens.length === 0 ? (
        <p className="text-sm text-muted">{t("mcp.token.empty")}</p>
      ) : (
        <ul className="flex flex-col max-w-xl">
          {tokens.map((tok) => (
            <Row
              key={tok.id}
              actions={
                <Button size="sm" variant="ghost" onClick={() => revoke(tok)}>
                  {t("mcp.token.revoke")}
                </Button>
              }
            >
              <span className="font-medium">{tok.name}</span>
              <span className="block text-xs text-muted">
                {t("mcp.token.dates", { created: date(tok.created), used: date(tok.lastUsed) })}
              </span>
            </Row>
          ))}
        </ul>
      )}
    </Group>
  );
}

function Allowlist({ tokens }: { tokens: MCPToken[] }) {
  const run = useAsyncToast();
  const failed = useWiden();
  const { hosts } = useHosts();
  const ssh = hosts.filter((h) => hostKind(h) === "ssh");
  const [entries, setEntries] = useState<MCPAllowEntry[]>([]);
  const [tokenId, setTokenId] = useState("");
  const [hostId, setHostId] = useState("");
  const [command, setCommand] = useState("");
  const reload = () => api.mcpListAllowlist().then(setEntries).catch(() => {});
  useEffect(() => {
    reload();
  }, [tokens]);

  const add = async () => {
    try {
      await api.mcpAddAllowEntry(tokenId || tokens[0]?.id, hostId || ssh[0]?.id, command);
      setCommand("");
    } catch (e) {
      failed(e);
    }
    reload();
  };

  return (
    <Group title={t("mcp.allowlist")} description={t("mcp.allowlistHint")}>
      {tokens.length > 0 && ssh.length > 0 && (
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (command.trim()) add();
          }}
        >
          <Field label={t("mcp.allow.token")}>
            <Select value={tokenId || tokens[0].id} onChange={(e) => setTokenId(e.target.value)}>
              {tokens.map((tok) => (
                <option key={tok.id} value={tok.id}>
                  {tok.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("mcp.allow.host")}>
            <Select value={hostId || ssh[0].id} onChange={(e) => setHostId(e.target.value)}>
              {ssh.map((h) => (
                <option key={h.id} value={h.id}>
                  {h.alias}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("mcp.allow.command")} className="flex-1 min-w-48">
            <Input className="w-full font-mono" spellCheck={false} value={command} onChange={(e) => setCommand(e.target.value)} />
          </Field>
          <Button type="submit" icon={AddIcon} disabled={!command.trim()}>
            {t("mcp.allow.add")}
          </Button>
        </form>
      )}
      {entries.length === 0 ? (
        <p className="text-sm text-muted">{t("mcp.allow.empty")}</p>
      ) : (
        <ul className="flex flex-col max-w-xl">
          {entries.map((e) => (
            <Row
              key={`${e.tokenId}-${e.hostId}-${e.command}`}
              actions={
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={async () => {
                    await run(() => api.mcpRemoveAllowEntry(e.tokenId, e.hostId, e.command));
                    reload();
                  }}
                >
                  {t("mcp.allow.remove")}
                </Button>
              }
            >
              <code className="font-mono allow-select">{e.command}</code>
              <span className="block text-xs text-muted">{t("mcp.allow.where", { token: e.tokenName, host: e.hostLabel })}</span>
            </Row>
          ))}
        </ul>
      )}
    </Group>
  );
}

function Folder({ status, onChange }: { status: MCPStatus | null; onChange: () => void }) {
  const { push } = useToast();
  return (
    <Group title={t("mcp.folder")} description={t("mcp.folderHint")}>
      <p className="text-sm font-mono allow-select break-all">{status?.folder || t("mcp.folderNone")}</p>
      <Button className="w-fit" onClick={() => api.mcpPickFolder().then((dir) => void (dir && onChange()), (e) => push("error", api.errorInfo(e).message))}>
        {t("mcp.folderChange")}
      </Button>
    </Group>
  );
}
