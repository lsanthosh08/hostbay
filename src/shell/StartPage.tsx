import { useState } from "react";
import { keysFor, type AppCommands } from "../actions/registry";
import { t } from "../i18n";
import { recentHosts, useHosts } from "../state/hosts";
import { useSessions } from "../state/sessions";
import { Button } from "../ui/Button";
import { Input } from "../ui/Field";
import { AddIcon, ArchiveIcon, DockerIcon, ImportIcon, SearchIcon, SerialIcon, TerminalIcon } from "../ui/icons";
import { Kbd } from "../ui/Kbd";
import { StatusDot } from "../ui/StatusDot";
import { fuzzyRank } from "./fuzzy";
import { hostIcon } from "./kindIcon";

/** What shows when no session is open: quick connect, Recent, imports, and
 *  first steps for a new user. */
export function StartPage({ app }: { app: AppCommands }) {
  const { hosts } = useHosts();
  const { connect, statusByHost, openContainers } = useSessions();
  const [query, setQuery] = useState("");
  const matches = query.trim() ? fuzzyRank(hosts, query, (h) => `${h.alias} ${h.hostname}`).slice(0, 6) : [];
  const recent = recentHosts(hosts, 8);
  const firstRun = hosts.length === 0;

  const hostButton = (h: (typeof hosts)[number]) => {
    const Kind = hostIcon(h);
    return (
      <li key={h.id}>
        <button
          onClick={() => connect(h)}
          className="w-full text-left px-3 py-2 rounded-control flex items-center gap-2 text-sm hover:bg-hover"
        >
          <Kind className="size-4 shrink-0 text-muted" aria-hidden />
          <span className="font-medium truncate">{h.alias}</span>
          <span className="text-xs text-muted font-mono truncate">
            {h.user ? `${h.user}@` : ""}
            {h.hostname}
          </span>
          <span className="flex-1" />
          {statusByHost[h.id] && <StatusDot status={statusByHost[h.id]} />}
        </button>
      </li>
    );
  };

  return (
    <div className="flex-1 min-h-0 overflow-auto">
      <div className="max-w-2xl mx-auto px-6 py-12 flex flex-col gap-8">
        <header>
          <h1 className="text-2xl font-semibold">{t("app.name")}</h1>
          <p className="text-sm text-muted mt-1">
            {firstRun ? t("start.welcome") : t("start.tagline")}{" "}
            <Kbd>{keysFor("palette.open")}</Kbd> {t("start.paletteHint")}
          </p>
        </header>

        {firstRun ? (
          <section className="panel rounded-card p-5 flex flex-col gap-4">
            <h2 className="font-medium">{t("start.firstRun.title")}</h2>
            <ol className="text-sm text-muted list-decimal pl-5 flex flex-col gap-1.5">
              <li>{t("start.firstRun.add")}</li>
              <li>{t("start.firstRun.connect")}</li>
              <li>{t("start.firstRun.organize")}</li>
            </ol>
            <div className="flex flex-wrap gap-2">
              <Button variant="primary" icon={AddIcon} onClick={app.newHost}>
                {t("action.newHost")}
              </Button>
              <Button variant="ghost" icon={ImportIcon} onClick={app.importSshConfig}>
                {t("action.importSshConfig")}
              </Button>
              <Button variant="ghost" icon={ArchiveIcon} onClick={app.restore}>
                {t("action.restore")}
              </Button>
            </div>
          </section>
        ) : (
          <section className="flex flex-col gap-2">
            <h2 className="text-xs font-semibold uppercase tracking-wide text-muted">{t("start.quickConnect")}</h2>
            <div className="relative">
              <SearchIcon className="size-4 text-muted absolute left-3 top-1/2 -translate-y-1/2 pointer-events-none" aria-hidden />
              <Input
                autoFocus
                type="search"
                aria-label={t("start.quickConnect")}
                placeholder={t("start.quickConnectHint")}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && matches[0] && connect(matches[0])}
                className="w-full pl-9"
              />
            </div>
            {query.trim() &&
              (matches.length ? (
                <ul className="flex flex-col">{matches.map(hostButton)}</ul>
              ) : (
                <p className="text-sm text-muted px-3 py-2">{t("hosts.noMatch")}</p>
              ))}
          </section>
        )}

        {recent.length > 0 && !query.trim() && (
          <section className="flex flex-col gap-2">
            <h2 className="text-xs font-semibold uppercase tracking-wide text-muted">{t("sidebar.recent")}</h2>
            <ul className="flex flex-col">{recent.map(hostButton)}</ul>
          </section>
        )}

        <section className="flex flex-col gap-2">
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted">{t("start.more")}</h2>
          <div className="flex flex-wrap gap-2">
            {!firstRun && (
              <>
                <Button variant="ghost" size="sm" icon={AddIcon} onClick={app.newHost}>
                  {t("action.newHost")}
                </Button>
                <Button variant="ghost" size="sm" icon={ImportIcon} onClick={app.importSshConfig}>
                  {t("action.importSshConfig")}
                </Button>
              </>
            )}
            <Button variant="ghost" size="sm" icon={TerminalIcon} onClick={app.openLocalShell}>
              {t("action.localShell")}
            </Button>
            <Button variant="ghost" size="sm" icon={SerialIcon} onClick={app.openSerial}>
              {t("action.serialPort")}
            </Button>
            <Button variant="ghost" size="sm" icon={DockerIcon} onClick={openContainers}>
              {t("containers.open")}
            </Button>
          </div>
        </section>
      </div>
    </div>
  );
}
