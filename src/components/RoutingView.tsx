import { useCallback, useEffect, useRef, useState } from "react";
import * as api from "../api";
import type { GreedyRoute, HelperStatus, PacState, RouteChange, RoutePlan, RoutingReport, RoutingState, Vpn } from "../api";
import { t, type MessageKey } from "../i18n";
import { Group, Row } from "../shell/settings/Group";
import { isFullTunnel } from "../shell/vpnForm";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Field } from "../ui/Field";
import { SegmentedControl } from "../ui/SegmentedControl";
import { Select } from "../ui/Select";
import { usePrompt } from "../ui/prompt";
import { useAsyncToast } from "../ui/toast";
import { TagEditor } from "./TagEditor";
import { reloadVpns, useVpns } from "../state/vpns";

type Mode = "" | "full" | "split";
const MODES: { value: Mode | "per"; label: MessageKey }[] = [
  { value: "per", label: "routing.mode.perDestination" },
  { value: "full", label: "routing.mode.full" },
  { value: "split", label: "routing.mode.split" },
];
const KIND: Record<api.RouteKind, MessageKey> = {
  add: "routing.kind.add",
  delete: "routing.kind.delete",
  narrow: "routing.kind.narrow",
  pin: "routing.kind.pin",
  mtu: "routing.kind.mtu",
};
const HELPER: Partial<Record<api.HelperState, MessageKey>> = {
  missing: "routing.helper.missing",
  repair: "routing.helper.repair",
  stopped: "routing.helper.stopped",
  reinstall: "routing.helper.reinstall",
};
const PAC: Record<api.PacStatus, MessageKey> = {
  off: "routing.pac.off",
  set: "routing.pac.set",
  helper: "routing.pac.helper",
  failed: "routing.pac.failed",
  overridden: "routing.pac.overridden",
};
const INSTALL: Partial<Record<api.HelperState, MessageKey>> = {
  missing: "routing.install",
  stopped: "routing.install",
  repair: "routing.repair",
  reinstall: "routing.reinstall",
};

/** A profile's mode as the plan applies it: full-tunnel routes in the
 *  default mode are a full tunnel. */
const modeOf = (v?: Vpn): Mode => (v?.routingMode as Mode) || (isFullTunnel(v?.routes) ? "full" : "");

/** Profiles that route the whole computer: system-wide and external VPNs. */
const routed = (vpns: Vpn[]) => vpns.filter((v) => v.tier === "system" || v.tier === "external");

/** Why routes cannot apply, or undefined when they can (R30). */
function blockedReason(h: HelperStatus | null) {
  if (!h || h.state === "ready") return undefined;
  if (h.state === "unavailable") return h.reason ?? "";
  return t(HELPER[h.state] ?? "routing.helper.missing");
}

const target = (c: RouteChange) =>
  c.kind === "mtu"
    ? t("routing.mtuOf", { iface: c.iface ?? "", mtu: c.mtu ?? 0 })
    : `${c.dst} ${c.gateway ? t("routing.viaLocal") : t("routing.viaIface", { iface: c.iface ?? "" })}`;

export type RoutingPanelProps = {
  helper: HelperStatus | null;
  vpns: Vpn[];
  profile: string;
  mode: Mode;
  plan: RoutePlan | null;
  /** Why the plan could not be read, with onRetry. */
  planError?: string;
  onRetry?: () => void;
  report: RoutingReport | null;
  state: RoutingState;
  local: string[];
  busy: boolean;
  onProfile: (id: string) => void;
  onMode: (m: Mode) => void;
  onLocal: (dsts: string[]) => void;
  onApply: () => void;
  onReset: () => void;
  onInstall: () => void;
  onNarrow: (g: GreedyRoute) => void;
  onPin: (profile: string, iface: string) => void;
  /** How browsers reach app-scoped VPNs: through Hostbay's system proxy, or why not. */
  pac?: PacState;
};

/** The routing engine's page (U11): mode, planned changes with their
 *  interface and VPN, Apply and its per-change results. */
