import { useState } from "react";
import { t } from "../i18n";
import { Input } from "../ui/Field";
import { CloseIcon } from "../ui/icons";

/** `tags` with `raw` added: trimmed, and dropped when it is empty or already
 *  there in any casing, so the casing first typed is the one kept (R6). */
export function addTag(tags: string[], raw: string): string[] {
  const tag = raw.trim();
  return !tag || tags.some((x) => x.toLowerCase() === tag.toLowerCase()) ? tags : [...tags, tag];
}

/** Free-text tags as chips. Enter (or leaving the field) adds what was
 *  typed; Backspace in the empty field removes the last one. Inside a Field,
 *  the field's label names the input. */
export function TagEditor({
  tags,
  onChange,
  placeholder,
  invalid,
}: {
  tags: string[];
  onChange: (tags: string[]) => void;
  placeholder?: string;
  invalid?: boolean;
}) {
  const [text, setText] = useState("");
  const commit = () => {
    if (!text.trim()) return;
    onChange(addTag(tags, text));
    setText("");
  };
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {tags.map((tag) => (
        <span key={tag} className="inline-flex items-center gap-1 h-6 pl-2 pr-1 rounded-full bg-raised border border-line text-xs allow-select">
          {tag}
          <button
            type="button"
            aria-label={t("tags.remove", { tag })}
            className="rounded-full p-0.5 text-muted hover:text-fg hover:bg-hover"
            onClick={() => onChange(tags.filter((x) => x !== tag))}
          >
            <CloseIcon className="size-3" aria-hidden />
          </button>
        </span>
      ))}
      <Input
        className="flex-1 min-w-32 !h-8"
        spellCheck={false}
        value={text}
        placeholder={placeholder ?? t("tags.placeholder")}
        aria-invalid={invalid}
        onChange={(e) => setText(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            commit();
          } else if (e.key === "Backspace" && !text && tags.length) onChange(tags.slice(0, -1));
        }}
      />
    </div>
  );
}
