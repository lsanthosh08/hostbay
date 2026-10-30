import { useEffect, useState } from "react";
import type { Vpn } from "../api";
import { open, openFolder } from "../dialogs";
import { t } from "../i18n";
import { commitVpnImport, previewVpnImport, scanVpnConfigs, type FoundVpnConfig, type VpnDraft, type VpnImportPreview } from "../importApi";
import { RouteEditor } from "../shell/settings/Vpns";
import { AUTH_LABEL, hasRouting, typeLabel, validateVpn, type VpnField } from "../shell/vpnForm";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Field, Input } from "../ui/Field";
import { useAsyncToast, useToast } from "../ui/toast";
import { TagEditor } from "./TagEditor";
import { reloadVpns } from "../state/vpns";

/** How far the review is: drafts decided so far and the ones kept. */
export type Review = { at: number; kept: Vpn[] };
/** Records Save (the edited profile) or Skip (null) for the current draft. */
export const decide = (r: Review, keep: Vpn | null): Review => ({ at: r.at + 1, kept: keep ? [...r.kept, keep] : r.kept });

// The fields the review edits; the rest come from the file as they are.
const EDITED: VpnField[] = ["name", "routes", "dnsServers", "dnsDomains"];
const reviewErrors = (v: Vpn) => {
  const all = validateVpn(v);
  return Object.fromEntries(EDITED.filter((f) => all[f]).map((f) => [f, all[f]!])) as Partial<typeof all>;
};

// .tblk and .visc are packages on a Mac with their client installed, so the file picker takes them.
const FILE_TYPES = ["ovpn", "conf", "xml", "mobileconfig", "nmconnection", "pbk", "json", "tblk", "visc"];

/** One imported profile under review (R42): its gateway and sign-in as the
 *  file has them, name, tags, routes and DNS to edit, what the file held
 *  that was dropped (R43), and whether the password is asked for on first
 *  connect (R44). */
