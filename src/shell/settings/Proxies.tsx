import { useEffect, useState } from "react";
import * as api from "../../api";
import type { Proxy } from "../../api";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { AddIcon, DeleteIcon, EditIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { Select } from "../../ui/Select";
import { useAsyncToast } from "../../ui/toast";
import { proxyAsksPasswordAgain, validateProxy, type ProxyField } from "../hostForm";
import { Group, Row } from "./Group";

const NEW: Proxy = { id: "", name: "", type: "socks5", host: "", port: 1080, user: "", password: "" };
const TYPE_LABEL = { socks5: t("proxyForm.type.socks5"), http: t("proxyForm.type.http") };

/** SOCKS5 and HTTP proxies that hosts connect through (R49). A saved
 *  password is never shown; an empty one keeps it. */
export function Proxies() {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const { hosts } = useHosts();
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const [editing, setEditing] = useState<Proxy | null>(null);
  const reload = () => api.listProxies().then(setProxies).catch(() => {});
  useEffect(() => {
    reload();
  }, []);

  const usedBy = (p: Proxy) => hosts.filter((h) => h.proxyId === p.id);
  const remove = async (p: Proxy) => {
    const users = usedBy(p);
    const text = users.length
      ? t("proxyForm.deleteConfirmHosts", { name: p.name, hosts: users.map((h) => `• ${h.alias}`).join("\n") })
      : t("proxyForm.deleteConfirm", { name: p.name });
    if (!(await confirm(text))) return;
    await run(() => api.deleteProxy(p.id), t("proxyForm.deleted"));
    reload();
  };

  return (
    <Group title={t("settings.proxiesSaved")} description={t("settings.proxiesHint")}>
      <p className="text-sm text-warning max-w-prose">{t("proxyForm.cleartext")}</p>
      <Button className="w-fit" icon={AddIcon} onClick={() => setEditing({ ...NEW })}>
        {t("proxyForm.add")}
      </Button>
      {proxies.length === 0 ? (
        <p className="text-sm text-muted">{t("proxyForm.empty")}</p>
      ) : (
        <ul className="flex flex-col max-w-xl">
          {proxies.map((p) => {
            const n = usedBy(p).length;
            return (
              <Row
                key={p.id}
                actions={
                  <>
                    <IconButton label={t("proxyForm.edit", { name: p.name })} icon={EditIcon} onClick={() => setEditing({ ...p, password: "" })} />
                    <IconButton label={t("proxyForm.delete", { name: p.name })} icon={DeleteIcon} tone="danger" onClick={() => remove(p)} />
                  </>
                }
              >
                <span className="font-medium">{p.name}</span>
                <span className="text-muted allow-select">
                  {" "}
                  {TYPE_LABEL[p.type] ?? p.type} · {p.user ? `${p.user}@` : ""}
                  {p.host}:{p.port}
                </span>
                {n > 0 && <span className="block text-xs text-muted">{t("proxyForm.usedBy", { n })}</span>}
              </Row>
            );
          })}
        </ul>
      )}
      {editing && (
        <ProxyForm
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

function ProxyForm({ initial, onClose, onSaved }: { initial: Proxy; onClose: () => void; onSaved: () => void }) {
  const run = useAsyncToast();
  const [p, setP] = useState(initial);
  const set = (patch: Partial<Proxy>) => setP((x) => ({ ...x, ...patch }));
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const errors = validateProxy(p);
  const err = (f: ProxyField) => (tried && errors[f] ? t(errors[f]!) : undefined);
  const dirty = JSON.stringify(p) !== JSON.stringify(initial);

  const save = async () => {
    setTried(true);
    if (Object.keys(errors).length) return;
    setBusy(true);
    const ok = await run(() => api.saveProxy({ ...p, name: p.name.trim() }), t("proxyForm.saved"));
    setBusy(false);
    if (ok !== undefined) onSaved();
  };

  return (
    <Dialog
      title={p.id ? t("proxyForm.editTitle", { name: initial.name }) : t("proxyForm.newTitle")}
      onClose={onClose}
      dismissible={!dirty}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" form="proxy-form" disabled={busy}>
            {t("hostForm.save")}
          </Button>
        </>
      }
    >
      <form
        id="proxy-form"
        noValidate
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("proxyForm.name")} error={err("name")}>
            <Input className="w-full" autoFocus value={p.name} aria-invalid={!!err("name")} onChange={(e) => set({ name: e.target.value })} />
          </Field>
          <Field label={t("proxyForm.type")} error={err("type")}>
            <Select
              className="w-full"
              value={p.type}
              aria-invalid={!!err("type")}
              onChange={(e) => {
                const type = e.target.value as Proxy["type"];
                // Each type's usual port, unless the user set another.
                const port = p.port === (p.type === "http" ? 3128 : 1080) ? (type === "http" ? 3128 : 1080) : p.port;
                set({ type, port });
              }}
            >
              <option value="socks5">{TYPE_LABEL.socks5}</option>
              <option value="http">{TYPE_LABEL.http}</option>
            </Select>
          </Field>
        </div>
        <div className="grid grid-cols-1 sm:grid-cols-[1fr_7rem] gap-3">
          <Field label={t("proxyForm.host")} error={err("host")}>
            <Input
              className="w-full"
              spellCheck={false}
              value={p.host}
              aria-invalid={!!err("host")}
              onChange={(e) => set({ host: e.target.value })}
            />
          </Field>
          <Field label={t("proxyForm.port")} error={err("port")}>
            <Input
              className="w-full"
              type="number"
              min={1}
              max={65535}
              value={Number.isNaN(p.port) ? "" : p.port}
              aria-invalid={!!err("port")}
              onChange={(e) => set({ port: e.target.value.trim() === "" ? Number.NaN : Number(e.target.value) })}
            />
          </Field>
        </div>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("proxyForm.user")} error={err("user")}>
            <Input
              className="w-full"
              spellCheck={false}
              autoComplete="off"
              value={p.user ?? ""}
              aria-invalid={!!err("user")}
              onChange={(e) => set({ user: e.target.value })}
            />
          </Field>
          <Field label={t("proxyForm.password")} error={err("password")}>
            <Input
              type="password"
              className="w-full"
              autoComplete="new-password"
              placeholder={p.hasPassword ? t("hostForm.passwordKeep") : ""}
              value={p.password ?? ""}
              aria-invalid={!!err("password")}
              onChange={(e) => set({ password: e.target.value })}
            />
          </Field>
        </div>
        {proxyAsksPasswordAgain(initial, p) && <p className="text-xs text-warning">{t("proxyForm.passwordAgain")}</p>}
        <p className="text-xs text-warning">{t("proxyForm.cleartext")}</p>
      </form>
    </Dialog>
  );
}
