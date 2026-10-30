import { useEffect, useId, useState } from "react";
import * as api from "../api";
import { actions, keysFor, type AppCommands } from "../actions/registry";
import { t } from "../i18n";
import { isVpnActive, useVpns } from "../state/vpns";
import { useHosts } from "../state/hosts";
import { connLabel, tabsOf, useSessions, viewsFor } from "../state/sessions";
import { Dialog } from "../ui/Dialog";
import { SearchIcon, SplitDownIcon, SplitIcon, TerminalIcon, VncIcon, VpnIcon, type Icon } from "../ui/icons";
import { Kbd } from "../ui/Kbd";
import { fuzzyRank } from "./fuzzy";
import { hostIcon, sessionIcon } from "./kindIcon";
import { VIEW_INFO } from "./SessionLayer";

type Item = { key: string; label: string; group: string; icon?: Icon; keys?: string; run: () => void };

/** Finds and runs anything: connect to a host, switch to an open tab or a
 *  view, open a settings category, or run any registered action. */
export function CommandPalette({ app, onClose }: { app: AppCommands; onClose: () => void }) {
  const { hosts } = useHosts();
  const state = useSessions();
  const { sessions, active, connect, openX11Desktop, activate, setView, openLocalShell, split } = state;
  const [query, setQuery] = useState("");
  const [at, setAt] = useState(0);
  const [shells, setShells] = useState<api.LocalShell[]>([]);
  const { vpns, states } = useVpns();
  const listId = useId();
  useEffect(() => {
    api.listLocalShells().then(setShells).catch(() => {});
  }, []);

  const items: Item[] = [
      ...sessions.map((c) => ({
        key: `tab:${c.sessionId}`,
        label: c.host.alias,
        group: `${t("palette.tab")} · ${connLabel(c)}`,
        icon: sessionIcon(c),
        run: () => activate(c.sessionId),
      })),
      // Any other plain tab moves into a pane beside the focused one (R65).
      ...(active
        ? tabsOf(state)
            .filter((x) => x.plain && x.id !== active.sessionId)
            .flatMap((x) =>
              (["right", "bottom"] as const).map((side) => ({
                key: `split:${side}:${x.id}`,
                label: t(side === "right" ? "palette.splitRight" : "palette.splitDown", {
                  name: sessions.find((c) => c.sessionId === x.id)!.host.alias,
                }),
                group: t("palette.split"),
                icon: side === "right" ? SplitIcon : SplitDownIcon,
                run: () => split(x.id, active.sessionId, side),
              }))
            )
        : []),
      ...(active
        ? viewsFor(active).map((v) => ({
            key: `view:${v}`,
            label: VIEW_INFO[v].label,
            group: t("palette.view"),
            icon: VIEW_INFO[v].icon,
            run: () => setView(active.sessionId, v),
          }))
        : []),
      ...hosts.map((h) => ({
        key: `host:${h.id}`,
        label: h.alias,
        group: `${t("palette.connect")} · ${h.hostname}`,
        icon: hostIcon(h),
        run: () => connect(h),
      })),
      ...hosts
        .filter((h) => api.hostKind(h) === "ssh")
        .map((h) => ({
          key: `x11:${h.id}`,
          label: t("x11.openDesktopOn", { name: h.alias }),
          group: `${t("palette.action")} · ${h.hostname}`,
          icon: VncIcon,
          run: () => openX11Desktop(h),
        })),
      // vpn.connect and vpn.disconnect, one entry per profile (R8).
      ...vpns.map((v) =>
        isVpnActive(states[v.id])
          ? { key: `vpn.disconnect:${v.id}`, label: t("vpn.disconnectNamed", { name: v.name }), group: t("nav.vpns"), icon: VpnIcon, run: () => app.disconnectVpn(v.id) }
          : { key: `vpn.connect:${v.id}`, label: t("vpn.connectNamed", { name: v.name }), group: t("nav.vpns"), icon: VpnIcon, run: () => app.connectVpn(v.id) }
      ),
      ...shells.map((s) => ({
        key: `shell:${s.id}`,
        label: s.name,
        group: t("action.localShell"),
        icon: TerminalIcon,
        run: () => openLocalShell(s),
      })),
      ...actions
        .filter((a) => !a.hidden && a.palette !== false && (!a.available || a.available(app)))
        .map((a) => ({
          key: `action:${a.id}`,
          label: t(a.label, a.vars),
          group: a.group === "settings" ? t("palette.settings") : t("palette.action"),
          icon: a.icon,
          keys: keysFor(a.id),
          run: () => a.run(app),
        })),
  ];
  const shown = fuzzyRank(items, query, (i) => i.label).slice(0, 200);
  // Braces: Chromium's scrollIntoView returns a promise, which React would call as a cleanup.
  useEffect(() => {
    document.getElementById(`${listId}-${at}`)?.scrollIntoView({ block: "nearest" });
  }, [at, listId]);
  const pick = (i?: Item) => {
    if (!i) return;
    onClose();
    i.run();
  };

  return (
    <Dialog label={t("action.palette")} onClose={onClose} size="lg" bodyClassName="flex flex-col min-h-0">
      <div className="relative border-b border-line">
        <SearchIcon className="size-4 text-muted absolute left-4 top-1/2 -translate-y-1/2 pointer-events-none" aria-hidden />
        <input
          autoFocus
          role="combobox"
          aria-expanded
          aria-controls={listId}
          aria-activedescendant={shown[at] ? `${listId}-${at}` : undefined}
          aria-label={t("palette.search")}
          placeholder={t("palette.search")}
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setAt(0);
          }}
          onKeyDown={(e) => {
            const step = e.key === "ArrowDown" ? 1 : e.key === "ArrowUp" ? -1 : 0;
            if (step) {
              e.preventDefault();
              const n = shown.length;
              setAt((i) => (n ? (i + step + n) % n : 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              pick(shown[at]);
            }
          }}
          className="w-full h-12 pl-11 pr-4 bg-transparent text-sm outline-none placeholder:text-muted"
        />
      </div>
      <ul id={listId} role="listbox" aria-label={t("action.palette")} className="overflow-auto max-h-[50vh] p-1">
        {shown.map((i, n) => (
          <li
            key={i.key}
            id={`${listId}-${n}`}
            role="option"
            aria-selected={n === at}
            onMouseMove={() => n !== at && setAt(n)}
            onClick={() => pick(i)}
            className={
              "px-3 py-2 rounded-control flex items-center gap-2.5 text-sm cursor-pointer " + (n === at ? "bg-active" : "")
            }
          >
            {i.icon && <i.icon className="size-4 shrink-0 text-muted" aria-hidden />}
            <span className="truncate">{i.label}</span>
            <span className="text-xs text-muted truncate flex-1 min-w-0">{i.group}</span>
            {i.keys && <Kbd>{i.keys}</Kbd>}
          </li>
        ))}
        {shown.length === 0 && <li className="px-3 py-6 text-sm text-muted text-center">{t("palette.empty")}</li>}
      </ul>
    </Dialog>
  );
}