export function VpnDraftView({ draft, value, tried, onChange }: { draft: VpnDraft; value: Vpn; tried: boolean; onChange: (v: Vpn) => void }) {
  const set = (patch: Partial<Vpn>) => onChange({ ...value, ...patch });
  const errors = reviewErrors(value);
  const err = (f: VpnField) => (tried && errors[f] ? t(errors[f]!) : undefined);
  const ignored = draft.ignored ?? [];
  const p = draft.profile;
  return (
    <div className="flex flex-col gap-3">
      <Field label={t("vpnForm.name")} error={err("name")}>
        <Input className="w-full" autoFocus value={value.name} aria-invalid={!!err("name")} onChange={(e) => set({ name: e.target.value })} />
      </Field>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm allow-select">
        <dt className="text-muted">{t("vpnForm.type")}</dt>
        <dd>{typeLabel(p.type)}</dd>
        {p.gateway && (
          <>
            <dt className="text-muted">{t("vpnImport.gateway")}</dt>
            <dd className="break-all">{p.port ? `${p.gateway}:${p.port}` : p.gateway}</dd>
          </>
        )}
        <dt className="text-muted">{t("vpnForm.auth")}</dt>
        <dd>
          {p.auth ? t(AUTH_LABEL[p.auth] ?? "vpnImport.signInNone") : t("vpnImport.signInNone")}
          {p.user ? ` · ${p.user}` : ""}
        </dd>
      </dl>
      {draft.askPassword && <p className="text-xs text-warning">{t("vpnImport.askPassword")}</p>}
      <Field label={t("vpnForm.tags")} hint={t("vpnForm.tagsHint")}>
        <TagEditor tags={value.tags ?? []} onChange={(tags) => set({ tags })} />
      </Field>
      {hasRouting(p.type) && (
        <section className="flex flex-col gap-3 pt-2 border-t border-line">
          <h3 className="text-sm font-semibold">{t("vpnForm.routing")}</h3>
          <RouteEditor v={value} set={set} err={err} />
        </section>
      )}
      {ignored.length > 0 && (
        <section className="flex flex-col gap-1 pt-2 border-t border-line">
          <h3 className="text-sm font-semibold">{t("vpnImport.ignored", { count: ignored.length })}</h3>
          <p className="text-xs text-muted">{t("vpnImport.ignoredHint")}</p>
          <ul className="text-xs font-mono allow-select">
            {ignored.map((line, i) => (
              <li key={i} className="break-all">
                {line}
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

/** What the file held besides profiles: entries not imported, notes, and
 *  browser mappings, saved with the profiles kept. */
function PreviewNotes({ pre }: { pre: VpnImportPreview }) {
  const skipped = pre.skipped ?? [];
  const notes = pre.notes ?? [];
  const browsers = pre.browsers ?? [];
  if (!skipped.length && !notes.length && !browsers.length) return null;
  return (
    <div className="flex flex-col gap-2 text-xs mb-3 allow-select">
      {browsers.length > 0 && (
        <div>
          <p className="text-muted">{t("vpnImport.browsers", { count: browsers.length })}</p>
          <ul>
            {browsers.map((b, i) => (
              <li key={i}>
                {b.profile} → {b.vpn}
              </li>
            ))}
          </ul>
        </div>
      )}
      {skipped.length > 0 && (
        <div>
          <p className="text-muted">{t("import.skipped", { count: skipped.length })}</p>
          <ul>
            {skipped.map((s, i) => (
              <li key={i} className="break-words">
                <span className="font-medium">{s.file}</span> {s.reason}
              </li>
            ))}
          </ul>
        </div>
      )}
      {notes.length > 0 && (
        <div>
          <p className="text-muted">{t("import.notes", { count: notes.length })}</p>
          <ul>
            {notes.map((n, i) => (
              <li key={i} className="break-words">
                {n}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

/** Imports VPN profiles (R38, R39, F5): pick a file or bundle, or one an
 *  installed client keeps, then save or skip each profile it holds. Nothing
 *  is saved until a profile is saved; closing keeps only those saved. With
 *  `path`, it opens that file straight away. */
export function VpnImport({ path, onClose }: { path?: string; onClose: () => void }) {
  const run = useAsyncToast();
  const { push } = useToast();
  const [pre, setPre] = useState<VpnImportPreview | null>(null);
  const [review, setReview] = useState<Review>({ at: 0, kept: [] });
  const [value, setValue] = useState<Vpn | null>(null);
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [found, setFound] = useState<FoundVpnConfig[] | null>(null);

  const load = async (p: string) => {
    setBusy(true);
    const r = await run(() => previewVpnImport(p));
    setBusy(false);
    if (!r) return;
    setPre(r);
    setReview({ at: 0, kept: [] });
    setValue(r.drafts[0]?.profile ?? null);
  };
  useEffect(() => {
    if (path) load(path);
    else scanVpnConfigs().then(setFound, () => setFound([]));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path]);

  const finish = async (kept: Vpn[]) => {
    if (!pre) return onClose();
    setBusy(true);
    const rep = await run(() => commitVpnImport(pre.token, kept));
    setBusy(false);
    if (rep === undefined) return; // a refused edit: the import stays open
    if (kept.length) {
      push("success", t("vpnImport.saved", { count: rep.vpns?.length ?? 0 }));
      reloadVpns();
    }
    onClose();
  };
  const step = (keep: boolean) => {
    if (!pre || !value) return;
    if (keep && Object.keys(reviewErrors(value)).length) return setTried(true);
    const next = decide(review, keep ? { ...value, name: value.name.trim() } : null);
    if (next.at >= pre.drafts.length) return finish(next.kept);
    setReview(next);
    setValue(pre.drafts[next.at].profile);
    setTried(false);
  };

  const draft = pre?.drafts[review.at];
  const pick = async () => {
    const p = await open({ filters: [{ name: "VPN", extensions: FILE_TYPES }, { name: "All files", extensions: ["*"] }] });
    if (typeof p === "string") load(p);
  };
  const pickFolder = async () => {
    const p = await openFolder();
    if (p) load(p);
  };

  return (
    <Dialog
      title={draft ? t("vpnImport.review", { name: draft.profile.name, n: review.at + 1, count: pre!.drafts.length }) : t("vpn.import")}
      onClose={() => (pre ? finish(review.kept) : onClose())}
      dismissible={!pre}
      size="lg"
      footer={
        draft && value ? (
          <>
            <Button variant="ghost" disabled={busy} onClick={() => step(false)}>
              {t("vpnImport.skip")}
            </Button>
            <Button variant="primary" disabled={busy} onClick={() => step(true)}>
              {t("hostForm.save")}
            </Button>
          </>
        ) : (
          <Button variant="ghost" disabled={busy} onClick={() => (pre ? finish([]) : onClose())}>
            {t("common.close")}
          </Button>
        )
      }
    >
      {pre && review.at === 0 && <PreviewNotes pre={pre} />}
      {draft && value ? (
        <VpnDraftView key={draft.profile.id} draft={draft} value={value} tried={tried} onChange={setValue} />
      ) : pre ? (
        !pre.drafts.length && <p className="text-sm text-muted">{t("vpnImport.nothing")}</p>
      ) : (
        <div className="flex flex-col gap-4">
          <p className="text-sm text-muted">{t("vpnImport.formats")}</p>
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" disabled={busy} onClick={pick}>
              {t("vpnImport.chooseFile")}
            </Button>
            <Button disabled={busy} onClick={pickFolder}>
              {t("vpnImport.chooseFolder")}
            </Button>
          </div>
          {found !== null && !path && (
            <section className="flex flex-col gap-1">
              <h3 className="text-sm font-semibold">{t("vpnImport.found")}</h3>
              {found.length === 0 ? (
                <p className="text-xs text-muted">{t("vpnImport.foundNone")}</p>
              ) : (
                <ul className="flex flex-col">
                  {found.map((f) => (
                    <li key={f.path}>
                      <button
                        type="button"
                        disabled={!f.readable || busy}
                        title={f.readable ? f.path : t("vpnImport.unreadable")}
                        onClick={() => load(f.path)}
                        className="w-full text-left px-2 py-1 rounded text-sm hover:bg-hover disabled:opacity-60 disabled:hover:bg-transparent"
                      >
                        <span className="font-medium">{f.client}</span> <span className="text-muted break-all">{f.path}</span>
                        {!f.readable && <span className="block text-xs text-muted">{t("vpnImport.unreadable")}</span>}
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </section>
          )}
        </div>
      )}
    </Dialog>
  );
}
