import { useRef, useState } from "react";
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
  separators = /[,\n]+/,
  bad,
}: {
  tags: string[];
  onChange: (tags: string[]) => void;
  placeholder?: string;
  invalid?: boolean;
  /** What splits pasted or typed text into several tags: commas and line
   *  breaks, since a tag such as a project name may hold spaces. */
  separators?: RegExp;
  /** Marks a chip the field will not accept. */
  bad?: (tag: string) => boolean;
}) {
  const [text, setText] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const commit = (raw = text) => {
    if (!raw.trim()) return;
    onChange(raw.split(separators).reduce(addTag, tags));
    setText("");
  };
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {tags.map((tag) => (
        <span
          key={tag}
          className={"inline-flex items-center gap-1 h-6 pl-2 pr-1 rounded-full bg-raised border text-xs allow-select " + (bad?.(tag) ? "border-danger text-danger" : "border-line")}
        >
          {tag}
          <button
            type="button"
            aria-label={t("tags.remove", { tag })}
            className="rounded-full p-1 text-muted hover:text-fg hover:bg-hover"
            onClick={() => {
              onChange(tags.filter((x) => x !== tag));
              input.current?.focus(); // the chip's button is gone with it
            }}
          >
            <CloseIcon className="size-3.5" aria-hidden />
          </button>
        </span>
      ))}
      <Input
        ref={input}
        className="flex-1 min-w-32 !h-8"
        spellCheck={false}
        value={text}
        placeholder={placeholder ?? t("tags.placeholder")}
        aria-invalid={invalid}
        // A pasted list becomes its tags at once instead of one invalid chip.
        onChange={(e) => (separators.test(e.target.value) ? commit(e.target.value) : setText(e.target.value))}
        onBlur={() => commit()}
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
