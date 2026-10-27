import type { AISkill } from "../../api";
import { t } from "../../i18n";

// The name being typed after a leading run of /names, as Go reads them
// (internal/ai/skills.go).
const typing = /^(?:\/[a-z0-9_-]+\s+)*\/([a-z0-9_-]*)$/;

/** The saved skills and templates the draft's last /word can still name,
 *  by name; none once a space follows it. */
/** True while the draft ends in a "/name" being typed. */
export function isSlashDraft(draft: string): boolean {
  return typing.test(draft);
}

export function slashMatches(draft: string, skills: AISkill[]): AISkill[] {
  const m = typing.exec(draft);
  if (!m) return [];
  return skills.filter((s) => s.name.startsWith(m[1])).sort((a, b) => a.name.localeCompare(b.name));
}

/** The draft with its last /word completed to name. */
export function pickSlash(draft: string, name: string): string {
  return draft.replace(/\/[a-z0-9_-]*$/, `/${name} `);
}

/** The / menu above the AI sidebar's message box (R61). Picking a skill
 *  attaches it to the message; a template becomes the message. Every
 *  field is shown as text. */
export function SlashMenu({ matches, active, onPick }: { matches: AISkill[]; active: number; onPick: (s: AISkill) => void }) {
  return (
    <ul id="ai-slash-menu" role="listbox" aria-label={t("ai.slash.label")} className="max-h-48 overflow-auto rounded-control border border-line bg-raised text-sm">
      {matches.map((s, i) => (
        <li
          key={s.id}
          id={`ai-slash-${s.id}`}
          role="option"
          aria-selected={i === active}
          onMouseDown={(e) => {
            e.preventDefault(); // keep focus in the message box
            onPick(s);
          }}
          className={`px-2 py-1 cursor-pointer flex flex-col ${i === active ? "bg-active" : "hover:bg-hover"}`}
        >
          <span className="flex items-baseline gap-2">
            <span className="font-mono">/{s.name}</span>
            <span className="text-xs text-muted">{s.kind === "template" ? t("skills.kind.template") : t("skills.kind.skill")}</span>
            {s.agentAuthored && <span className="text-xs text-warning">{t("skills.agentAuthored")}</span>}
          </span>
          {s.description && <span className="text-xs text-muted truncate">{s.description}</span>}
        </li>
      ))}
    </ul>
  );
}
