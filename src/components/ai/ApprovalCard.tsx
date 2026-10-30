import { useEffect, useState } from "react";
import type { AIPending } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";

/** An MCP connect_vpn or disconnect_vpn request carries these too; Go
 *  refuses a profile name, gateway or tag a card could not show as it is. */
export type VPNPending = AIPending & { tag?: string; tier?: string; gateway?: string };

const secondsLeft = (expires: string) => Math.max(0, Math.round((Date.parse(expires) - Date.now()) / 1000));

/** One request to run a command (KTD32): the host Go derived from the
 *  terminal or, for an MCP request, the client's token name and the saved
 *  host, the exact bytes in full and their size. A save_skill request
 *  shows the skill's full name and body instead, and whether it replaces
 *  one (KTD33). A VPN request names the client, the profile, the tag it
 *  was picked by, what it reaches (the whole computer for a system-wide
 *  profile) and the gateway, and has no bytes (KTD6). Allow and Deny are
 *  plain buttons; neither is the default and neither takes focus. */
export function ApprovalCard({ p, onAnswer }: { p: VPNPending; onAnswer: (allow: boolean) => void }) {
  const [left, setLeft] = useState(() => secondsLeft(p.expires));
  useEffect(() => {
    const id = setInterval(() => setLeft(secondsLeft(p.expires)), 1000);
    return () => clearInterval(id);
  }, [p.expires]);
  const skill = p.tool === "save_skill";
  const vpn = p.tool === "connect_vpn" || p.tool === "disconnect_vpn";
  const label = skill
    ? t(p.replaces ? "ai.card.skillReplace" : "ai.card.skill", { name: `/${p.name ?? ""}` })
    : vpn
      ? t(p.tool === "connect_vpn" ? "mcp.card.vpnConnect" : "mcp.card.vpnDisconnect", { client: p.client ?? "", name: p.host })
      : p.client
      ? t("mcp.card.label", { client: p.client, host: p.host })
      : t("ai.card.label", { host: p.host });

  return (
    <article aria-label={label} className="panel rounded-card shadow-lg p-3 flex flex-col gap-2 text-sm">
      <header className="flex items-baseline gap-2">
        <h3 className="font-medium flex-1 min-w-0 break-words">{label}</h3>
        <span className="text-xs text-muted tabular-nums shrink-0">{t("ai.card.left", { n: left })}</span>
      </header>
      {vpn ? (
        <ul className="flex flex-col gap-0.5">
          {p.tag && <li>{t("mcp.card.vpnTag", { tag: p.tag })}</li>}
          <li className={p.tier === "system" ? "text-danger font-medium" : undefined}>
            {t(p.tier === "system" ? "mcp.card.vpnTier.system" : p.tier === "external" ? "mcp.card.vpnTier.external" : "mcp.card.vpnTier.app")}
          </li>
          {p.gateway && <li className="break-all allow-select">{t("mcp.card.vpnGateway", { gateway: p.gateway })}</li>}
        </ul>
      ) : (
        <>
          <pre
            tabIndex={0}
            aria-label={t(skill ? "ai.card.skillBody" : "ai.card.bytes")}
            className="max-h-48 overflow-auto p-2 rounded-control bg-raised font-mono text-xs whitespace-pre-wrap break-all allow-select"
          >
            {p.bytes}
          </pre>
          <p className="text-xs text-muted">{t("ai.card.size", { n: p.size })}</p>
        </>
      )}
      <div className="flex gap-2 justify-end">
        <Button type="button" variant="ghost" size="sm" onClick={() => onAnswer(false)}>
          {t("ai.card.deny")}
        </Button>
        <Button type="button" size="sm" onClick={() => onAnswer(true)}>
          {t("ai.card.allow")}
        </Button>
      </div>
    </article>
  );
}
