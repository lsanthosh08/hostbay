import { useEffect, useState } from "react";
import * as api from "../api";
import type { PostLoginRule, PostLoginStep } from "../api";
import { t } from "../i18n";
import { Button } from "../ui/Button";
import { Field, Input } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { AddIcon, DeleteIcon } from "../ui/icons";
import { Select } from "../ui/Select";
import { useToast } from "../ui/toast";

const blank = (hostId: string): PostLoginRule => ({ id: "", hostId, steps: [], hasAnswer: false, held: false });

/** A saved host's post-login steps (R70, KTD39): each waits for a prompt or
 *  for quiet, then sends text, the host's saved password or the rule's own
 *  secret reply. The secret goes to Go and never comes back; the page only
 *  learns whether one is saved. It saves on its own, apart from the host
 *  form. */
export function PostLoginEditor({ hostId }: { hostId: string }) {
  const { push } = useToast();
  const [rule, setRule] = useState<PostLoginRule>(() => blank(hostId));
  const [answer, setAnswer] = useState("");
  const [busy, setBusy] = useState(false);
  const fail = (e: unknown) => push("error", api.errorInfo(e).message);
  useEffect(() => {
    api.postLoginRules(hostId).then((r) => setRule(r[0] ?? blank(hostId)), fail);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hostId]);

  const setStep = (i: number, patch: Partial<PostLoginStep>) =>
    setRule((r) => ({ ...r, steps: r.steps.map((s, j) => (j === i ? { ...s, ...patch } : s)) }));
  const usesSecret = rule.steps.some((s) => s.kind === "secret");

  const save = async () => {
    setBusy(true);
    try {
      if (rule.id && rule.steps.length === 0) {
        await api.deletePostLoginRule(rule.id);
        setRule(blank(hostId));
      } else {
        setRule(await api.savePostLoginRule(rule, usesSecret ? answer || null : ""));
      }
      setAnswer("");
      push("success", t("postLogin.saved"));
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    // Enter in a step would submit the host form around it and drop the steps.
    <div
      className="flex flex-col gap-3"
      onKeyDown={(e) => e.key === "Enter" && e.target instanceof HTMLInputElement && e.preventDefault()}
    >
      <p className="text-xs text-muted">{t("postLogin.hint")}</p>
      {rule.held && <p className="text-sm text-warning">{t("postLogin.held")}</p>}
      {rule.steps.map((s, i) => (
        <div key={i} className="flex items-end gap-2">
          <Field className="flex-1 min-w-0" label={t("postLogin.expect")} hint={i === 0 ? t("postLogin.expectHint") : undefined}>
            <Input className="w-full font-mono" spellCheck={false} value={s.expect ?? ""} onChange={(e) => setStep(i, { expect: e.target.value })} />
          </Field>
          <Field label={t("postLogin.reply")}>
            <Select value={s.kind ?? ""} onChange={(e) => setStep(i, { kind: e.target.value as PostLoginStep["kind"] })}>
              <option value="">{t("postLogin.kind.text")}</option>
              <option value="password">{t("postLogin.kind.password")}</option>
              <option value="secret">{t("postLogin.kind.secret")}</option>
            </Select>
          </Field>
          {!s.kind && (
            <Input
              className="flex-1 min-w-0 font-mono"
              aria-label={t("postLogin.reply")}
              spellCheck={false}
              value={s.reply ?? ""}
              onChange={(e) => setStep(i, { reply: e.target.value })}
            />
          )}
          <IconButton
            type="button"
            label={t("postLogin.removeStep")}
            icon={DeleteIcon}
            onClick={() => setRule((r) => ({ ...r, steps: r.steps.filter((_, j) => j !== i) }))}
          />
        </div>
      ))}
      {usesSecret && (
        <Field label={t("postLogin.secret")} hint={rule.hasAnswer ? t("postLogin.secretSaved") : t("postLogin.secretMissing")}>
          <Input type="password" className="w-full" autoComplete="off" value={answer} onChange={(e) => setAnswer(e.target.value)} />
        </Field>
      )}
      <div className="flex gap-2">
        <Button
          type="button"
          variant="ghost"
          icon={AddIcon}
          onClick={() => setRule((r) => ({ ...r, steps: [...r.steps, { expect: "", reply: "", kind: "" }] }))}
        >
          {t("postLogin.addStep")}
        </Button>
        <Button type="button" disabled={busy || (!rule.id && rule.steps.length === 0)} onClick={save}>
          {rule.id && rule.steps.length === 0 ? t("postLogin.delete") : t("postLogin.save")}
        </Button>
      </div>
    </div>
  );
}
