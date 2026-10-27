import { useEffect, useRef, useState } from "react";
import * as api from "../../api";
import type { AIPending } from "../../api";
import { t } from "../../i18n";
import { layer } from "../../ui/layers";
import { useToast } from "../../ui/toast";
import { ApprovalCard } from "./ApprovalCard";

/** The tray's element ID; the "ai.approvals" action focuses it. */
export const APPROVAL_TRAY_ID = "ai-approval-tray";

/** Every pending request from every conversation, oldest first, shown
 *  whether or not the sidebar is open (KTD32). A new card is announced
 *  without taking focus from the terminal. */
export function ApprovalTray() {
  const { push } = useToast();
  const [pending, setPending] = useState<AIPending[]>([]);
  const [announce, setAnnounce] = useState("");
  const seen = useRef(new Set<string>());

  useEffect(() => {
    let off: (() => void) | undefined;
    let gone = false;
    const show = (l: AIPending[]) => {
      const fresh = l.filter((p) => !seen.current.has(p.id));
      seen.current = new Set(l.map((p) => p.id));
      const last = fresh[fresh.length - 1];
      if (last)
        setAnnounce(
          last.tool === "save_skill"
            ? t("ai.tray.newSkill", { n: l.length })
            : last.client
              ? t("mcp.tray.new", { client: last.client, host: last.host, n: l.length })
              : t("ai.tray.new", { host: last.host, n: l.length }),
        );
      setPending(l);
    };
    api.onAIApprovals(show).then((f) => (gone ? f() : (off = f)));
    api.aiApprovals().then(show).catch(() => {});
    return () => {
      gone = true;
      off?.();
    };
  }, []);

  const answer = (p: AIPending, allow: boolean) =>
    api.aiAnswerApproval(p.id, allow).catch((e) => push("error", api.errorInfo(e).message));

  return (
    <>
      <div role="status" aria-live="polite" className="sr-only">
        {announce}
      </div>
      <section
        id={APPROVAL_TRAY_ID}
        tabIndex={-1}
        aria-label={t("ai.tray.title", { n: pending.length })}
        className={pending.length ? `fixed bottom-10 right-4 ${layer.popover} w-96 max-w-[calc(100vw-2rem)] max-h-[70vh] overflow-auto flex flex-col gap-2 outline-none` : "sr-only"}
      >
        {pending.map((p) => (
          <ApprovalCard key={p.id} p={p} onAnswer={(allow) => answer(p, allow)} />
        ))}
      </section>
    </>
  );
}
