import type { ReactNode } from "react";
import { RoutingView } from "../components/RoutingView";
import { SyncSettings } from "../components/SyncSettings";
import { t } from "../i18n";
import { SECTION_LABEL, SETTINGS_SECTIONS, type SettingsSection } from "../state/sessions";
import { focusRing } from "../ui/Button";
import { About } from "./settings/About";
import { AI } from "./settings/AI";
import { Appearance } from "./settings/Appearance";
import { Backup } from "./settings/Backup";
import { General } from "./settings/General";
import { Keyboard } from "./settings/Keyboard";
import { Keys } from "./settings/Keys";
import { Logging } from "./settings/Logging";
import { MCP } from "./settings/MCP";
import { Proxies } from "./settings/Proxies";
import { Vpns } from "./settings/Vpns";
import { Security } from "./settings/Security";
import { Skills } from "./settings/Skills";
import { Terminal } from "./settings/Terminal";

/** Each category's body. */
const BODY: Record<SettingsSection, () => ReactNode> = {
  general: () => <General />,
  appearance: () => <Appearance />,
  terminal: () => <Terminal />,
  keyboard: () => <Keyboard />,
  security: () => <Security />,
  keys: () => <Keys />,
  proxies: () => <Proxies />,
  vpns: () => (
    <>
      <Vpns />
      <RoutingView />
    </>
  ),
  ai: () => <AI />,
  sync: () => <SyncSettings />,
  backup: () => <Backup />,
  about: () => <About />,
  logging: () => <Logging />,
  skills: () => <Skills />,
  mcp: () => <MCP />,
};

/** The Settings tab: every setting, by category (R74). */
export function SettingsView({ section, onSection }: { section: SettingsSection; onSection: (s: SettingsSection) => void }) {
  return (
    <div className="flex-1 min-h-0 flex">
      <nav aria-label={t("settings.title")} className="w-52 shrink-0 border-r border-line p-3 flex flex-col gap-0.5 overflow-auto">
        <h1 className="px-2.5 pb-2 text-base font-semibold">{t("settings.title")}</h1>
        {SETTINGS_SECTIONS.map((s) => (
          <button
            key={s}
            type="button"
            aria-current={s === section ? "page" : undefined}
            onClick={() => onSection(s)}
            className={
              `text-left px-2.5 py-1.5 rounded-control text-sm transition-colors ${focusRing} ` +
              (s === section ? "bg-active text-fg font-medium" : "text-muted hover:text-fg hover:bg-hover")
            }
          >
            {t(SECTION_LABEL[s])}
          </button>
        ))}
      </nav>
      <div className="flex-1 min-w-0 overflow-auto">
        <div className="max-w-3xl px-8 py-6 flex flex-col gap-8">
          <h2 className="text-lg font-semibold">{t(SECTION_LABEL[section])}</h2>
          {/* Keyed, so a category's unsaved fields do not leak into the next. */}
          <div key={section} className="flex flex-col gap-8">
            {BODY[section]()}
          </div>
        </div>
      </div>
    </div>
  );
}
