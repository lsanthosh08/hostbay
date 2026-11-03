import { actions, keysFor, type Action, type AppCommands, type PanelId } from "../actions/registry";
import { t } from "../i18n";
import { Notifications } from "../components/Notifications";
import { focusRing } from "../ui/Button";
import { syncLabel, type SyncState } from "./StatusBar";

const BADGE: Record<SyncState, string> = {
  off: "",
  on: "bg-success",
  paused: "bg-warning",
  conflicts: "bg-warning",
  error: "bg-danger",
};

/** The left rail: one entry per side panel, then sync and appearance. Its
 *  entries come from the action registry; one that has not shipped is hidden.
 *  The bell at the foot lists this session's notices. */
export function ActivityRail({ app, panel, sync }: { app: AppCommands; panel: PanelId | null; sync: SyncState }) {
  const entry = (a: Action) => {
    const panelId = a.id.startsWith("panel.") ? (a.id.slice(6) as PanelId) : null;
    const on = panelId !== null && panelId === panel;
    const keys = keysFor(a.id);
    const label = a.id === "sync.open" ? `${t(a.label)}: ${syncLabel(sync)}` : t(a.label, a.vars);
    const Icon = a.icon!;
    return (
      <button
        key={a.id}
        type="button"
        aria-label={label}
        title={keys ? `${label} (${keys})` : label}
        aria-pressed={panelId ? on : undefined}
        // Choosing the open panel again folds the side panel away.
        onClick={() => (on ? app.toggleSidePanel() : a.run(app))}
        className={
          `relative size-9 inline-flex items-center justify-center rounded-control transition-colors ${focusRing} ` +
          (on ? "text-fg bg-active" : "text-muted hover:text-fg hover:bg-hover")
        }
      >
        {on && <span className="absolute -left-1.5 top-1.5 bottom-1.5 w-0.5 rounded-full bg-accent" aria-hidden />}
        <Icon className="size-5" aria-hidden />
        {a.id === "sync.open" && BADGE[sync] && (
          <span className={`absolute top-1.5 right-1.5 size-2 rounded-full ring-2 ring-surface ${BADGE[sync]}`} aria-hidden />
        )}
      </button>
    );
  };
  const shown = actions.filter((a) => a.rail && !a.hidden);
  return (
    <nav aria-label={t("nav.rail")} className="w-12 shrink-0 panel border-r flex flex-col items-center gap-1 py-2">
      {shown.filter((a) => a.rail === "top").map(entry)}
      <div className="flex-1" />
      {shown.filter((a) => a.rail === "bottom").map(entry)}
      <Notifications />
    </nav>
  );
}
