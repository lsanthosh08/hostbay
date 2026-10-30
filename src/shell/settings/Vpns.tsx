import { useEffect, useState, type ReactNode } from "react";
import * as api from "../../api";
import type { Vpn } from "../../api";
import { reloadVpns, useVpns } from "../../components/VpnPanel";
import { TagEditor } from "../../components/TagEditor";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { AddIcon, DeleteIcon, EditIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { SegmentedControl } from "../../ui/SegmentedControl";
import { Select } from "../../ui/Select";
import { useAsyncToast } from "../../ui/toast";
import {
  AUTH_LABEL,
  FULL_TUNNEL,
  VPN_TYPES,
  authsFor,
  defaultPort,
  isFullTunnel,
  needsGateway,
  tierFor,
  typeLabel,
  validateVpn,
  vpnAsksSecretAgain,
  type VpnField,
} from "../vpnForm";
import { Group, Row } from "./Group";

export const NEW_VPN: Vpn = { id: "", name: "", type: "wireguard", tier: "app", port: 51820, routes: [], tags: [] };
const noSecrets = { password: "", privateKey: "", presharedKey: "", authKey: "" };

/** Saved VPN profiles (R1): add, edit and delete. A saved secret is never
 *  shown; an empty field keeps it. */
export function Vpns() {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const { hosts } = useHosts();
  const { vpns } = useVpns();
  const [editing, setEditing] = useState<Vpn | null>(null);
  useEffect(() => {
    reloadVpns();
  }, []);

  const usedBy = (v: Vpn) => hosts.filter((h) => h.vpnId === v.id);
  const remove = async (v: Vpn) => {
    const users = usedBy(v);
    const text = users.length
      ? t("vpnForm.deleteConfirmHosts", { name: v.name, hosts: users.map((h) => `• ${h.alias}`).join("\n") })
      : t("vpnForm.deleteConfirm", { name: v.name });
    if (!(await confirm(text))) return;
    await run(() => api.deleteVpn(v.id), t("vpnForm.deleted"));
    reloadVpns();
  };

  return (
    <Group title={t("settings.vpnsSaved")} description={t("settings.vpnsHint")}>
      <Button className="w-fit" icon={AddIcon} onClick={() => setEditing({ ...NEW_VPN })}>
        {t("vpn.new")}
      </Button>
      {vpns.length === 0 ? (
        <p className="text-sm text-muted">{t("vpn.empty.title")}</p>
      ) : (
        <ul className="flex flex-col max-w-xl">
          {vpns.map((v) => {
            const n = usedBy(v).length;
            return (
              <Row
                key={v.id}
                actions={
                  <>
                    <IconButton label={t("vpn.edit", { name: v.name })} icon={EditIcon} onClick={() => setEditing(v)} />
                    <IconButton label={t("vpnForm.delete", { name: v.name })} icon={DeleteIcon} tone="danger" onClick={() => remove(v)} />
                  </>
                }
              >
                <span className="font-medium">{v.name}</span>
                <span className="text-muted allow-select">
                  {" "}
                  {typeLabel(v.type)}
                  {v.gateway ? ` · ${v.gateway}` : ""}
                </span>
                {n > 0 && <span className="block text-xs text-muted">{t("proxyForm.usedBy", { n })}</span>}
              </Row>
            );
          })}
        </ul>
      )}
      {editing && (
        <VpnForm
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reloadVpns();
          }}
        />
      )}
    </Group>
  );
}

/** Creates or edits one profile: type, sign-in per type, routes (full or
 *  split tunnel), DNS and MTU (R20, R42). */
