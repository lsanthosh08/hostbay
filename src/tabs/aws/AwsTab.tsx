import { Browser } from "@wailsio/runtime";
import { useCallback, useEffect, useRef, useState } from "react";
import * as api from "../../api";
import type { AppError, AwsDatabase, AwsIdentity, AwsInstance, AwsInstanceAction, AwsProfile, AwsTools } from "../../api";
import { t } from "../../i18n";
import { useSessions } from "../../state/sessions";
import { Button } from "../../ui/Button";
import { EmptyState } from "../../ui/EmptyState";
import { Input } from "../../ui/Field";
import { AwsIcon } from "../../ui/icons";
import { Spinner } from "../../ui/kit";
import { usePrompt } from "../../ui/prompt";
import { SegmentedControl } from "../../ui/SegmentedControl";
import { Select } from "../../ui/Select";
import { useToast } from "../../ui/toast";
import { useHostManager } from "../../components/HostManager";
import { databaseDraft, instanceActions, instanceDraft, matches, signInNeeded } from "./aws";

const CLI_INSTALL = "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html";
const PLUGIN_INSTALL = "https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html";

const ACTION_LABEL = { start: "aws.start", stop: "aws.stop", reboot: "aws.reboot" } as const;

const th = "text-left font-medium px-3 py-2 opacity-70";
const td = "px-3 py-2 border-t border-line align-top";

/**
 * The AWS tab: the profiles in this computer's AWS config, and for one
 * profile and region its account, EC2 instances and RDS databases. An
 * instance opens an SSM shell, and starts, stops or reboots after a
 * confirmation naming the account, region and instance. Credentials stay
 * in Go; the backend holds no session, so every call names the profile
 * and region.
 */