export function RoutingPanel(p: RoutingPanelProps) {
  const name = (id?: string) => p.vpns.find((v) => v.id === id)?.name ?? "";
  const blocked = blockedReason(p.helper);
  const install = p.helper && INSTALL[p.helper.state];
  const vpns = routed(p.vpns);
  const flagged = Object.entries(p.state.profiles).filter(([, s]) => s !== "applied");
  return (
    <Group title={t("routing.title")} description={t("routing.hint")}>
      {blocked !== undefined && (
        <div className="flex items-center gap-3 text-sm max-w-xl">
          <p className="flex-1 text-muted">{blocked}</p>
          {install && (
            <Button onClick={p.onInstall} disabled={p.busy}>
              {t(install)}
            </Button>
          )}
        </div>
      )}
      {p.pac && (
        <p className={"text-sm max-w-xl " + (p.pac.state === "overridden" || p.pac.state === "failed" ? "text-warning" : "text-muted")}>
          {t(PAC[p.pac.state], { reason: p.pac.reason ?? "" })}
        </p>
      )}
      {vpns.length === 0 && <p className="text-sm text-muted max-w-xl">{t("routing.noProfiles")}</p>}
      {vpns.length > 0 && (
        <div className="flex flex-wrap items-end gap-3">
          <Field label={t("routing.profile")}>
            <Select value={p.profile} onChange={(e) => p.onProfile(e.target.value)}>
              {vpns.map((v) => (
                <option key={v.id} value={v.id}>
                  {v.name}
                </option>
              ))}
            </Select>
          </Field>
          <SegmentedControl
            label={t("routing.mode")}
            value={p.mode || "per"}
            options={MODES.map((m) => ({ value: m.value, label: t(m.label) }))}
            onChange={(m) => p.onMode(m === "per" ? "" : m)}
          />
        </div>
      )}
      <Field label={t("routing.local")} hint={t("routing.localHint")}>
        <TagEditor tags={p.local} placeholder="1.2.3.4" onChange={p.onLocal} />
      </Field>

      {p.planError && (
        <div className="flex items-center gap-3 text-sm max-w-xl">
          <p className="flex-1 text-danger" role="alert">
            {t("routing.planFailed", { error: p.planError })}
          </p>
          {p.onRetry && (
            <Button size="sm" onClick={p.onRetry}>
              {t("vpn.retry")}
            </Button>
          )}
        </div>
      )}
      {p.plan && (
        <>
          <h4 className="text-sm font-medium">{t("routing.planned")}</h4>
          {p.plan.changes.length === 0 ? (
            <p className="text-sm text-muted">{t("routing.none")}</p>
          ) : (
            <ul className="flex flex-col max-w-xl">
              {p.plan.changes.map((c, i) => (
                <Row key={i}>
                  <span className="font-medium">{t(KIND[c.kind])}</span> <span className="allow-select">{target(c)}</span>
                  {c.profile && <span className="text-muted"> · {name(c.profile)}</span>}
                  <span className="block text-xs text-muted">{c.reason}</span>
                </Row>
              ))}
            </ul>
          )}
          {p.plan.skipped.length > 0 && (
            <>
              <h4 className="text-sm font-medium">{t("routing.skipped")}</h4>
              <ul className="flex flex-col max-w-xl">
                {p.plan.skipped.map((s, i) => (
                  <Row key={i}>
                    <span className="allow-select">{s.dst}</span>
                    {s.profile && <span className="text-muted"> · {name(s.profile)}</span>}
                    <span className="block text-xs text-muted">{s.reason}</span>
                  </Row>
                ))}
              </ul>
            </>
          )}
          {p.plan.greedy.map((g) => (
            <div key={g.dst + g.iface} className="flex items-center gap-3 text-sm max-w-xl">
              <p className="flex-1">{t("routing.greedy", { dst: g.dst, iface: g.iface })}</p>
              <Button size="sm" onClick={() => p.onNarrow(g)}>
                {t("routing.narrow")}
              </Button>
            </div>
          ))}
          {p.plan.unlabeled.map((iface) => (
            <div key={iface} className="flex items-end gap-3 text-sm max-w-xl">
              <p className="flex-1">{t("routing.unlabeled", { iface })}</p>
              <Field label={t("routing.pinTo", { iface })}>
                <Select value="" onChange={(e) => e.target.value && p.onPin(e.target.value, iface)}>
                  <option value="" />
                  {vpns.map((v) => (
                    <option key={v.id} value={v.id}>
                      {v.name}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
          ))}
        </>
      )}

      <div className="flex items-center gap-2">
        <Button variant="primary" onClick={p.onApply} disabled={p.busy || blocked !== undefined} title={blocked} aria-busy={p.busy || undefined}>
          {p.busy ? t("routing.applying") : t("routing.apply")}
        </Button>
        <Button onClick={p.onReset} disabled={p.busy || blocked !== undefined} title={blocked}>
          {t("routing.reset")}
        </Button>
      </div>
      {flagged.map(([id, s]) => (
        <p key={id} className="text-sm text-danger">
          {t(s === "fighting" ? "routing.fighting" : "routing.partly", {
            name: name(id),
          })}
        </p>
      ))}
      {p.state.notice && <p className="text-xs text-muted">{p.state.notice}</p>}

      {p.report && p.report.results.length > 0 && (
        <>
          <h4 className="text-sm font-medium">{t("routing.results")}</h4>
          <ul className="flex flex-col max-w-xl">
            {p.report.results.map((r, i) => (
              <Row key={i}>
                <span className={r.error ? "font-medium text-danger" : "font-medium"}>{r.error ? t("routing.failed") : t("routing.ok")}</span>{" "}
                <span className="allow-select">
                  {t(KIND[r.kind])} {target(r)}
                </span>
                {r.error && <span className="block text-xs text-danger allow-select">{r.error}</span>}
              </Row>
            ))}
          </ul>
        </>
      )}
    </Group>
  );
}

/** Lists the subnets to keep of a greedy route (R27); an empty list cannot
 *  be saved. */
function NarrowDialog({ g, onClose, onSave }: { g: GreedyRoute; onClose: () => void; onSave: (keep: string[]) => void }) {
  const [keep, setKeep] = useState(g.keep);
  return (
    <Dialog
      title={t("routing.narrowTitle", { dst: g.dst })}
      onClose={onClose}
      footer={
        <>
          {keep.length === 0 && <span className="mr-auto text-xs text-muted">{t("routing.keepOne")}</span>}
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" disabled={keep.length === 0} onClick={() => onSave(keep)}>
            {t("routing.narrow")}
          </Button>
        </>
      }
    >
      <p className="text-sm text-muted mb-3">{t("routing.narrowHint", { dst: g.dst, iface: g.iface })}</p>
      <Field label={t("routing.keep")}>
        <TagEditor tags={keep} placeholder="10.1.0.0/16" onChange={setKeep} />
      </Field>
    </Dialog>
  );
}

/** Hands a response to `set` only if no newer request began since: a slow older one is stale. */
export function latestOnly(seq: { current: number }, req: Promise<RoutePlan>, set: (p: RoutePlan | null) => void, fail?: (e: unknown) => void) {
  const mine = ++seq.current;
  req
    .then((p) => mine === seq.current && set(p))
    .catch((e) => {
      if (mine !== seq.current) return;
      set(null);
      fail?.(e);
    });
}

/** System routes in Settings, beside the VPN profiles. */
export function RoutingView() {
  const run = useAsyncToast();
  const { vpns } = useVpns();
  const [helper, setHelper] = useState<HelperStatus | null>(null);
  const [profile, setProfile] = useState("");
  const [mode, setMode] = useState<Mode>("");
  const [plan, setPlan] = useState<RoutePlan | null>(null);
  const [report, setReport] = useState<RoutingReport | null>(null);
  const [pac, setPac] = useState<PacState>();
  const [state, setState] = useState<RoutingState>({
    profiles: {},
    reset: false,
  });
  const [local, setLocal] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [planError, setPlanError] = useState("");
  const { confirm } = usePrompt();
  const [narrowing, setNarrowing] = useState<GreedyRoute | null>(null);

  const first = routed(vpns)[0];
  const id = profile || first?.id || "";
  const previewSeq = useRef(0);
  const preview = useCallback(() => {
    setPlanError("");
    latestOnly(previewSeq, api.previewRoutes(id, id ? mode : ""), setPlan, (e) => setPlanError(api.errorInfo(e).message));
  }, [id, mode]);
  const previewRef = useRef(preview);
  previewRef.current = preview;

  useEffect(() => {
    // An unknown helper is not a ready one: Apply stays off and says why.
    api
      .helperStatus()
      .then(setHelper)
      .catch((e) => setHelper({ state: "unavailable", reason: api.errorInfo(e).message } as HelperStatus));
    api
      .routingState()
      .then(setState)
      .catch(() => {});
    api
      .localRoutes()
      .then(setLocal)
      .catch(() => {});
    api
      .pacState()
      .then(setPac)
      .catch(() => {});
    reloadVpns();
    const offs = [api.onRoutingState(setState), api.onPacState(setPac)];
    return () => {
      offs.forEach((off) => off.then((f) => f()));
    };
  }, []);
  useEffect(preview, [preview]);

  const pick = (next: string) => {
    setProfile(next);
    setMode(modeOf(vpns.find((v) => v.id === next)));
  };
  useEffect(() => {
    if (!profile && first) setMode(modeOf(first));
  }, [profile, first]);

  const act = async <T,>(fn: () => Promise<T>, ok?: string) => {
    setBusy(true);
    const r = await run(fn, ok);
    setBusy(false);
    previewRef.current();
    return r;
  };

  return (
    <>
      <RoutingPanel
        helper={helper}
        vpns={vpns}
        profile={id}
        mode={mode}
        plan={plan}
        planError={planError}
        onRetry={preview}
        report={report}
        state={state}
        local={local}
        busy={busy}
        onProfile={pick}
        onMode={setMode}
        onLocal={(dsts) => act(() => api.setLocalRoutes(dsts)).then(() => api.localRoutes().then(setLocal))}
        onApply={() =>
          act(() => api.applyRoutes(id, id ? mode : ""), t("routing.applied")).then((r) => {
            if (r) setReport(r);
            reloadVpns();
          })
        }
        onReset={async () => {
          // Reset removes every route Hostbay added, under any live session.
          if (await confirm(t("routing.resetConfirm"), { confirmLabel: t("routing.reset") }))
            act(() => api.resetRoutes(), t("routing.resetDone")).then((r) => r && setReport(r));
        }}
        onInstall={() => act(() => api.installHelper()).then(() => api.helperStatus().then(setHelper))}
        onNarrow={setNarrowing}
        onPin={(p, iface) => act(() => api.pinInterface(p, iface))}
        pac={pac}
      />
      {narrowing && (
        <NarrowDialog
          g={narrowing}
          onClose={() => setNarrowing(null)}
          onSave={(keep) => {
            const g = narrowing;
            setNarrowing(null);
            act(() => api.narrowRoute(g.profile, g.dst, keep));
          }}
        />
      )}
    </>
  );
}
