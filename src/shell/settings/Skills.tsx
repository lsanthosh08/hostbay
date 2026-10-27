import { useEffect, useState } from "react";
import * as api from "../../api";
import type { AISkill, AISkillKind } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Field, Input } from "../../ui/Field";
import { IconButton } from "../../ui/IconButton";
import { AddIcon, DeleteIcon, EditIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { Select } from "../../ui/Select";
import { useAsyncToast, useToast } from "../../ui/toast";
import { Group, Row } from "./Group";

const NEW: AISkill = { id: "", name: "", description: "", body: "", kind: "skill", agentAuthored: false };

/** Settings > Skills (R61): skills and prompt templates the AI sidebar
 *  attaches with /name. They sync and go into backups; none holds a
 *  secret. Every field is shown as text. */
export function Skills() {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [skills, setSkills] = useState<AISkill[]>([]);
  const [editing, setEditing] = useState<AISkill | null>(null);
  const reload = () => api.aiListSkills().then(setSkills).catch(() => {});
  useEffect(() => {
    reload();
  }, []);

  const remove = async (s: AISkill) => {
    if (!(await confirm(t("skills.deleteConfirm", { name: s.name })))) return;
    await run(() => api.aiDeleteSkill(s.id), t("skills.deleted"));
    reload();
  };

  return (
    <Group title={t("skills.title")} description={t("skills.hint")}>
      <Button className="w-fit" icon={AddIcon} onClick={() => setEditing({ ...NEW })}>
        {t("skills.add")}
      </Button>
      {skills.length === 0 ? (
        <p className="text-sm text-muted">{t("skills.empty")}</p>
      ) : (
        <ul className="flex flex-col max-w-xl">
          {skills.map((s) => (
            <Row
              key={s.id}
              actions={
                <>
                  <IconButton label={t("skills.edit", { name: s.name })} icon={EditIcon} onClick={() => setEditing(s)} />
                  <IconButton label={t("skills.delete", { name: s.name })} icon={DeleteIcon} tone="danger" onClick={() => remove(s)} />
                </>
              }
            >
              <span className="font-mono">/{s.name}</span>
              <span className="text-muted"> · {s.kind === "template" ? t("skills.kind.template") : t("skills.kind.skill")}</span>
              {s.agentAuthored && <span className="text-xs text-warning"> · {t("skills.agentAuthored")}</span>}
              {s.description && <span className="block text-xs text-muted truncate">{s.description}</span>}
            </Row>
          ))}
        </ul>
      )}
      {editing && (
        <SkillForm
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reload();
          }}
        />
      )}
    </Group>
  );
}

function SkillForm({ initial, onClose, onSaved }: { initial: AISkill; onClose: () => void; onSaved: () => void }) {
  const run = useAsyncToast();
  const [s, setS] = useState(initial);
  const set = (patch: Partial<AISkill>) => setS((x) => ({ ...x, ...patch }));
  const save = async () => {
    const ok = await run(() => api.aiSaveSkill(s), t("skills.saved"));
    if (ok !== undefined) onSaved();
  };
  return (
    <Dialog
      title={s.id ? t("skills.editTitle", { name: initial.name }) : t("skills.newTitle")}
      onClose={onClose}
      dismissible={JSON.stringify(s) === JSON.stringify(initial)}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" form="skill-form">
            {t("hostForm.save")}
          </Button>
        </>
      }
    >
      <form
        id="skill-form"
        noValidate
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("skills.name")} hint={t("skills.nameHint")}>
            <Input className="w-full font-mono" autoFocus value={s.name} onChange={(e) => set({ name: e.target.value })} />
          </Field>
          <Field label={t("skills.kind")}>
            <Select className="w-full" value={s.kind} onChange={(e) => set({ kind: e.target.value as AISkillKind })}>
              <option value="skill">{t("skills.kind.skill")}</option>
              <option value="template">{t("skills.kind.template")}</option>
            </Select>
          </Field>
        </div>
        <Field label={t("skills.description")}>
          <Input className="w-full" value={s.description} onChange={(e) => set({ description: e.target.value })} />
        </Field>
        <Field label={t("skills.body")} hint={s.kind === "template" ? t("skills.bodyTemplateHint") : t("skills.bodySkillHint")}>
          <textarea
            aria-label={t("skills.body")}
            className="w-full min-h-40 rounded-control border border-control bg-field p-2 text-sm font-mono"
            value={s.body}
            onChange={(e) => set({ body: e.target.value })}
          />
        </Field>
      </form>
    </Dialog>
  );
}

/** Settings > AI's terminal suggestions switch (R62). It is this
 *  computer's own setting and off until turned on, because each pause in
 *  typing sends the line to the provider. */
export function Suggestions() {
  const { push } = useToast();
  const [on, setOn] = useState(false);
  useEffect(() => {
    api.settingsGetLocal().then((l) => setOn(!!l.aiSuggestions), () => {});
  }, []);
  return (
    <Group title={t("suggest.title")} description={t("suggest.hint")}>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={on}
          onChange={(e) => {
            const next = e.target.checked;
            api.settingsSetLocal({ aiSuggestions: next }).then(() => setOn(next), (err) => push("error", api.errorInfo(err).message));
          }}
        />
        {t("suggest.toggle")}
      </label>
    </Group>
  );
}