export function AwsTab() {
  const { openAwsTerm } = useSessions();
  const { newHostFrom } = useHostManager();
  const { confirm } = usePrompt();
  const { push } = useToast();
  const [profiles, setProfiles] = useState<AwsProfile[] | null>(null);
  const [tools, setTools] = useState<AwsTools>({ cli: true, plugin: true });
  const [profile, setProfile] = useState("");
  const [region, setRegion] = useState("");
  const [regions, setRegions] = useState<api.AwsRegionList>({ regions: [], checked: true });
  const [identity, setIdentity] = useState<AwsIdentity | null>(null);
  const [error, setError] = useState<AppError | null>(null);
  const [view, setView] = useState<"ec2" | "rds">("ec2");
  const [filter, setFilter] = useState("");
  const [instances, setInstances] = useState<AwsInstance[]>([]);
  const [databases, setDatabases] = useState<AwsDatabase[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState("");
  // Bumped per load; an answer that is no longer the latest is dropped, so
  // a slow list for the profile or region just left never replaces this one.
  const seq = useRef(0);

  useEffect(() => {
    api.awsTools().then(setTools, () => {});
    api.awsProfiles().then(
      (ps) => {
        setProfiles(ps);
        if (ps[0]) pick(ps[0]);
      },
      (e) => {
        setProfiles([]);
        setError(api.errorInfo(e));
      }
    );
  }, []);

  const pick = (p: AwsProfile) => {
    setProfile(p.name);
    setRegion(p.region || "us-east-1");
  };

  const load = useCallback(async () => {
    if (!profile || !region) return;
    const mine = ++seq.current;
    setLoading(true);
    setError(null);
    try {
      const id = await api.awsAccount(profile, region);
      if (mine !== seq.current) return;
      setIdentity(id);
      const [rs, is, dbs] = await Promise.all([
        api.awsRegions(profile, region),
        view === "ec2" ? api.awsInstances(profile, region) : Promise.resolve(null),
        view === "rds" ? api.awsDatabases(profile, region) : Promise.resolve(null),
      ]);
      if (mine !== seq.current) return;
      setRegions(rs);
      if (is) setInstances(is);
      if (dbs) setDatabases(dbs);
    } catch (e) {
      if (mine !== seq.current) return;
      setError(api.errorInfo(e));
      setInstances([]);
      setDatabases([]);
    } finally {
      if (mine === seq.current) setLoading(false);
    }
  }, [profile, region, view]);

  useEffect(() => {
    load();
  }, [load]);

  const signIn = error ? signInNeeded(error) : "";
  const regionChoices = [...new Set([region, ...regions.regions].filter(Boolean))].sort();

  const act = async (i: AwsInstance, action: AwsInstanceAction) => {
    const ok = await confirm(
      t("aws.confirmAction", {
        action: t(ACTION_LABEL[action]),
        instance: i.id,
        name: i.name || i.id,
        account: identity?.account ?? "",
        region,
      }),
      { confirmLabel: t(ACTION_LABEL[action]) }
    );
    if (!ok) return;
    setBusy(i.id);
    try {
      await api.awsInstanceAction(profile, region, i.id, action);
      push("success", t("aws.done"));
      await load();
    } catch (e) {
      push("error", api.errorInfo(e).message);
    } finally {
      setBusy("");
    }
  };

  if (profiles && profiles.length === 0) {
    return (
      <div className="flex flex-col flex-1 h-full min-h-0 overflow-auto">
        <EmptyState icon={AwsIcon} title={t("aws.noProfiles")}>
          {error ? error.message : t("aws.noProfilesBody")}
        </EmptyState>
      </div>
    );
  }

  const shownInstances = instances.filter((i) => matches(i, filter));
  const shownDatabases = databases.filter((d) => matches(d, filter));

  return (
    <div className="flex flex-col flex-1 h-full min-h-0">
      <div className="flex flex-wrap items-center gap-2 px-3 py-2 border-b border-line">
        <Select
          aria-label={t("aws.profile")}
          value={profile}
          onChange={(e) => {
            const p = profiles?.find((x) => x.name === e.target.value);
            if (p) pick(p);
          }}
        >
          {(profiles ?? []).map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}
            </option>
          ))}
        </Select>
        <Select aria-label={t("aws.region")} value={region} onChange={(e) => setRegion(e.target.value)}>
          {regionChoices.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </Select>
        <SegmentedControl
          label={t("aws.resources")}
          value={view}
          onChange={setView}
          options={[
            { value: "ec2", label: t("aws.ec2") },
            { value: "rds", label: t("aws.rds") },
          ]}
        />
        <Input placeholder={t("aws.filter")} aria-label={t("aws.filter")} value={filter} onChange={(e) => setFilter(e.target.value)} className="w-40" />
        <Button size="sm" onClick={() => load()} disabled={loading}>
          {t("aws.refresh")}
        </Button>
        {identity && (
          <span className="text-xs text-muted ml-auto allow-select break-all">{t("aws.account", { account: identity.account, arn: identity.arn })}</span>
        )}
      </div>
      <div role="status" aria-live="polite" className="flex flex-wrap items-center gap-2 px-3 text-xs min-h-6">
        {loading && <Spinner />}
        {error && <span className="text-danger allow-select break-words">{error.message}</span>}
        {error?.details?.expiresAt && <span className="text-muted">{t("aws.expiredAt", { time: new Date(error.details.expiresAt).toLocaleString() })}</span>}
        {signIn && (
          <Button
            size="sm"
            variant="primary"
            disabled={!tools.cli}
            onClick={() => openAwsTerm(t("aws.signInTab", { profile: signIn }), { kind: "aws", action: "login", profile })}
          >
            {t("aws.signIn")}
          </Button>
        )}
        {!regions.checked && <span className="text-muted">{t("aws.regionsUnchecked")}</span>}
        {!tools.cli && (
          <span className="text-warning">
            {t("aws.cliMissing")}{" "}
            <button className="underline" onClick={() => Browser.OpenURL(CLI_INSTALL)}>
              {t("aws.install")}
            </button>
          </span>
        )}
        {tools.cli && !tools.plugin && view === "ec2" && (
          <span className="text-warning">
            {t("aws.pluginMissing")}{" "}
            <button className="underline" onClick={() => Browser.OpenURL(PLUGIN_INSTALL)}>
              {t("aws.install")}
            </button>
          </span>
        )}
      </div>
      <div className="flex-1 min-h-0 overflow-auto">
        {!error && !loading && view === "ec2" && instances.length === 0 && <EmptyState>{t("aws.noInstances", { region })}</EmptyState>}
        {!error && !loading && view === "rds" && databases.length === 0 && <EmptyState>{t("aws.noDatabases", { region })}</EmptyState>}
        {filter.trim() && (view === "ec2" ? instances.length > 0 && !shownInstances.length : databases.length > 0 && !shownDatabases.length) && (
          <p className="text-sm text-muted p-4">{t("aws.noMatch")}</p>
        )}
        {view === "ec2" && shownInstances.length > 0 && (
          <table className="w-full text-sm">
            <thead>
              <tr>
                <th className={th}>{t("aws.name")}</th>
                <th className={th}>{t("aws.state")}</th>
                <th className={th}>{t("aws.type")}</th>
                <th className={th}>{t("aws.address")}</th>
                <th className={th}>{t("aws.keyPair")}</th>
                <th className={th}>{t("aws.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {shownInstances.map((i) => (
                <tr key={i.id}>
                  <td className={`${td} allow-select break-all`}>
                    {i.name || i.id}
                    {i.name && <div className="text-xs text-muted font-mono">{i.id}</div>}
                  </td>
                  <td className={td}>{i.state}</td>
                  <td className={td}>{i.type}</td>
                  <td className={`${td} font-mono allow-select`}>
                    {i.publicIp && <div>{i.publicIp}</div>}
                    {i.privateIp && <div className="text-muted">{i.privateIp}</div>}
                  </td>
                  <td className={td}>{i.keyName}</td>
                  <td className={td}>
                    <div className="flex flex-wrap gap-1">
                      {busy === i.id && <Spinner />}
                      {instanceActions(i.state).map((a) =>
                        a === "shell" ? (
                          <Button
                            key={a}
                            size="sm"
                            disabled={!tools.cli || !!busy}
                            onClick={() =>
                              openAwsTerm(t("aws.shellTab", { name: i.name || i.id }), { kind: "aws", action: "shell", profile, region, instanceId: i.id })
                            }
                          >
                            {t("aws.shell")}
                          </Button>
                        ) : (
                          <Button key={a} size="sm" variant={a === "start" ? "default" : "ghost"} disabled={!!busy} onClick={() => act(i, a)}>
                            {t(ACTION_LABEL[a])}
                          </Button>
                        )
                      )}
                      <Button size="sm" variant="ghost" onClick={() => newHostFrom(instanceDraft(i))}>
                        {t(i.platform === "windows" ? "aws.saveRdp" : "aws.saveSsh")}
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {view === "rds" && shownDatabases.length > 0 && (
          <table className="w-full text-sm">
            <thead>
              <tr>
                <th className={th}>{t("aws.name")}</th>
                <th className={th}>{t("aws.engine")}</th>
                <th className={th}>{t("aws.state")}</th>
                <th className={th}>{t("aws.endpoint")}</th>
                <th className={th}>{t("aws.user")}</th>
                <th className={th}>{t("aws.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {shownDatabases.map((d) => {
                const draft = databaseDraft(d);
                return (
                  <tr key={d.id}>
                    <td className={`${td} allow-select break-all`}>{d.id}</td>
                    <td className={td}>{d.engine}</td>
                    <td className={td}>{d.status}</td>
                    <td className={`${td} font-mono allow-select break-all`}>{d.address && `${d.address}:${d.port}`}</td>
                    <td className={td}>{d.user}</td>
                    <td className={td}>
                      {draft && (
                        <Button size="sm" variant="ghost" onClick={() => newHostFrom(draft)}>
                          {t("aws.saveDb")}
                        </Button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
