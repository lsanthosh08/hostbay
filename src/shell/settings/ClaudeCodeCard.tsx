import { useCallback, useEffect, useState } from "react";
import * as api from "../../api";
import type { AIProvider, ClaudeCodeCandidate, ClaudeCodeStatus } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { useAsyncToast, useToast } from "../../ui/toast";

/** The setup card's body (R1, R5, R7): what Go found and what the person
 *  can do next. It shows only what Go returned and offers only the actions
 *  the state allows. Signing in is Anthropic's own flow, run in the person's
 *  terminal, so no state here has a sign-in action (R2). */
export function ClaudeCodeView({
  status,
  candidates,
  busy,
  onSelect,
  onPick,
  onRefresh,
  onConsent,
}: {
  status: ClaudeCodeStatus | null;
  candidates: ClaudeCodeCandidate[];
  busy: boolean;
  onSelect: (index: number) => void;
  onPick: () => void;
  onRefresh: () => void;
  onConsent: () => void;
}) {
  if (!status) return <p className="text-sm text-muted">{t("ai.cc.checking")}</p>;
  const { state } = status;
  const chosen = status.path !== "";
  const choose = state === "notFound" || state === "tooOld" || state === "unsupported" || state === "signedOut";
  return (
    <div className="flex flex-col gap-3 text-sm">
      <p>{state === "unsupported" ? status.message : t(MESSAGE[state], { version: status.version, method: status.authMethod })}</p>
      {chosen && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs">
          <dt className="text-muted">{t("ai.cc.path")}</dt>
          <dd className="allow-select break-all">{status.path}</dd>
          {status.target && status.target !== status.path && (
            <>
              <dt className="text-muted">{t("ai.cc.target")}</dt>
              <dd className="allow-select break-all">{status.target}</dd>
            </>
          )}
          {status.version && (
            <>
              <dt className="text-muted">{t("ai.cc.version")}</dt>
              <dd className="allow-select">{status.version}</dd>
            </>
          )}
          {status.authMethod && (
            <>
              <dt className="text-muted">{t("ai.cc.signedInWith")}</dt>
              <dd className="allow-select">{status.authMethod}</dd>
            </>
          )}
        </dl>
      )}
      {state === "consent" && (
        <div className="rounded-control border border-line p-3">
          <p className="text-xs font-medium">{t("ai.cc.sentTitle")}</p>
          <ul className="mt-1 list-disc pl-4 text-xs text-muted">
            <li>{t("ai.cc.sent1")}</li>
            <li>{t("ai.cc.sent2")}</li>
            <li>{t("ai.cc.sent3")}</li>
          </ul>
        </div>
      )}
      {choose && candidates.length > 0 && (
        <div>
          <p className="text-xs text-muted mb-1">{t("ai.cc.found")}</p>
          <ul className="flex flex-col gap-1">
            {candidates.map((c) => (
              <li key={c.index} className="flex items-center gap-2">
                <span className="flex-1 min-w-0 truncate text-xs allow-select" title={c.path}>
                  {c.path}
                </span>
                <Button size="sm" disabled={busy} onClick={() => onSelect(c.index)}>
                  {t("ai.cc.use")}
                </Button>
              </li>
            ))}
          </ul>
        </div>
      )}
      <div className="flex flex-wrap gap-2">
        {state === "consent" && (
          <Button variant="primary" disabled={busy} onClick={onConsent}>
            {t("ai.cc.review")}
          </Button>
        )}
        {(choose || state === "ready") && (
          <Button variant="ghost" disabled={busy} onClick={onPick}>
            {t("ai.cc.pick")}
          </Button>
        )}
        <Button variant="ghost" disabled={busy} onClick={onRefresh}>
          {t("ai.cc.recheck")}
        </Button>
      </div>
    </div>
  );
}

const MESSAGE = {
  notFound: "ai.cc.notFound",
  tooOld: "ai.cc.tooOld",
  signedOut: "ai.cc.signedOut",
  unsupported: "ai.cc.notFound", // replaced by Go's own message
  consent: "ai.cc.consentIntro",
  ready: "ai.cc.ready",
} as const;

