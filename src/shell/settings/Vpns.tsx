import { useEffect, useState, type ReactNode } from "react";
import * as api from "../../api";
import type { Vpn } from "../../api";
import { reloadVpns, useVpns } from "../../state/vpns";
import { TagEditor } from "../../components/TagEditor";
import { VpnImport } from "../../components/VpnImport";
import { openApp } from "../../dialogs";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { AddIcon, CloseIcon, DeleteIcon, EditIcon, ImportIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { SegmentedControl } from "../../ui/SegmentedControl";
import { Select } from "../../ui/Select";
import { useAsyncToast, useToast } from "../../ui/toast";
import {
  AUTH_LABEL,
  FULL_TUNNEL,
  VPN_TYPES,
  authsFor,
  defaultPort,
  hasRouting,
  isFullTunnel,
  needsGateway,
  pickAuth,
  tierFor,
  typeLabel,
  validateVpn,
  vpnAsksSecretAgain,
  type VpnAuth,
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
  const [importing, setImporting] = useState(false);
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
      <TunnelHubBanner />
      <div className="flex gap-2">
        <Button className="w-fit" icon={AddIcon} onClick={() => setEditing({ ...NEW_VPN })}>
          {t("vpn.new")}
        </Button>
        <Button className="w-fit" icon={ImportIcon} onClick={() => setImporting(true)}>
          {t("vpn.import")}
        </Button>
      </div>
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
      {importing && <VpnImport onClose={() => setImporting(false)} />}
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

const TUNNELHUB_DISMISSED = "tunnelHubBannerDismissed";
const tunnelHubDismissed = () => {
  try {
    return localStorage.getItem(TUNNELHUB_DISMISSED) === "1";
  } catch {
    return false;
  }
};

/** A one-time offer to move off TunnelHub (R45): import its config (R41),
 *  then remove its helper and agent after Go's dialog. Dismissing it is
 *  remembered on this device. */
function TunnelHubBanner() {
  const run = useAsyncToast();
  const { push } = useToast();
  const [hidden, setHidden] = useState(tunnelHubDismissed);
  const [found, setFound] = useState<api.TunnelHubFound | null>(null);
  const [importing, setImporting] = useState(false);
  const [busy, setBusy] = useState(false);
  const detect = () => api.detectTunnelHub().then(setFound, () => setFound(null));
  useEffect(() => {
    if (!hidden) detect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hidden]);
  if (hidden || !found || (!found.configPath && found.items.length === 0)) return null;

  const remove = async () => {
    setBusy(true);
    const r = await run(api.removeTunnelHub);
    setBusy(false);
    if (!r) return;
    if (r.status === "cancelled") push("info", t("tunnelHub.cancelled"));
    if (r.removed.length) push("success", t("tunnelHub.removed", { items: r.removed.join(", ") }));
    if (r.failed.length) push("error", t("tunnelHub.failed", { items: r.failed.join(", "), error: r.error ?? "" }));
    detect();
  };
  const dismiss = () => {
    try {
      localStorage.setItem(TUNNELHUB_DISMISSED, "1");
    } catch {
      // storage refused: hidden for this session only
    }
    setHidden(true);
  };

  return (
    <section aria-label={t("tunnelHub.title")} className="max-w-xl rounded-control border border-warning/60 bg-warning/10 p-3 text-sm flex flex-col gap-2">
      <div className="flex items-start gap-2">
        <p className="flex-1">
          <span className="font-medium">{t("tunnelHub.title")}</span> <span className="text-muted">{t("tunnelHub.hint")}</span>
        </p>
        <IconButton label={t("tunnelHub.dismiss")} icon={CloseIcon} onClick={dismiss} />
      </div>
      <ol className="flex flex-col gap-2 list-decimal pl-5">
        {found.configPath && (
          <li>
            <Button size="sm" onClick={() => setImporting(true)}>
              {t("tunnelHub.import")}
            </Button>
          </li>
        )}
        {found.items.length > 0 && (
          <li>
            <Button size="sm" disabled={busy} onClick={remove}>
              {t("tunnelHub.remove")}
            </Button>
          </li>
        )}
      </ol>
      {found.appPath && <p className="text-xs text-muted">{t("tunnelHub.app", { path: found.appPath })}</p>}
      {importing && found.configPath && <VpnImport path={found.configPath} onClose={() => setImporting(false)} />}
    </section>
  );
}

/** Full or split tunnel, the routes and DNS (R42), for the profile form
 *  and the import review. */
export function RouteEditor({ v, set, err }: { v: Vpn; set: (patch: Partial<Vpn>) => void; err: (f: VpnField) => string | undefined }) {
  const full = isFullTunnel(v.routes);
  const list = (label: string, field: "routes" | "dnsServers" | "dnsDomains", placeholder: string, hint?: ReactNode) => (
    <Field label={label} hint={hint} error={err(field)}>
      <TagEditor tags={v[field] ?? []} placeholder={placeholder} invalid={!!err(field)} onChange={(x) => set({ [field]: x })} />
    </Field>
  );
  return (
    <>
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
    </>
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
  const [groups, setGroups] = useState<string[]>([]);
  const [fetchingGroups, setFetchingGroups] = useState(false);
  const [importing, setImporting] = useState(false);
  const { vpns: saved } = useVpns();
  const errors = validateVpn(v);
  const err = (f: VpnField) => (tried && errors[f] ? t(errors[f]!) : undefined);
  const dirty = JSON.stringify(v) !== JSON.stringify(start);
  // An external profile's own app holds its keys and sign-in.
  const external = v.tier === "external";
  const wg = v.type === "wireguard" && !external;
  const auths = external ? [] : authsFor(v.type);
  // A sign-in the type no longer offers (an imported certificate one) shows as its first.
  const auth = external ? "" : pickAuth(v.type, v.auth);
  const keep = v.hasSecret ? t("hostForm.passwordKeep") : "";
  // OpenVPN needs the server's certificate, which only a .ovpn file carries.
  const ovpn = v.type === "openvpn" && !external;
  const ovpnNew = ovpn && !v.id;
  // Two profiles for one client cannot be told apart by name (R24).
  const sharesClient =
    external && !!v.client && !v.iface && saved.some((o) => o.id !== v.id && o.tier === "external" && o.client?.toLowerCase() === v.client?.toLowerCase());
  const chooseApp = async () => {
    const path = await openApp().catch(() => null);
    if (!path) return;
    // The name the OS gives the VPN stays as typed; an empty one takes the app's.
    const name = path.split(/[\\/]/).pop()?.replace(/\.(app|exe)$/i, "") ?? "";
    set({ clientPath: path, client: v.client?.trim() ? v.client : name });
  };

  const save = async () => {
    if (ovpnNew) return; // Enter in the name field submits too; the button is only disabled
    setTried(true);
    if (Object.keys(errors).length) return;
    setBusy(true);
    const ok = await run(() => api.saveVpn({ ...v, name: v.name.trim(), gateway: v.gateway?.trim(), auth }), t("vpnForm.saved"));
    setBusy(false);
    if (ok !== undefined) onSaved();
  };

  const secret = (label: string, field: "password" | "privateKey" | "presharedKey" | "authKey", error?: string, hint?: string) => (
    <Field label={label} error={error} hint={hint}>
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
          <Button variant="primary" type="submit" form="vpn-form" disabled={busy || ovpnNew}>
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
          <Field label={t("vpnForm.type")} hint={tierFor(v.type) === "system" ? t("vpnForm.typeLater") : undefined}>
            <Select
              className="w-full"
              value={v.type}
              onChange={(e) => {
                const type = e.target.value;
                // The type's usual port, unless the user set another; a
                // sign-in the new type lacks is dropped, and so are typed
                // secrets, so one type's key never reaches another's field.
                const port = (v.port ?? 0) === defaultPort(v.type) ? defaultPort(type) : v.port;
                set({ ...noSecrets, type, tier: tierFor(type), port, auth: pickAuth(type, v.auth) });
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

        {ovpn && (
          <div className="rounded-control border border-line bg-hover p-3 text-sm flex flex-col gap-2">
            {ovpnNew && <p className="font-medium">{t("vpnForm.ovpnImportTitle")}</p>}
            <p className="text-muted">{t(ovpnNew ? "vpnForm.ovpnImportHint" : "vpnForm.ovpnReimportHint")}</p>
            <Button type="button" className="w-fit" variant={ovpnNew ? "primary" : "default"} icon={ImportIcon} onClick={() => setImporting(true)}>
              {t("vpnForm.ovpnImportButton")}
            </Button>
          </div>
        )}
        {!external && !ovpnNew && (
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
          <>
            <Field label={t("vpnForm.client")} hint={t("vpnForm.clientHint")}>
              <Input className="w-full" spellCheck={false} value={v.client ?? ""} onChange={(e) => set({ client: e.target.value })} />
            </Field>
            {list(t("vpnForm.address"), "address", "10.255.0.0/16", t("vpnForm.addressExternalHint"))}
            <div className="grid grid-cols-1 sm:grid-cols-[1fr_10rem] gap-3 items-start">
              <Field label={t("vpnForm.clientPath")} hint={t("vpnForm.clientPathHint")}>
                <div className="flex gap-2">
                  <Input className="w-full" spellCheck={false} value={v.clientPath ?? ""} onChange={(e) => set({ clientPath: e.target.value })} />
                  <Button type="button" onClick={chooseApp}>
                    {t("vpnForm.chooseApp")}
                  </Button>
                </div>
              </Field>
              <Field label={t("vpnForm.iface")} hint={t("vpnForm.ifaceHint")}>
                <Input className="w-full font-mono" spellCheck={false} value={v.iface ?? ""} onChange={(e) => set({ iface: e.target.value.trim() })} />
              </Field>
            </div>
            {sharesClient && <p className="text-xs text-warning">{t("vpnForm.clientSharedWarning")}</p>}
          </>
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
        {v.type === "tailscale" && (
          <>
            {secret(t("vpnForm.authKey"), "authKey", undefined, t("vpnForm.authKeyHint"))}
            {initial.id && initial.type === "tailscale" && (
              <Button
                type="button"
                className="w-fit"
                size="sm"
                onClick={() => run(() => api.signInAgainVpn(initial.id), t("vpnForm.signedOut"))}
              >
                {t("vpnForm.signInAgain")}
              </Button>
            )}
          </>
        )}
        {auths.length > 0 && !ovpnNew && (
          <>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <Field label={t("vpnForm.auth")}>
                <Select className="w-full" value={auth} onChange={(e) => set({ auth: e.target.value as VpnAuth })}>
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
            {v.type === "anyconnect" && (
              <Field label={t("vpnForm.authGroup")} error={err("authGroup")}>
                <div className="flex gap-2">
                  {groups.length > 0 ? (
                    <Select className="w-full" value={v.authGroup ?? ""} onChange={(e) => set({ authGroup: e.target.value })}>
                      <option value="">{t("vpnForm.authGroupDefault")}</option>
                      {(v.authGroup && !groups.includes(v.authGroup) ? [v.authGroup, ...groups] : groups).map((g) => (
                        <option key={g} value={g}>
                          {g}
                        </option>
                      ))}
                    </Select>
                  ) : (
                    <Input
                      className="w-full"
                      spellCheck={false}
                      autoComplete="off"
                      placeholder={t("vpnForm.authGroupPlaceholder")}
                      value={v.authGroup ?? ""}
                      onChange={(e) => set({ authGroup: e.target.value })}
                    />
                  )}
                  <Button
                    type="button"
                    size="sm"
                    disabled={fetchingGroups || !(v.gateway ?? "").trim()}
                    onClick={async () => {
                      setFetchingGroups(true);
                      const g = await run(() => api.vpnGroups((v.gateway ?? "").trim(), v.port ?? 0));
                      setFetchingGroups(false);
                      if (g) setGroups(g);
                    }}
                  >
                    {fetchingGroups ? t("vpnForm.authGroupFetching") : t("vpnForm.authGroupFetch")}
                  </Button>
                </div>
                <p className="text-xs text-muted mt-1">{t("vpnForm.authGroupHint")}</p>
              </Field>
            )}
            {auth === "certificate" ? secret(t("vpnForm.certKey"), "privateKey") : auth !== "browser" && secret(t("vpnForm.password"), "password")}
            {auth === "otp" && <p className="text-xs text-muted">{t("vpnForm.otpHint")}</p>}
            {auth === "browser" && <p className="text-xs text-muted">{t("vpnForm.browserHint")}</p>}
            {v.type === "ipsec" && secret(t("vpnForm.presharedKey"), "presharedKey")}
          </>
        )}
        {vpnAsksSecretAgain(start, v) && <p className="text-xs text-warning">{t("vpnForm.secretAgain")}</p>}

        {/* An external profile's routes label its tunnel, and its DNS servers resolve names over it (R24, KTD7). */}
        {hasRouting(v.type) && (
          <section className="flex flex-col gap-3 pt-2 border-t border-line">
            <h3 className="text-sm font-semibold">{t("vpnForm.routing")}</h3>
            <RouteEditor v={v} set={set} err={err} />
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
      {importing && (
        <VpnImport
          onClose={() => setImporting(false)}
          // A blank new profile has nothing left to do once its import saved; cancelling keeps the form.
          onSaved={() => {
            if (!v.id) onClose();
          }}
        />
      )}
    </Dialog>
  );
}
