import { useEffect, useRef, type MouseEvent } from "react";
import { t } from "../i18n";
import { keysFor } from "../actions/registry";
import { useVpns } from "../components/VpnPanel";
import { SETTINGS_TAB, activeTab, connLabel, tabsOf, useSessions, vpnTabNote, type Connection } from "../state/sessions";
import { useContextMenu } from "../ui/contextmenu";
import { IconButton } from "../ui/IconButton";
import { AddIcon, CommandIcon, ExpandedIcon, SettingsIcon, WorkspaceIcon } from "../ui/icons";
import { Kbd } from "../ui/Kbd";
import { StatusDot } from "../ui/StatusDot";
import { Tabs, type TabItem } from "../ui/Tabs";
import { sessionIcon } from "./kindIcon";

/** One tab per session, or per split tab (R65): kind icon, status dot (with
 *  its words while it is not live), close, drag to reorder or into a pane,
 *  and a menu listing every tab. A split tab shows its focused pane's state. */
export function SessionTabBar({ onStart, onPalette }: { onStart: () => void; onPalette: () => void }) {
  const state = useSessions();
  const { sessions, activate, disconnect, move, settings, showSettings, layouts } = state;
  const { vpns, states } = useVpns();
  const tabs = tabsOf(state).map((tab) => {
    const l = layouts.find((x) => x.id === tab.id);
    const panes = tab.sessions.map((id) => sessions.find((c) => c.sessionId === id)!);
    const c: Connection = panes.find((x) => x.sessionId === l?.focus) ?? panes[0];
    const name = l ? l.name ?? panes.map((x) => x.host.alias).join(" + ") : c.host.alias;
    // "Connecting VPN P" while the host's profile comes up, "via P" once live.
    const note = vpnTabNote(c, vpns, states);
    const status = note?.kind === "pending" ? t("connect.vpnPending", { name: note.name }) : connLabel(c);
    const via = note?.kind === "via" ? t("tab.via", { name: note.name }) : "";
    return { id: tab.id, c, name, status, via, icon: l ? WorkspaceIcon : sessionIcon(c), close: () => panes.forEach((x) => disconnect(x.sessionId)) };
  });
  const current = activeTab(state);
  const { at, node: menuNode } = useContextMenu();
  const strip = useRef<HTMLDivElement>(null);

  // Keep the active tab in view when the strip scrolls.
  useEffect(() => {
    strip.current?.querySelector(`[data-tab="${current}"]`)?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [current]);

  const openOverflow = (e: MouseEvent<HTMLElement>) => {
    e.stopPropagation();
    const r = e.currentTarget.getBoundingClientRect();
    at(
      tabs.map((tab, i) => ({
        label: `${tab.name} — ${tab.status}`,
        icon: tab.icon,
        shortcut: keysFor(`tab.${i + 1}`),
        onClick: () => activate(tab.id),
      })),
      r.right - 240,
      r.bottom + 4
    );
  };

  return (
    <div className="h-10 shrink-0 flex items-end gap-1 pl-2 pr-1 border-b border-line">
      <div ref={strip} className="min-w-0 overflow-x-auto overflow-y-hidden flex-1 flex items-end [scrollbar-width:none]">
        <Tabs
          label={t("tabs.label")}
          value={current ?? ""}
          onChange={activate}
          onReorder={move}
          className="h-10"
          items={tabs.map<TabItem<string>>(({ id, c, name, status, via, icon, close }) => ({
            id,
            icon,
            title: `${name} — ${status}${via ? ` (${via})` : ""}`,
            label: (
              <span className="flex items-center gap-1.5 max-w-48">
                <StatusDot status={c.status} />
                <span className="truncate">{name}</span>
                {c.status !== "live" && <span className="text-xs text-muted truncate">{status}</span>}
                {via && <span className="text-xs text-muted truncate shrink-0 max-w-24">{via}</span>}
              </span>
            ),
            onClose: close,
          })).concat(
            // The Settings tab sits after the sessions while it is open.
            settings
              ? [{ id: SETTINGS_TAB, icon: SettingsIcon, title: t("settings.title"), label: t("settings.title"), onClose: () => showSettings(null) }]
              : []
          )}
        />
      </div>
      <div className="flex items-center gap-0.5 self-center shrink-0">
        <IconButton label={t("tabs.newConnection")} icon={AddIcon} onClick={onStart} />
        {sessions.length > 0 && (
          <IconButton label={t("tabs.all")} icon={ExpandedIcon} aria-haspopup="menu" onClick={openOverflow} />
        )}
        <button
          onClick={onPalette}
          title={t("action.palette")}
          className="ml-1 inline-flex items-center gap-1.5 px-2 h-7 rounded-control text-xs text-muted hover:text-fg hover:bg-hover"
        >
          <CommandIcon className="size-3.5" aria-hidden />
          <Kbd>{keysFor("palette.open")}</Kbd>
        </button>
      </div>
      {menuNode}
    </div>
  );
}
