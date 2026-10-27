import { useEffect, useRef } from "react";
import { bindingOf, platform, recording, type KeyInput } from "../../actions/shortcuts";
import { t } from "../../i18n";

/** Waits for one key press and reports its binding; Esc cancels. While it
 *  waits no shortcut fires (KTD40), and the key reaches nothing else. A
 *  modifier alone or a dead key keeps it waiting. */
export function KeyRecorder({ onKey, onCancel }: { onKey: (binding: string, e: KeyInput) => void; onCancel: () => void }) {
  const ref = useRef<HTMLSpanElement>(null);
  const live = useRef({ onKey, onCancel });
  live.current = { onKey, onCancel };
  useEffect(() => {
    recording.on = true;
    ref.current?.focus();
    const onKeyDown = (e: KeyboardEvent) => {
      e.preventDefault();
      e.stopPropagation();
      if (e.key === "Escape" && !e.metaKey && !e.ctrlKey && !e.altKey && !e.shiftKey) return live.current.onCancel();
      const b = bindingOf(e, platform);
      if (b) live.current.onKey(b, e);
    };
    window.addEventListener("keydown", onKeyDown, true);
    return () => {
      recording.on = false;
      window.removeEventListener("keydown", onKeyDown, true);
    };
  }, []);
  return (
    <span ref={ref} tabIndex={-1} role="status" onBlur={() => live.current.onCancel()} className="text-xs text-accent outline-none">
      {t("keys.recording")}
    </span>
  );
}
