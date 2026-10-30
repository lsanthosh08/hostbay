import { useEffect, useState } from "react";
import * as api from "../api";
import type { BrowserMapping, InstalledBrowser, Vpn } from "../api";
import { t } from "../i18n";
import { Button } from "../ui/Button";
import { IconButton } from "../ui/IconButton";
import { DeleteIcon, StartIcon } from "../ui/icons";
import { Select } from "../ui/Select";
import { useToast } from "../ui/toast";

/** The mapped browser profiles, each launched through its VPN, and a row to
 *  map another: an installed browser, one of its profiles and an app-scoped
 *  VPN (R33). Without data source, so tests can render it. */
export function BrowserMappingsView(p: {
  browsers: InstalledBrowser[];
  mappings: BrowserMapping[];
  vpns: Vpn[];
  onSave: (m: BrowserMapping) => void;
  onRemove: (m: BrowserMapping) => void;
  onLaunch: (m: BrowserMapping) => void;
}) {
  const appVpns = p.vpns.filter((v) => v.tier === "app");
  const [draft, setDraft] = useState<BrowserMapping>({ browser: "", profile: "", vpn: "" });
  const browser = p.browsers.find((b) => b.id === draft.browser) ?? p.browsers[0];
  const profiles = browser?.profiles ?? [];
  const pick = { browser: browser?.id ?? "", profile: draft.profile || profiles[0]?.dir || "", vpn: draft.vpn || appVpns[0]?.id || "" };
  const label = (m: BrowserMapping) => {
    const b = p.browsers.find((x) => x.id === m.browser);
    return {
      browser: b?.name ?? m.browser,
      profile: b?.profiles?.find((x) => x.dir === m.profile)?.name || m.profile,
      vpn: p.vpns.find((v) => v.id === m.vpn),
    };
  };
  return (
    <section className="px-2 pb-2 border-t border-line" aria-label={t("vpn.browsers")}>
      <h3 className="px-2 pt-2 text-xs font-semibold text-muted">{t("vpn.browsers")}</h3>
      <p className="px-2 text-xs text-muted">{t("vpn.browsersHint")}</p>
      <ul>
        {p.mappings.map((m) => {
          const l = label(m);
          const vpn = l.vpn?.name ?? t("vpn.browserMissingVpn");
          return (
            <li key={m.browser + "/" + m.profile} className="flex flex-col px-2 py-1 text-sm">
              <div className="flex items-center gap-2">
                <span className="truncate flex-1">{t("vpn.browserMapping", { browser: l.browser, profile: l.profile, vpn })}</span>
                <IconButton size="sm" label={t("vpn.browserLaunch", { profile: l.profile, vpn })} icon={StartIcon} onClick={() => p.onLaunch(m)} />
                <IconButton size="sm" label={t("vpn.browserRemove", { profile: l.profile })} icon={DeleteIcon} onClick={() => p.onRemove(m)} />
              </div>
              {l.vpn && l.vpn.tier !== "app" && <span className="text-xs text-warning">{t("vpn.browserNoSocks", { vpn })}</span>}
            </li>
          );
        })}
      </ul>
      {p.browsers.length === 0 ? (
        <p className="px-2 text-xs text-muted">{t("vpn.browserNone")}</p>
      ) : (
        <div className="flex flex-wrap items-center gap-1 px-2 pt-1">
          <Select compact aria-label={t("vpn.browserPick")} value={pick.browser} onChange={(e) => setDraft({ ...draft, browser: e.target.value, profile: "" })}>
            {p.browsers.map((b) => (
              <option key={b.id} value={b.id}>
                {b.name}
              </option>
            ))}
          </Select>
          <Select compact aria-label={t("vpn.browserProfile")} value={pick.profile} onChange={(e) => setDraft({ ...draft, browser: pick.browser, profile: e.target.value })}>
            {profiles.map((x) => (
              <option key={x.dir} value={x.dir}>
                {x.name || x.dir}
              </option>
            ))}
          </Select>
          <Select compact aria-label={t("vpn.browserVpn")} value={pick.vpn} onChange={(e) => setDraft({ ...draft, vpn: e.target.value })}>
            {appVpns.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </Select>
          <Button size="sm" disabled={!pick.browser || !pick.profile || !pick.vpn} onClick={() => p.onSave(pick)}>
            {t("vpn.browserAdd")}
          </Button>
        </div>
      )}
    </section>
  );
}

/** BrowserMappingsView with its data: the installed browsers, read once,
 *  and the saved mappings. */
export function BrowserMappings({ vpns }: { vpns: Vpn[] }) {
  const { push } = useToast();
  const [browsers, setBrowsers] = useState<InstalledBrowser[]>([]);
  const [mappings, setMappings] = useState<BrowserMapping[]>([]);
  const fail = (e: unknown) => push("error", api.errorInfo(e).message);
  const reload = () => api.browserMappings().then(setMappings, fail);
  useEffect(() => {
    api.installedBrowsers().then(setBrowsers, () => setBrowsers([]));
  }, []);
  // An import saves mappings with its profiles, and reloads the profiles.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => void reload(), [vpns]);
  return (
    <BrowserMappingsView
      browsers={browsers}
      mappings={mappings}
      vpns={vpns}
      onSave={(m) => api.saveBrowserMapping(m).then(reload, fail)}
      onRemove={(m) => api.deleteBrowserMapping(m.browser, m.profile).then(reload, fail)}
      onLaunch={(m) => api.launchBrowser(m.browser, m.profile).catch(fail)}
    />
  );
}
