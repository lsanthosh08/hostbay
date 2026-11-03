import { useEffect, useRef, useState } from "react";
import { t } from "../i18n";
import { Button, focusRing } from "../ui/Button";
import { ApprovalIcon, ErrorIcon, InfoIcon, NotificationsIcon, SuccessIcon } from "../ui/icons";
import { layer } from "../ui/layers";
import { notices, useNotices, type Notice } from "../ui/notices";
import { APPROVAL_TRAY_ID } from "./ai/ApprovalTray";

const look = {
  info: [InfoIcon, "text-muted"],
  success: [SuccessIcon, "text-success"],
  error: [ErrorIcon, "text-danger"],
  approval: [ApprovalIcon, "text-warning"],
} as const;

const time = (at: number) => new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

/** The rail's bell: a count of approvals still waiting, else a dot for
 *  unread notices, and a panel beside the rail with every notice of this
 *  session, newest first. A waiting approval opens the approval tray. */
export function Notifications() {
  const { list, unread } = useNotices();
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const waiting = list.filter((n) => n.pending).length;

  useEffect(() => {
    if (!open) return;
    notices.read();
    const away = (e: MouseEvent) => !root.current?.contains(e.target as Node) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("mousedown", away);
    window.addEventListener("keydown", esc);
    return () => {
      window.removeEventListener("mousedown", away);
      window.removeEventListener("keydown", esc);
    };
  }, [open, unread]);

  const review = (n: Notice) => {
    if (!n.pending) return;
    setOpen(false);
    document.getElementById(APPROVAL_TRAY_ID)?.focus();
  };

  const label = waiting ? t("notices.waiting", { n: waiting }) : t("notices.title");
  return (
    <div ref={root} className="relative">
      <button
        type="button"
        aria-label={label}
        title={label}
        aria-expanded={open}
        aria-haspopup="dialog"
        onClick={() => setOpen(!open)}
        className={
          `relative size-9 inline-flex items-center justify-center rounded-control transition-colors ${focusRing} ` +
          (open ? "text-fg bg-active" : "text-muted hover:text-fg hover:bg-hover")
        }
      >
        <NotificationsIcon className="size-5" aria-hidden />
        {waiting > 0 ? (
          <span className="absolute top-0.5 right-0.5 min-w-4 h-4 px-1 rounded-full bg-warning text-[10px] leading-4 font-medium text-black tabular-nums ring-2 ring-surface" aria-hidden>
            {waiting}
          </span>
        ) : (
          unread > 0 && <span className="absolute top-1.5 right-1.5 size-2 rounded-full bg-accent ring-2 ring-surface" aria-hidden />
        )}
      </button>
      {open && (
        <section
          role="dialog"
          aria-label={t("notices.title")}
          className={`fixed left-14 bottom-2 ${layer.popover} w-96 max-w-[calc(100vw-4.5rem)] max-h-[70vh] panel rounded-card shadow-float flex flex-col`}
        >
          <header className="flex items-center gap-2 px-3 py-2 border-b">
            <h2 className="flex-1 text-sm font-medium">{t("notices.title")}</h2>
            <Button size="sm" variant="ghost" disabled={!list.some((n) => !n.pending)} onClick={() => notices.clear()}>
              {t("notices.clear")}
            </Button>
          </header>
          {list.length === 0 ? (
            <p className="p-4 text-sm text-muted">{t("notices.empty")}</p>
          ) : (
            <ul className="overflow-auto py-1">
              {list.map((n) => {
                const [Icon, tone] = look[n.kind];
                const body = (
                  <>
                    <Icon className={`size-4 mt-0.5 shrink-0 ${tone}`} aria-hidden />
                    <span className="flex-1 min-w-0 break-words">
                      {n.msg}
                      {n.pending && <span className="block text-xs text-warning">{t("notices.review")}</span>}
                    </span>
                    <time className="text-xs text-muted tabular-nums shrink-0">{time(n.at)}</time>
                  </>
                );
                return (
                  <li key={n.id}>
                    {n.pending ? (
                      <button type="button" onClick={() => review(n)} className={`w-full text-left px-3 py-2 text-sm flex items-start gap-2 hover:bg-hover ${focusRing}`}>
                        {body}
                      </button>
                    ) : (
                      <div className={`px-3 py-2 text-sm flex items-start gap-2 allow-select ${n.pending === false ? "text-muted" : ""}`}>{body}</div>
                    )}
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      )}
    </div>
  );
}