export function VpnForm({ initial, onClose, onSaved }: { initial: Vpn; onClose: () => void; onSaved: () => void }) {
  const run = useAsyncToast();
  const [start] = useState<Vpn>(() => ({ ...initial, ...noSecrets }));
  const [v, setV] = useState(start);
  const set = (patch: Partial<Vpn>) => setV((x) => ({ ...x, ...patch }));
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const errors = validateVpn(v);
  const err = (f: VpnField) => (tried && errors[f] ? t(errors[f]!) : undefined);
  const dirty = JSON.stringify(v) !== JSON.stringify(start);
  const wg = v.type === "wireguard";
  const auths = authsFor(v.type);
  const auth = auths.length ? v.auth || auths[0] : "";
  const external = v.tier === "external";
  const full = isFullTunnel(v.routes);
  const keep = v.hasSecret ? t("hostForm.passwordKeep") : "";

  const save = async () => {
    setTried(true);
    if (Object.keys(errors).length) return;
    setBusy(true);
    const ok = await run(() => api.saveVpn({ ...v, name: v.name.trim(), gateway: v.gateway?.trim(), auth }), t("vpnForm.saved"));
    setBusy(false);
    if (ok !== undefined) onSaved();
  };

  const secret = (label: string, field: "password" | "privateKey" | "presharedKey" | "authKey", error?: string) => (
    <Field label={label} error={error}>
      <Input
        type="password"
        className="w-full"
        autoComplete="new-password"
        placeholder={keep}
        value={v[field] ?? ""}
        aria-invalid={!!error}
        onChange={(e) => set({ [field]: e.target.value })}
      />
    </Field>
  );
  const list = (label: string, field: "address" | "routes" | "dnsServers" | "dnsDomains", placeholder: string, hint?: ReactNode) => (
    <Field label={label} hint={hint} error={err(field)}>
      <TagEditor tags={v[field] ?? []} placeholder={placeholder} invalid={!!err(field)} onChange={(x) => set({ [field]: x })} />
    </Field>
  );

  return (
    <Dialog
      title={v.id ? t("vpnForm.editTitle", { name: initial.name }) : t("vpnForm.newTitle")}
      onClose={onClose}
      dismissible={!dirty}
      size="lg"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" form="vpn-form" disabled={busy}>
            {t("hostForm.save")}
          </Button>
        </>
      }
    >
      <form
        id="vpn-form"
        noValidate
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("vpnForm.name")} error={err("name")}>
            <Input className="w-full" autoFocus value={v.name} aria-invalid={!!err("name")} onChange={(e) => set({ name: e.target.value })} />
          </Field>
          <Field label={t("vpnForm.type")} hint={v.type === "wireguard" ? undefined : t("vpnForm.typeLater")}>
            <Select
              className="w-full"
              value={v.type}
              onChange={(e) => {
                const type = e.target.value;
                // The type's usual port, unless the user set another; a
                // sign-in the new type lacks is dropped, and so are typed
                // secrets, so one type's key never reaches another's field.
                const port = (v.port ?? 0) === defaultPort(v.type) ? defaultPort(type) : v.port;
                set({ ...noSecrets, type, tier: tierFor(type), port, auth: authsFor(type).includes(v.auth as never) ? v.auth : authsFor(type)[0] ?? "" });
              }}
            >
              {VPN_TYPES.map((ty) => (
                <option key={ty.id} value={ty.id}>
                  {t(ty.label)}
                </option>
              ))}
              {!VPN_TYPES.some((ty) => ty.id === v.type) && <option value={v.type}>{v.type}</option>}
            </Select>
          </Field>
        </div>
        <Field label={t("vpnForm.tags")} hint={t("vpnForm.tagsHint")}>
          <TagEditor tags={v.tags ?? []} onChange={(tags) => set({ tags })} />
        </Field>

        {!external && (
          <div className="grid grid-cols-1 sm:grid-cols-[1fr_7rem] gap-3">
            <Field label={t(v.type === "tailscale" ? "vpnForm.controlServer" : "vpnForm.gateway")} error={err("gateway")}>
              <Input
                className="w-full"
                spellCheck={false}
                value={v.gateway ?? ""}
                aria-invalid={!!err("gateway")}
                onChange={(e) => set({ gateway: e.target.value })}
              />
            </Field>
            {needsGateway(v.type) && (
              <Field label={t("vpnForm.port")} error={err("port")}>
                <Input
                  className="w-full"
                  type="number"
                  min={1}
                  max={65535}
                  value={v.port || ""}
                  aria-invalid={!!err("port")}
                  onChange={(e) => set({ port: e.target.value.trim() === "" ? 0 : Number(e.target.value) })}
                />
              </Field>
            )}
          </div>
        )}
        {external && (
          <Field label={t("vpnForm.client")} hint={t("vpnForm.clientHint")}>
            <Input className="w-full" spellCheck={false} value={v.client ?? ""} onChange={(e) => set({ client: e.target.value })} />
          </Field>
        )}

        {wg && (
          <>
            {list(t("vpnForm.address"), "address", "10.8.0.2/32")}
            <Field label={t("vpnForm.peerKey")} error={err("peerKey")}>
              <Input className="w-full font-mono" spellCheck={false} value={v.peerKey ?? ""} aria-invalid={!!err("peerKey")} onChange={(e) => set({ peerKey: e.target.value })} />
            </Field>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              {secret(t("vpnForm.privateKey"), "privateKey", err("privateKey"))}
              {secret(t("vpnForm.presharedKey"), "presharedKey")}
            </div>
          </>
        )}
        {v.type === "tailscale" && secret(t("vpnForm.authKey"), "authKey")}
        {auths.length > 0 && (
          <>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <Field label={t("vpnForm.auth")}>
                <Select className="w-full" value={auth} onChange={(e) => set({ auth: e.target.value })}>
                  {auths.map((a) => (
                    <option key={a} value={a}>
                      {t(AUTH_LABEL[a])}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label={t("vpnForm.user")} error={err("user")}>
                <Input className="w-full" spellCheck={false} autoComplete="off" value={v.user ?? ""} onChange={(e) => set({ user: e.target.value })} />
              </Field>
            </div>
            {auth === "certificate" ? secret(t("vpnForm.certKey"), "privateKey") : auth !== "browser" && secret(t("vpnForm.password"), "password")}
            {auth === "otp" && <p className="text-xs text-muted">{t("vpnForm.otpHint")}</p>}
            {auth === "browser" && <p className="text-xs text-muted">{t("vpnForm.browserHint")}</p>}
            {v.type === "ipsec" && secret(t("vpnForm.presharedKey"), "presharedKey")}
          </>
        )}
        {vpnAsksSecretAgain(start, v) && <p className="text-xs text-warning">{t("vpnForm.secretAgain")}</p>}

        {!external && v.type !== "tailscale" && v.type !== "zerotier" && (
          <section className="flex flex-col gap-3 pt-2 border-t border-line">
            <h3 className="text-sm font-semibold">{t("vpnForm.routing")}</h3>
            <SegmentedControl
              label={t("vpnForm.tunnel")}
              value={full ? "full" : "split"}
              options={[
                { value: "split", label: t("vpnForm.split") },
                { value: "full", label: t("vpnForm.full") },
              ]}
              onChange={(m) => set({ routes: m === "full" ? [...FULL_TUNNEL] : (v.routes ?? []).filter((r) => !FULL_TUNNEL.includes(r)) })}
            />
            {full ? <p className="text-xs text-muted">{t("vpnForm.fullHint")}</p> : list(t("vpnForm.routes"), "routes", "10.0.0.0/8", t("vpnForm.routesHint"))}
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              {list(t("vpnForm.dnsServers"), "dnsServers", "10.0.0.53")}
              {list(t("vpnForm.dnsDomains"), "dnsDomains", "corp.example")}
            </div>
            <Field label={t("vpnForm.mtu")} hint={t("vpnForm.mtuHint")} error={err("mtu")}>
              <Input
                className="w-32"
                type="number"
                min={576}
                max={65535}
                placeholder="1280"
                value={v.mtu || ""}
                aria-invalid={!!err("mtu")}
                onChange={(e) => set({ mtu: e.target.value.trim() === "" ? 0 : Number(e.target.value) })}
              />
            </Field>
          </section>
        )}
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={!!v.autoReconnect} onChange={(e) => set({ autoReconnect: e.target.checked })} />
          {t("vpnForm.autoReconnect")}
        </label>
        {!external && (
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={!!v.autoApply} onChange={(e) => set({ autoApply: e.target.checked })} />
            {t("vpnForm.autoApply")}
          </label>
        )}
      </form>
    </Dialog>
  );
}