/** Settings > AI > Claude Code login: the name and model of one provider,
 *  then the setup above. */
export function ClaudeCodeCard({ initial, onClose, onChanged }: { initial: AIProvider; onClose: () => void; onChanged: () => void }) {
  const run = useAsyncToast();
  const { push } = useToast();
  const [p, setP] = useState(initial);
  const [status, setStatus] = useState<ClaudeCodeStatus | null>(null);
  const [candidates, setCandidates] = useState<ClaudeCodeCandidate[]>([]);
  const [models, setModels] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [test, setTest] = useState<{ ok: boolean; text: string } | null>(null);

  const refresh = useCallback(async () => {
    setBusy(true);
    setTest(null); // a result belongs to the program it was run against
    try {
      const [found, state] = await Promise.all([api.aiClaudeCodeCandidates(), api.aiClaudeCodeState(initial.id)]);
      setCandidates(found);
      setStatus(state);
    } catch (e) {
      setStatus({ state: "unsupported", message: api.errorInfo(e).message, path: "", target: "", version: "", authMethod: "", consented: false });
    } finally {
      setBusy(false);
    }
  }, [initial.id]);
  useEffect(() => {
    refresh();
    api.aiDefaultModels("claude-code").then(setModels).catch(() => {});
  }, [refresh]);

  // A cancelled Go dialog changes nothing and needs no toast.
  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      onChanged();
    } catch (e) {
      const info = api.errorInfo(e);
      if (info.code !== "VerifyFailed") push("error", info.message);
    }
    setBusy(false);
    await refresh();
  };
  const save = async () => {
    setBusy(true);
    const saved = await run(() => api.aiSaveProvider(p, ""), t("ai.provider.saved"));
    setBusy(false);
    if (saved !== undefined) {
      onChanged();
      onClose();
    }
  };
  const runTest = async () => {
    setBusy(true);
    setTest(null);
    try {
      await api.aiTestProvider(p, "");
      setTest({ ok: true, text: t("ai.cc.ready", { version: status?.version ?? "", method: status?.authMethod ?? "" }) });
    } catch (e) {
      setTest({ ok: false, text: api.errorInfo(e).message });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={t("ai.cc.title")}
      onClose={onClose}
      dismissible={JSON.stringify(p) === JSON.stringify(initial)}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          {status?.state === "ready" && (
            <Button type="button" onClick={runTest} disabled={busy}>
              {t("ai.test")}
            </Button>
          )}
          <Button variant="primary" onClick={save} disabled={busy}>
            {t("hostForm.save")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <p className="text-sm text-muted">{t("ai.cc.intro")}</p>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("ai.provider.name")}>
            <Input className="w-full" value={p.name} onChange={(e) => setP({ ...p, name: e.target.value })} />
          </Field>
          <Field label={t("ai.provider.model")}>
            <Input className="w-full" list="ai-cc-models" spellCheck={false} value={p.model} onChange={(e) => setP({ ...p, model: e.target.value })} />
          </Field>
        </div>
        <datalist id="ai-cc-models">
          {models.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
        <ClaudeCodeView
          status={status}
          candidates={candidates}
          busy={busy}
          onSelect={(i) => act(() => api.aiClaudeCodeSelect(initial.id, i))}
          onPick={() => act(() => api.aiClaudeCodePickFile(initial.id))}
          onRefresh={refresh}
          onConsent={() => act(() => api.aiClaudeCodeConsent(initial.id))}
        />
        {test && (
          <p role="status" className={`text-sm ${test.ok ? "text-success" : "text-danger"}`}>
            {test.text}
          </p>
        )}
        <p className="text-xs text-muted">{t("ai.cc.localOnly")}</p>
        <p className="text-xs text-muted">{t("ai.cc.noSuggest")}</p>
      </div>
    </Dialog>
  );
}
