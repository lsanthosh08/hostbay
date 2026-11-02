import type { Host } from "../api";
import { t, type MessageKey } from "../i18n";
import { useVpns } from "../state/vpns";
import { Field } from "../ui/Field";
import { Select } from "../ui/Select";
import { missingVpn } from "./hostForm";

/** The host editor's "Connect via VPN" picker. A VPN the host names but the
 *  list no longer holds stays selectable as "missing". */
export function VpnField({ h, set, error }: { h: Host; set: (patch: Partial<Host>) => void; error?: MessageKey }) {
  const { vpns } = useVpns();
  return (
    <Field
      label={t("hostForm.vpnVia")}
      hint={vpns.find((v) => v.id === h.vpnId)?.tier === "external" ? t("hostForm.vpnExternalHint") : t("hostForm.vpnHint")}
      error={error && t(error)}
    >
      <Select
        className="w-full"
        value={h.vpnId ?? ""}
        aria-invalid={!!error}
        onChange={(e) => set({ vpnId: e.target.value || null })}
      >
        <option value="">{t("hostForm.vpnNone")}</option>
        {vpns.map((v) => (
          <option key={v.id} value={v.id}>
            {v.tier === "external" ? `${v.name} (${t("hostForm.vpnExternal")})` : v.name}
          </option>
        ))}
        {missingVpn(h, vpns) && <option value={h.vpnId!}>{t("hostForm.vpnMissing")}</option>}
      </Select>
    </Field>
  );
}
